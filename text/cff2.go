package text

import (
	"encoding/binary"
	"fmt"
	"math"
)

// CFF2 is the OpenType variable-outline successor of CFF1: a compact
// charstring format whose operands may be interpolated across variation
// regions via the blend operator. This file parses the CFF2 table
// (header, Top DICT, CharStrings, FDArray/FDSelect, Private DICTs with
// Subrs, and the VariationStore) and executes CFF2 charstrings against
// the current variation coordinates.
//
// References:
//   - OpenType spec, CFF2 chapter (table layout, DICT operators)
//   - Adobe CFF2 charstring spec (vsindex/blend semantics)
//   - FreeType cffgload.c / cffparse.c (blend evaluation order)

// cff2MaxStack is the CFF2 charstring operand stack limit (CFF2 spec:
// maxstack 513, vs 48 for CFF1 Type 2). blend needs n*(numRegions+1)+1
// operands, which routinely exceeds the CFF1 limit.
const cff2MaxStack = 513

// cff2Private holds the per-FD data a CFF2 glyph needs at decode time.
type cff2Private struct {
	localSubrs [][]byte
	vsindex    int // selected ItemVariationData subtable (default 0)
}

// cff2Font contains only immutable data required to execute CFF2 glyphs.
// Variation-dependent output is computed per request and never stored here,
// so concurrent and repeated glyph requests never share mutable state.
type cff2Font struct {
	charStrings [][]byte
	globalSubrs [][]byte
	fds         []cff2Private
	fdSelect    []uint16 // nil when a single FD covers all glyphs
	vstore      *itemVariationStore
}

// parseCFF2 parses a CFF2 table.
//
// Layout (OpenType spec, CFF2):
//
//	Header: uint8 major(=2), uint8 minor, uint8 headerSize, uint16 topDictLength
//	Top DICT data (topDictLength bytes, immediately after the header)
//	Global Subr INDEX (immediately after the Top DICT)
//	everything else by absolute offset: CharStrings (17), vstore (24),
//	FDArray (12 36), FDSelect (12 37)
//
//nolint:gocognit,gocyclo,cyclop,funlen // CFF2 parsing is a linear, bounded validation pipeline mirroring parseCFF1; splitting it would obscure offset ownership.
func parseCFF2(data []byte) (*cff2Font, error) {
	fail := func(format string, args ...any) (*cff2Font, error) {
		return nil, fmt.Errorf("text: CFF2: "+format, args...)
	}
	if len(data) < 5 {
		return fail("truncated header")
	}
	if data[0] != 2 {
		return fail("unsupported major version %d", data[0])
	}
	hdrSize := int(data[2])
	if hdrSize < 5 || hdrSize > len(data) {
		return fail("invalid header size %d", hdrSize)
	}
	topLen := int(binary.BigEndian.Uint16(data[3:5]))
	if topLen > len(data)-hdrSize {
		return fail("Top DICT length %d exceeds table", topLen)
	}
	top, err := parseCFFDict(data[hdrSize : hdrSize+topLen])
	if err != nil {
		return fail("Top DICT: %v", err)
	}
	global, _, err := parseCFFIndex(data, hdrSize+topLen, "Global Subrs INDEX")
	if err != nil {
		return fail("%v", err)
	}

	f := &cff2Font{globalSubrs: global}

	if off, ok := dictOptionalInt(top, 24); ok {
		if off >= len(data) {
			return fail("vstore offset %d outside table", off)
		}
		f.vstore, err = parseItemVariationStore(data[off:])
		if err != nil {
			return fail("VariationStore: %v", err)
		}
		for i := range f.vstore.itemData {
			for _, ri := range f.vstore.itemData[i].regionIndices {
				if int(ri) >= len(f.vstore.regions) {
					return fail("VariationStore data %d references region %d of %d", i, ri, len(f.vstore.regions))
				}
			}
		}
	}

	charOff, err := dictOneInt(top, 17, true)
	if err != nil {
		return fail("CharStrings: %v", err)
	}
	f.charStrings, _, err = parseCFFIndex(data, charOff, "CharStrings INDEX")
	if err != nil {
		return fail("%v", err)
	}
	if len(f.charStrings) == 0 {
		return fail("empty CharStrings INDEX")
	}

	fdArrayOff, err := dictOneInt(top, 1236, true)
	if err != nil {
		return fail("FDArray: %v", err)
	}
	fdDicts, _, err := parseCFFIndex(data, fdArrayOff, "FDArray INDEX")
	if err != nil {
		return fail("%v", err)
	}
	if len(fdDicts) == 0 {
		return fail("empty FDArray INDEX")
	}
	f.fds = make([]cff2Private, len(fdDicts))
	for i, raw := range fdDicts {
		d, e := parseCFFDict(raw)
		if e != nil {
			return fail("FDArray DICT %d: %v", i, e)
		}
		vals, ok := d[18]
		if !ok || len(vals) != 2 {
			return fail("FDArray DICT %d missing valid Private", i)
		}
		size, sizeErr := cffExactInt(vals[0])
		off, offErr := cffExactInt(vals[1])
		if sizeErr != nil || offErr != nil {
			return fail("FDArray DICT %d Private has non-integer size or offset", i)
		}
		f.fds[i], e = parseCFF2Private(data, size, off, f.vstore)
		if e != nil {
			return fail("FDArray DICT %d Private: %v", i, e)
		}
	}
	if len(f.fds) > 1 {
		fdSelectOff, e := dictOneInt(top, 1237, true)
		if e != nil {
			return fail("FDSelect: %v", e)
		}
		f.fdSelect, e = parseCFFFDSelect(data, fdSelectOff, len(f.charStrings), len(f.fds))
		if e != nil {
			return fail("FDSelect: %v", e)
		}
	} else if _, ok := top[1237]; ok {
		// A single FD covers every glyph; an FDSelect would be dead data, but
		// validate it anyway so malformed offsets are reported deterministically.
		fdSelectOff, e := dictOneInt(top, 1237, true)
		if e != nil {
			return fail("FDSelect: %v", e)
		}
		if _, e = parseCFFFDSelect(data, fdSelectOff, len(f.charStrings), len(f.fds)); e != nil {
			return fail("FDSelect: %v", e)
		}
	}
	return f, nil
}

// glyph returns the charstring, global subroutines, and private data for gid.
func (f *cff2Font) glyph(gid uint16) ([]byte, [][]byte, cff2Private, error) {
	if int(gid) >= len(f.charStrings) {
		return nil, nil, cff2Private{}, fmt.Errorf("text: CFF2: glyph ID %d out of range", gid)
	}
	fd := 0
	if len(f.fds) > 1 {
		if int(gid) >= len(f.fdSelect) {
			return nil, nil, cff2Private{}, fmt.Errorf("text: CFF2: missing FD selection for glyph %d", gid)
		}
		fd = int(f.fdSelect[gid])
		if fd < 0 || fd >= len(f.fds) {
			return nil, nil, cff2Private{}, fmt.Errorf("text: CFF2: FD %d out of range", fd)
		}
	}
	return f.charStrings[gid], f.globalSubrs, f.fds[fd], nil
}

// numRegions returns the blend region count for an ItemVariationData index.
func (f *cff2Font) numRegions(vsindex int) (int, error) {
	if f.vstore == nil {
		return 0, fmt.Errorf("text: CFF2: blend requires a VariationStore")
	}
	if vsindex < 0 || vsindex >= len(f.vstore.itemData) {
		return 0, fmt.Errorf("text: CFF2: vsindex %d out of range (%d variation data subtables)", vsindex, len(f.vstore.itemData))
	}
	return len(f.vstore.itemData[vsindex].regionIndices), nil
}

// blendContext carries the per-request variation state into charstring
// execution: the parsed VariationStore plus one interpolation scalar per
// variation region, computed from the current normalized coordinates.
type cff2BlendContext struct {
	vstore  *itemVariationStore
	scalars []float64 // one per vstore.regions entry, 16.16 fixed as float
}

// blendContext builds the execution-time blend state for coords (F2.14
// normalized, one per fvar axis). A nil coords slice yields the default
// instance (all scalars zero for well-formed regions). Returns nil when the
// font has no VariationStore.
func (f *cff2Font) blendContext(coords []int16) *cff2BlendContext {
	if f.vstore == nil {
		return nil
	}
	scalars := make([]float64, len(f.vstore.regions))
	for i := range f.vstore.regions {
		scalars[i] = float64(f.vstore.regions[i].computeScalar(coords)) / 65536
	}
	return &cff2BlendContext{vstore: f.vstore, scalars: scalars}
}

// parseCFF2Private parses a CFF2 Private DICT and its local Subrs INDEX.
// Unlike CFF1, CFF2 Private DICTs may contain vsindex (22) and blend (23)
// operators; blend operands are validated and discarded because outline
// extraction does not consume blended hint parameters (BlueValues etc.).
func parseCFF2Private(data []byte, size, off int, vstore *itemVariationStore) (cff2Private, error) {
	var p cff2Private
	if size < 0 || off < 0 || off > len(data) || size > len(data)-off {
		return p, fmt.Errorf("range [%d,%d) outside table", off, off+size)
	}
	d, vsindex, err := parseCFF2PrivateDict(data[off:off+size], vstore)
	if err != nil {
		return p, err
	}
	p.vsindex = vsindex
	if rel, ok := dictOptionalInt(d, 19); ok {
		absolute := off + rel
		if absolute < off || absolute > len(data) {
			return p, fmt.Errorf("subrs offset outside table")
		}
		p.localSubrs, _, err = parseCFFIndex(data, absolute, "Local Subrs INDEX")
		if err != nil {
			return p, err
		}
	}
	return p, nil
}

// parseCFF2PrivateDict parses CFF2 Private DICT bytes, resolving the blend
// operator against the VariationStore. It returns the non-blend operators
// and the effective vsindex (the last one wins, per spec).
func parseCFF2PrivateDict(data []byte, vstore *itemVariationStore) (map[int][]float64, int, error) {
	out := make(map[int][]float64)
	operands := make([]float64, 0, 64)
	vsindex := 0
	numRegions := func() (int, error) {
		if vstore == nil {
			return 0, fmt.Errorf("blend requires a VariationStore")
		}
		if vsindex >= len(vstore.itemData) {
			return 0, fmt.Errorf("vsindex %d out of range (%d variation data subtables)", vsindex, len(vstore.itemData))
		}
		return len(vstore.itemData[vsindex].regionIndices), nil
	}
	for pos := 0; pos < len(data); {
		b := data[pos]
		if b >= 32 || b == 28 || b == 29 || b == 30 {
			v, next, err := readCFFDictNumber(data, pos)
			if err != nil {
				return nil, 0, err
			}
			if len(operands) >= cff2MaxStack {
				return nil, 0, fmt.Errorf("operand stack overflow")
			}
			operands = append(operands, v)
			pos = next
			continue
		}
		pos++
		op := int(b)
		if b == 12 {
			if pos >= len(data) {
				return nil, 0, fmt.Errorf("truncated escaped operator")
			}
			op = 1200 + int(data[pos])
			pos++
		}
		switch op {
		case 22: // vsindex
			if len(operands) != 1 {
				return nil, 0, fmt.Errorf("vsindex has %d operands", len(operands))
			}
			idx, err := cffExactInt(operands[0])
			if err != nil || idx < 0 {
				return nil, 0, fmt.Errorf("vsindex %v is not a valid index", operands[0])
			}
			if vstore == nil {
				return nil, 0, fmt.Errorf("vsindex without VariationStore")
			}
			if idx >= len(vstore.itemData) {
				return nil, 0, fmt.Errorf("vsindex %d out of range (%d variation data subtables)", idx, len(vstore.itemData))
			}
			vsindex = idx
			out[op] = []float64{operands[0]}
			operands = operands[:0]
		case 23: // blend
			if len(operands) < 1 {
				return nil, 0, fmt.Errorf("blend without operand count")
			}
			n, err := cffExactInt(operands[len(operands)-1])
			if err != nil || n < 1 {
				return nil, 0, fmt.Errorf("blend count %v is not a positive integer", operands[len(operands)-1])
			}
			nr, err := numRegions()
			if err != nil {
				return nil, 0, err
			}
			need := n*(nr+1) + 1
			if len(operands) < need {
				return nil, 0, fmt.Errorf("blend wants %d operands, have %d", need, len(operands))
			}
			// Blended hint parameters are validated but unused for outlines.
			operands = operands[:len(operands)-need]
		default:
			vals := append([]float64(nil), operands...)
			out[op] = vals
			operands = operands[:0]
		}
	}
	if len(operands) != 0 {
		return nil, 0, fmt.Errorf("trailing operands without operator")
	}
	return out, vsindex, nil
}

// decodeCFF2Charstring executes one CFF2 charstring in font units and
// returns the outline segments (Y-up, unscaled). Unlike CFF1 Type 2 there
// is no width and no endchar: the program ends at the end of the data, and
// the advance always comes from hmtx/HVAR. blend may be nil when the font
// has no VariationStore; encountering vsindex/blend then is an error.
//
//nolint:gocognit,gocyclo,cyclop,funlen,gocritic,maintidx,nestif,revive // One bounded interpreter keeps the shared CFF2 operand/call state explicit across operators.
func decodeCFF2Charstring(charstring []byte, localSubrs, globalSubrs [][]byte, blend *cff2BlendContext) ([]OutlineSegment, error) {
	var segments []OutlineSegment
	stack := make([]float64, 0, 64)
	transient := [32]float64{}
	frames := []type2Frame{{code: charstring, subr: -1}}
	activeLocal := make(map[int]bool)
	activeGlobal := make(map[int]bool)
	var x, y float64
	stems := 0
	operations := 0
	vsindex := 0

	fail := func(format string, args ...any) ([]OutlineSegment, error) {
		return nil, fmt.Errorf("text: CFF2 charstring: "+format, args...)
	}
	clear := func() { stack = stack[:0] }
	exactInt := func(v float64, label string) (int, error) {
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v > float64(math.MaxInt) || v < float64(math.MinInt) {
			return 0, fmt.Errorf("%s %v is not an integer", label, v)
		}
		return int(v), nil
	}
	line := func(nx, ny float64) error {
		if math.IsNaN(nx) || math.IsNaN(ny) || math.IsInf(nx, 0) || math.IsInf(ny, 0) || math.Abs(nx) > math.MaxFloat32 || math.Abs(ny) > math.MaxFloat32 {
			return fmt.Errorf("non-finite line coordinate")
		}
		if len(segments) >= type2MaxSegments {
			return fmt.Errorf("segment limit exceeded")
		}
		x, y = nx, ny
		segments = append(segments, OutlineSegment{Op: OutlineOpLineTo, Points: [3]OutlinePoint{{X: float32(x), Y: float32(y)}}})
		return nil
	}
	move := func(nx, ny float64) error {
		if math.IsNaN(nx) || math.IsNaN(ny) || math.IsInf(nx, 0) || math.IsInf(ny, 0) || math.Abs(nx) > math.MaxFloat32 || math.Abs(ny) > math.MaxFloat32 {
			return fmt.Errorf("non-finite move coordinate")
		}
		if len(segments) >= type2MaxSegments {
			return fmt.Errorf("segment limit exceeded")
		}
		x, y = nx, ny
		segments = append(segments, OutlineSegment{Op: OutlineOpMoveTo, Points: [3]OutlinePoint{{X: float32(x), Y: float32(y)}}})
		return nil
	}
	curve := func(dx1, dy1, dx2, dy2, dx3, dy3 float64) error {
		if len(segments) >= type2MaxSegments {
			return fmt.Errorf("segment limit exceeded")
		}
		p1x, p1y := x+dx1, y+dy1
		p2x, p2y := p1x+dx2, p1y+dy2
		nx, ny := p2x+dx3, p2y+dy3
		for _, v := range []float64{p1x, p1y, p2x, p2y, nx, ny} {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > math.MaxFloat32 {
				return fmt.Errorf("non-finite curve coordinate")
			}
		}
		p1 := OutlinePoint{X: float32(p1x), Y: float32(p1y)}
		p2 := OutlinePoint{X: float32(p2x), Y: float32(p2y)}
		x, y = nx, ny
		segments = append(segments, OutlineSegment{Op: OutlineOpCubicTo, Points: [3]OutlinePoint{p1, p2, {X: float32(x), Y: float32(y)}}})
		return nil
	}

	for len(frames) != 0 {
		fr := &frames[len(frames)-1]
		if fr.pc >= len(fr.code) {
			if fr.isSubr {
				return fail("subroutine %d ended without return", fr.subr)
			}
			// CFF2 has no endchar: the glyph program ends with its data.
			return segments, nil
		}
		b := fr.code[fr.pc]
		if b == 28 || b == 255 || b >= 32 {
			v, next, err := readType2Number(fr.code, fr.pc)
			if err != nil {
				return fail("at byte %d: %v", fr.pc, err)
			}
			if len(stack) >= cff2MaxStack {
				return fail("operand stack overflow")
			}
			stack = append(stack, v)
			fr.pc = next
			continue
		}
		fr.pc++
		operations++
		if operations > type2MaxOperations {
			return fail("operation limit exceeded")
		}
		op := int(b)
		if b == 12 {
			if fr.pc >= len(fr.code) {
				return fail("truncated escaped operator")
			}
			op = 1200 + int(fr.code[fr.pc])
			fr.pc++
		}

		need := func(n int) error {
			if len(stack) < n {
				return fmt.Errorf("operator %d: operand stack underflow", op)
			}
			return nil
		}
		binary := func(fn func(float64, float64) float64) error {
			if err := need(2); err != nil {
				return err
			}
			n := len(stack)
			stack[n-2] = fn(stack[n-2], stack[n-1])
			stack = stack[:n-1]
			return nil
		}
		var err error
		switch op {
		case 1, 3, 18, 23: // stem operators (no width argument in CFF2)
			if len(stack)%2 != 0 {
				err = fmt.Errorf("operator %d: odd stem operands", op)
			} else {
				stems += len(stack) / 2
				clear()
			}
		case 19, 20: // hintmask/cntrmask
			if len(stack)%2 != 0 {
				err = fmt.Errorf("operator %d: odd stem operands", op)
			} else {
				stems += len(stack) / 2
				clear()
				n := (stems + 7) / 8
				if fr.pc+n > len(fr.code) {
					err = fmt.Errorf("operator %d: truncated mask", op)
				} else {
					fr.pc += n
				}
			}
		case 4: // vmoveto
			if len(stack) != 1 {
				err = fmt.Errorf("vmoveto: want 1 operand")
			} else {
				err = move(x, y+stack[0])
				clear()
			}
		case 21: // rmoveto
			if len(stack) != 2 {
				err = fmt.Errorf("rmoveto: want 2 operands")
			} else {
				err = move(x+stack[0], y+stack[1])
				clear()
			}
		case 22: // hmoveto
			if len(stack) != 1 {
				err = fmt.Errorf("hmoveto: want 1 operand")
			} else {
				err = move(x+stack[0], y)
				clear()
			}
		case 5: // rlineto
			if len(stack) < 2 || len(stack)%2 != 0 {
				err = fmt.Errorf("rlineto: invalid operand count")
			} else {
				for i := 0; i < len(stack) && err == nil; i += 2 {
					err = line(x+stack[i], y+stack[i+1])
				}
				clear()
			}
		case 6, 7: // hlineto/vlineto
			if len(stack) < 1 {
				err = fmt.Errorf("operator %d: operand stack underflow", op)
			} else {
				horizontal := op == 6
				for _, d := range stack {
					if horizontal {
						err = line(x+d, y)
					} else {
						err = line(x, y+d)
					}
					if err != nil {
						break
					}
					horizontal = !horizontal
				}
				clear()
			}
		case 8: // rrcurveto
			if len(stack) < 6 || len(stack)%6 != 0 {
				err = fmt.Errorf("rrcurveto: invalid operand count")
			} else {
				for i := 0; i < len(stack) && err == nil; i += 6 {
					err = curve(stack[i], stack[i+1], stack[i+2], stack[i+3], stack[i+4], stack[i+5])
				}
				clear()
			}
		case 24: // rcurveline
			if len(stack) < 8 || (len(stack)-2)%6 != 0 {
				err = fmt.Errorf("rcurveline: invalid operand count")
			} else {
				i := 0
				for i < len(stack)-2 && err == nil {
					err = curve(stack[i], stack[i+1], stack[i+2], stack[i+3], stack[i+4], stack[i+5])
					i += 6
				}
				if err == nil {
					err = line(x+stack[i], y+stack[i+1])
				}
				clear()
			}
		case 25: // rlinecurve
			if len(stack) < 8 || (len(stack)-6)%2 != 0 {
				err = fmt.Errorf("rlinecurve: invalid operand count")
			} else {
				i := 0
				for i < len(stack)-6 && err == nil {
					err = line(x+stack[i], y+stack[i+1])
					i += 2
				}
				if err == nil {
					err = curve(stack[i], stack[i+1], stack[i+2], stack[i+3], stack[i+4], stack[i+5])
				}
				clear()
			}
		case 26, 27: // vvcurveto/hhcurveto
			if len(stack) < 4 {
				err = fmt.Errorf("operator %d: operand stack underflow", op)
			} else {
				i := 0
				extra := 0.0
				if len(stack)%4 == 1 {
					extra = stack[0]
					i = 1
				}
				if (len(stack)-i)%4 != 0 {
					err = fmt.Errorf("operator %d: invalid operand count", op)
				} else {
					for i < len(stack) && err == nil {
						if op == 26 {
							err = curve(extra, stack[i], stack[i+1], stack[i+2], 0, stack[i+3])
						} else {
							err = curve(stack[i], extra, stack[i+1], stack[i+2], stack[i+3], 0)
						}
						extra = 0
						i += 4
					}
				}
				clear()
			}
		case 30, 31: // vhcurveto/hvcurveto
			if len(stack) < 4 {
				err = fmt.Errorf("operator %d: operand stack underflow", op)
			} else {
				i := 0
				vertical := op == 30
				for i < len(stack) && err == nil {
					rem := len(stack) - i
					if rem < 4 {
						err = fmt.Errorf("operator %d: invalid operand count", op)
						break
					}
					extra := 0.0
					take := 4
					if rem == 5 {
						extra = stack[i+4]
						take = 5
					}
					if vertical {
						err = curve(0, stack[i], stack[i+1], stack[i+2], stack[i+3], extra)
					} else {
						err = curve(stack[i], 0, stack[i+1], stack[i+2], extra, stack[i+3])
					}
					i += take
					vertical = !vertical
				}
				clear()
			}
		case 10, 29: // callsubr/callgsubr
			if err = need(1); err == nil {
				n := len(stack)
				raw, intErr := exactInt(stack[n-1], "subroutine operand")
				stack = stack[:n-1]
				list := localSubrs
				global := false
				active := activeLocal
				if op == 29 {
					list = globalSubrs
					global = true
					active = activeGlobal
				}
				idx := raw + type2SubrBias(len(list))
				if intErr != nil {
					err = intErr
				} else if idx < 0 || idx >= len(list) {
					err = fmt.Errorf("operator %d: subroutine %d out of range", op, idx)
				} else if len(frames) >= type2MaxCallDepth {
					err = fmt.Errorf("subroutine recursion limit exceeded")
				} else if active[idx] {
					err = fmt.Errorf("subroutine cycle at %d", idx)
				} else {
					active[idx] = true
					frames = append(frames, type2Frame{code: list[idx], subr: idx, isGlobal: global, isSubr: true})
				}
			}
		case 11: // return
			if !fr.isSubr {
				err = fmt.Errorf("return outside subroutine")
			} else {
				if fr.isGlobal {
					delete(activeGlobal, fr.subr)
				} else {
					delete(activeLocal, fr.subr)
				}
				frames = frames[:len(frames)-1]
			}
		case 15: // vsindex
			if err = need(1); err == nil {
				idx, intErr := exactInt(stack[len(stack)-1], "vsindex operand")
				if intErr != nil {
					err = intErr
				} else if idx < 0 {
					err = fmt.Errorf("vsindex %d out of range", idx)
				} else if blend == nil {
					err = fmt.Errorf("vsindex without VariationStore")
				} else if idx >= len(blend.vstore.itemData) {
					err = fmt.Errorf("vsindex %d out of range (%d variation data subtables)", idx, len(blend.vstore.itemData))
				} else {
					vsindex = idx
				}
				clear()
			}
		case 16: // blend
			if blend == nil {
				err = fmt.Errorf("blend without VariationStore")
				break
			}
			if err = need(1); err == nil {
				n := len(stack)
				count, countErr := exactInt(stack[n-1], "blend count")
				if countErr != nil {
					err = countErr
					break
				}
				data := &blend.vstore.itemData[vsindex]
				numRegions := len(data.regionIndices)
				if count < 1 {
					err = fmt.Errorf("blend count %d is not positive", count)
				} else if want := count*(numRegions+1) + 1; len(stack) < want {
					err = fmt.Errorf("blend wants %d operands, have %d", want, len(stack))
				} else {
					base := len(stack) - (count*(numRegions+1) + 1)
					for i := 0; i < count; i++ {
						v := stack[base+i*(numRegions+1)]
						for j := 0; j < numRegions; j++ {
							v += stack[base+i*(numRegions+1)+1+j] * blend.scalars[data.regionIndices[j]]
						}
						stack[base+i] = v
					}
					stack = stack[:base+count]
				}
			}
		case 1200:
			clear() // dotsection, obsolete but harmless
		case 1203:
			err = binary(func(a, b float64) float64 {
				if a != 0 && b != 0 {
					return 1
				}
				return 0
			})
		case 1204:
			err = binary(func(a, b float64) float64 {
				if a != 0 || b != 0 {
					return 1
				}
				return 0
			})
		case 1205:
			if err = need(1); err == nil {
				if stack[len(stack)-1] == 0 {
					stack[len(stack)-1] = 1
				} else {
					stack[len(stack)-1] = 0
				}
			}
		case 1209:
			if err = need(1); err == nil {
				stack[len(stack)-1] = math.Abs(stack[len(stack)-1])
			}
		case 1210:
			err = binary(func(a, b float64) float64 { return a + b })
		case 1211:
			err = binary(func(a, b float64) float64 { return a - b })
		case 1212:
			if err = need(2); err == nil {
				if stack[len(stack)-1] == 0 {
					err = fmt.Errorf("division by zero")
				} else {
					err = binary(func(a, b float64) float64 { return a / b })
				}
			}
		case 1214:
			if err = need(1); err == nil {
				stack[len(stack)-1] = -stack[len(stack)-1]
			}
		case 1215:
			err = binary(func(a, b float64) float64 {
				if a == b {
					return 1
				}
				return 0
			})
		case 1218:
			if err = need(1); err == nil {
				stack = stack[:len(stack)-1]
			}
		case 1220:
			if err = need(2); err == nil {
				n := len(stack)
				i, intErr := exactInt(stack[n-1], "put index")
				v := stack[n-2]
				stack = stack[:n-2]
				if intErr != nil {
					err = intErr
				} else if i < 0 || i >= len(transient) {
					err = fmt.Errorf("put index %d out of range", i)
				} else {
					transient[i] = v
				}
			}
		case 1221:
			if err = need(1); err == nil {
				i, intErr := exactInt(stack[len(stack)-1], "get index")
				if intErr != nil {
					err = intErr
				} else if i < 0 || i >= len(transient) {
					err = fmt.Errorf("get index %d out of range", i)
				} else {
					stack[len(stack)-1] = transient[i]
				}
			}
		case 1222:
			if err = need(4); err == nil {
				n := len(stack)
				v1, v2, s1, s2 := stack[n-4], stack[n-3], stack[n-2], stack[n-1]
				stack = stack[:n-4]
				if s1 <= s2 {
					stack = append(stack, v1)
				} else {
					stack = append(stack, v2)
				}
			}
		case 1223:
			if len(stack) >= cff2MaxStack {
				err = fmt.Errorf("operand stack overflow")
			} else {
				stack = append(stack, 0.5)
			} // deterministic random
		case 1224:
			err = binary(func(a, b float64) float64 { return a * b })
		case 1226:
			if err = need(1); err == nil {
				if stack[len(stack)-1] < 0 {
					err = fmt.Errorf("sqrt of negative value")
				} else {
					stack[len(stack)-1] = math.Sqrt(stack[len(stack)-1])
				}
			}
		case 1227:
			if err = need(1); err == nil {
				if len(stack) >= cff2MaxStack {
					err = fmt.Errorf("operand stack overflow")
				} else {
					stack = append(stack, stack[len(stack)-1])
				}
			}
		case 1228:
			if err = need(2); err == nil {
				n := len(stack)
				stack[n-1], stack[n-2] = stack[n-2], stack[n-1]
			}
		case 1229:
			if err = need(1); err == nil {
				n := len(stack)
				i, intErr := exactInt(stack[n-1], "index operand")
				stack = stack[:n-1]
				if intErr != nil {
					err = intErr
				} else {
					if i < 0 {
						i = 0
					}
					if i >= len(stack) {
						i = len(stack) - 1
					}
					if i < 0 {
						err = fmt.Errorf("index on empty stack")
						break
					}
					stack = append(stack, stack[len(stack)-1-i])
				}
			}
		case 1230:
			if err = need(2); err == nil {
				n := len(stack)
				count, countErr := exactInt(stack[n-2], "roll count")
				j, jErr := exactInt(stack[n-1], "roll shift")
				stack = stack[:n-2]
				if countErr != nil {
					err = countErr
				} else if jErr != nil {
					err = jErr
				} else if count < 0 || count > len(stack) {
					err = fmt.Errorf("roll count %d out of range", count)
				} else if count > 0 {
					j %= count
					if j < 0 {
						j += count
					}
					base := len(stack) - count
					tmp := append([]float64(nil), stack[base:]...)
					for k := 0; k < count; k++ {
						stack[base+(k+j)%count] = tmp[k]
					}
				}
			}
		case 1234: // hflex
			if len(stack) != 7 {
				err = fmt.Errorf("hflex: want 7 operands")
			} else {
				err = curve(stack[0], 0, stack[1], stack[2], stack[3], 0)
				if err == nil {
					err = curve(stack[4], 0, stack[5], -stack[2], stack[6], 0)
				}
				clear()
			}
		case 1235: // flex
			if len(stack) != 13 {
				err = fmt.Errorf("flex: want 13 operands")
			} else {
				err = curve(stack[0], stack[1], stack[2], stack[3], stack[4], stack[5])
				if err == nil {
					err = curve(stack[6], stack[7], stack[8], stack[9], stack[10], stack[11])
				}
				clear()
			}
		case 1236: // hflex1
			if len(stack) != 9 {
				err = fmt.Errorf("hflex1: want 9 operands")
			} else {
				dy := stack[1] + stack[3] + stack[7]
				err = curve(stack[0], stack[1], stack[2], stack[3], stack[4], 0)
				if err == nil {
					err = curve(stack[5], 0, stack[6], stack[7], stack[8], -dy)
				}
				clear()
			}
		case 1237: // flex1
			if len(stack) != 11 {
				err = fmt.Errorf("flex1: want 11 operands")
			} else {
				dx := stack[0] + stack[2] + stack[4] + stack[6] + stack[8]
				dy := stack[1] + stack[3] + stack[5] + stack[7] + stack[9]
				d6x, d6y := 0.0, 0.0
				if math.Abs(dx) > math.Abs(dy) {
					d6x = stack[10]
					d6y = -dy
				} else {
					d6x = -dx
					d6y = stack[10]
				}
				err = curve(stack[0], stack[1], stack[2], stack[3], stack[4], stack[5])
				if err == nil {
					err = curve(stack[6], stack[7], stack[8], stack[9], d6x, d6y)
				}
				clear()
			}
		default:
			err = fmt.Errorf("reserved or unsupported operator %d", op)
		}
		if err != nil {
			return fail("at operator byte %d: %v", fr.pc-1, err)
		}
	}
	return segments, nil
}
