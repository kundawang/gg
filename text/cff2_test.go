package text

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"testing"
)

// cff2VStoreForTest builds an ItemVariationStore with 2 axes and 3 regions:
// R0 = wght positive, R1 = wght negative, R2 = wdth positive. A single
// ItemVariationData subtable (vsindex 0) references all three regions and
// carries no delta sets — CFF2 blend only needs the region list.
func cff2VStoreForTest() []byte {
	regions := []byte{}
	regions = binary.BigEndian.AppendUint16(regions, 2) // axisCount
	regions = binary.BigEndian.AppendUint16(regions, 3) // regionCount
	region := func(s0, p0, e0, s1, p1, e1 int16) {
		for _, v := range []int16{s0, p0, e0, s1, p1, e1} {
			regions = binary.BigEndian.AppendUint16(regions, uint16(v))
		}
	}
	region(0, 16384, 16384, 0, 0, 0)   // R0: wght+
	region(-16384, -16384, 0, 0, 0, 0) // R1: wght-
	region(0, 0, 0, 0, 16384, 16384)   // R2: wdth+

	ivd := []byte{0, 0, 0, 0, 0, 3, 0, 0, 0, 1, 0, 2} // itemCount=0, wordDeltaCount=0, 3 region indexes
	out := []byte{}
	out = binary.BigEndian.AppendUint16(out, 1)                       // format
	out = binary.BigEndian.AppendUint32(out, 12)                      // regionListOffset
	out = binary.BigEndian.AppendUint16(out, 1)                       // itemVariationDataCount
	out = binary.BigEndian.AppendUint32(out, uint32(12+len(regions))) // data offset
	out = append(out, regions...)
	out = append(out, ivd...)
	return out
}

// syntheticCFF2 builds a CFF2 table with 4 glyphs exercising blend, global
// and local subroutines, a compound (two-contour) path, and an empty glyph.
//
//	gid 0: plain box, no variation
//	gid 1: blended rectangle (W = 400 + 100·wght+ + 50·wght- + 200·wdth+)
//	       plus a 50×50 square drawn by a global subr
//	gid 2: empty charstring (space-like glyph)
//	gid 3: rectangle drawn via a Private local subr
func syntheticCFF2() []byte {
	return syntheticCFF2WithVStore(cff2VStoreForTest())
}

func syntheticCFF2WithVStore(vstore []byte) []byte {
	gsubr := t2prog(50, 0, 0, 50, -50, 0, 0, -50, byte(5), byte(11))
	g0 := t2prog(0, 0, byte(21), 500, 0, 0, 700, -500, 0, 0, -700, byte(5))
	g1 := t2prog(
		100, 200, byte(21), // rmoveto (100,200)
		400, 100, 50, 200, 1, byte(16), 0, byte(5), // W blend; rlineto (W,0)
		0, 300, byte(5), // rlineto (0,300)
		-400, -100, -50, -200, 1, byte(16), 0, byte(5), // -W blend; rlineto (-W,0)
		0, -300, byte(5), // rlineto (0,-300)
		100, 400, byte(21), // rmoveto to (200,600)
		-107, byte(29), // callgsubr 0: 50×50 square
	)
	g2 := []byte{}                                 // empty glyph
	g3 := t2prog(0, 100, byte(21), -107, byte(10)) // rmoveto; callsubr 0
	lsubr := t2prog(200, 0, 0, 200, -200, 0, 0, -200, byte(5), byte(11))

	global := cffIndexForTest([][]byte{gsubr}, 1)
	chars := cffIndexForTest([][]byte{g0, g1, g2, g3}, 1)
	// Private DICT: blended BlueValues (validated then discarded), Subrs at
	// +15 (immediately after the DICT), explicit vsindex 0.
	private := []byte{
		139, 149, 159, 169, 139, 149, 159, 169, 141, 23, 6, // 0 10 20 30 0 10 20 30 2 blend; BlueValues
		154, 19, // Subrs +15
		139, 22, // vsindex 0
	}
	localSubrs := cffIndexForTest([][]byte{lsubr}, 1)

	const (
		headerLen = 5
		topLen    = 6 + 6 + 7 // CharStrings + vstore + FDArray, all long-encoded
		fdObjLen  = 5 + 5 + 1 // Private size + offset (long) + operator
	)
	fdArrayLen := 3 + 2 + fdObjLen // INDEX header + 2 offsets + 1 object
	charOff := headerLen + topLen + len(global)
	vstoreOff := charOff + len(chars)
	fdArrayOff := vstoreOff + len(vstore)
	privOff := fdArrayOff + fdArrayLen

	top := append([]byte{}, cffLong(charOff)...)
	top = append(top, 17)
	top = append(top, cffLong(vstoreOff)...)
	top = append(top, 24)
	top = append(top, cffLong(fdArrayOff)...)
	top = append(top, 12, 36)

	fdObj := append([]byte{}, cffLong(len(private))...)
	fdObj = append(fdObj, cffLong(privOff)...)
	fdObj = append(fdObj, 18)
	fdArray := cffIndexForTest([][]byte{fdObj}, 1)

	b := []byte{2, 0, headerLen, byte(topLen >> 8), byte(topLen)}
	b = append(b, top...)
	b = append(b, global...)
	b = append(b, chars...)
	b = append(b, vstore...)
	b = append(b, fdArray...)
	b = append(b, private...)
	b = append(b, localSubrs...)
	return b
}

// cff2FvarForTest builds an fvar table with wght (100/400/900) and
// wdth (50/100/200) axes.
func cff2FvarForTest() []byte {
	out := make([]byte, 16+2*20)
	binary.BigEndian.PutUint16(out[0:2], 1)  // majorVersion
	binary.BigEndian.PutUint16(out[4:6], 16) // axisArrayOffset
	binary.BigEndian.PutUint16(out[8:10], 2) // axisCount
	binary.BigEndian.PutUint16(out[10:12], 20)
	axis := func(i int, tag string, minV, defV, maxV int32) {
		off := 16 + i*20
		copy(out[off:off+4], tag)
		binary.BigEndian.PutUint32(out[off+4:], uint32(minV<<16))
		binary.BigEndian.PutUint32(out[off+8:], uint32(defV<<16))
		binary.BigEndian.PutUint32(out[off+12:], uint32(maxV<<16))
	}
	axis(0, "wght", 100, 400, 900)
	axis(1, "wdth", 50, 100, 200)
	return out
}

// syntheticCFF2OTF wraps syntheticCFF2 in a minimal sfnt with fvar, hmtx,
// and cmap (space→2, A→1, B→3).
func syntheticCFF2OTF() []byte {
	tables := map[string][]byte{}
	head := make([]byte, 54)
	binary.BigEndian.PutUint16(head[18:20], 1000)
	tables["head"] = head
	maxp := make([]byte, 6)
	binary.BigEndian.PutUint32(maxp[:4], 0x00005000)
	binary.BigEndian.PutUint16(maxp[4:], 4)
	tables["maxp"] = maxp
	hhea := make([]byte, 36)
	binary.BigEndian.PutUint16(hhea[34:], 4)
	tables["hhea"] = hhea
	hmtx := make([]byte, 16)
	for i, m := range [][2]int{{600, 0}, {800, 100}, {250, 0}, {300, 0}} {
		binary.BigEndian.PutUint16(hmtx[i*4:], uint16(m[0]))
		binary.BigEndian.PutUint16(hmtx[i*4+2:], int16bits(int16(m[1])))
	}
	tables["hmtx"] = hmtx
	cmap := make([]byte, 12+16+3*12)
	binary.BigEndian.PutUint16(cmap[2:4], 1)
	binary.BigEndian.PutUint16(cmap[4:6], 3)
	binary.BigEndian.PutUint16(cmap[6:8], 10)
	binary.BigEndian.PutUint32(cmap[8:12], 12)
	binary.BigEndian.PutUint16(cmap[12:14], 12)
	binary.BigEndian.PutUint32(cmap[16:20], 16+3*12)
	binary.BigEndian.PutUint32(cmap[24:28], 3)
	for i, g := range [][3]uint32{{32, 32, 2}, {65, 65, 1}, {66, 66, 3}} {
		binary.BigEndian.PutUint32(cmap[28+i*12:], g[0])
		binary.BigEndian.PutUint32(cmap[32+i*12:], g[1])
		binary.BigEndian.PutUint32(cmap[36+i*12:], g[2])
	}
	tables["cmap"] = cmap
	tables["fvar"] = cff2FvarForTest()
	tables["CFF2"] = syntheticCFF2()

	tags := []string{"CFF2", "cmap", "fvar", "head", "hhea", "hmtx", "maxp"}
	header := 12 + 16*len(tags)
	total := header
	for _, tag := range tags {
		total = (total + 3) &^ 3
		total += len(tables[tag])
	}
	out := make([]byte, total)
	copy(out[:4], "OTTO")
	binary.BigEndian.PutUint16(out[4:6], uint16(len(tags)))
	off := header
	for i, tag := range tags {
		off = (off + 3) &^ 3
		rec := 12 + i*16
		copy(out[rec:rec+4], tag)
		binary.BigEndian.PutUint32(out[rec+8:rec+12], uint32(off))
		binary.BigEndian.PutUint32(out[rec+12:rec+16], uint32(len(tables[tag])))
		copy(out[off:], tables[tag])
		off += len(tables[tag])
	}
	return out
}

func parseSyntheticCFF2(t *testing.T) *cff2Font {
	t.Helper()
	f, err := parseCFF2(syntheticCFF2())
	if err != nil {
		t.Fatalf("parseCFF2: %v", err)
	}
	return f
}

// cff2G1WidthX decodes gid 1 with the given normalized coords and returns
// the X of the first rlineto endpoint (= 100 + blended W).
func cff2G1WidthX(t *testing.T, f *cff2Font, coords []int16) float32 {
	t.Helper()
	cs, global, private, err := f.glyph(1)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := decodeCFF2Charstring(cs, private.localSubrs, global, f.blendContext(coords))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(segments) != 10 {
		t.Fatalf("segments=%d, want 10", len(segments))
	}
	return segments[1].Points[0].X
}

func TestCFF2ParseStructure(t *testing.T) {
	f := parseSyntheticCFF2(t)
	if len(f.charStrings) != 4 || len(f.fds) != 1 || len(f.globalSubrs) != 1 {
		t.Fatalf("charStrings=%d fds=%d globalSubrs=%d", len(f.charStrings), len(f.fds), len(f.globalSubrs))
	}
	if f.vstore == nil || len(f.vstore.regions) != 3 || len(f.vstore.itemData) != 1 {
		t.Fatalf("vstore=%+v", f.vstore)
	}
	if n, err := f.numRegions(0); err != nil || n != 3 {
		t.Fatalf("numRegions(0)=%d, %v", n, err)
	}
	if _, err := f.numRegions(1); err == nil {
		t.Fatal("numRegions(1) accepted out-of-range vsindex")
	}
	if _, _, _, err := f.glyph(9); err == nil {
		t.Fatal("glyph(9) accepted out-of-range glyph ID")
	}
	if _, _, private, err := f.glyph(3); err != nil || len(private.localSubrs) != 1 {
		t.Fatalf("glyph(3) private=%+v, %v", private, err)
	}

	if _, err := parseCFF2([]byte{1, 0, 5, 0, 0}); err == nil {
		t.Fatal("CFF1 major version accepted by parseCFF2")
	}
	for n := 0; n < len(syntheticCFF2()); n++ {
		if _, err := parseCFF2(syntheticCFF2()[:n]); err == nil {
			t.Fatalf("truncated CFF2 (%d bytes) parsed without error", n)
		}
	}
	// Bad Top DICT: trailing operand without operator.
	badTop := []byte{2, 0, 5, 0, 1, 139, 0, 0}
	if _, err := parseCFF2(badTop); err == nil {
		t.Fatal("Top DICT with trailing operands accepted")
	}
	// Bad Private DICT range.
	if _, err := parseCFF2Private([]byte{1, 2}, 3, 0, nil); err == nil {
		t.Fatal("out-of-range Private DICT accepted")
	}
	// Private DICT blend/vsindex validation.
	if _, _, err := parseCFF2PrivateDict([]byte{139, 22}, nil); err == nil {
		t.Fatal("vsindex without VariationStore accepted")
	}
	f = parseSyntheticCFF2(t)
	if _, _, err := parseCFF2PrivateDict([]byte{140, 22}, f.vstore); err == nil {
		t.Fatal("out-of-range vsindex accepted")
	}
	if _, _, err := parseCFF2PrivateDict([]byte{139, 141, 23}, f.vstore); err == nil {
		t.Fatal("blend operand underflow accepted")
	}
	// VariationStore region index out of range.
	badVstore := cff2VStoreForTest()
	badVstore[len(badVstore)-1] = 9 // last region index → 9 of 3 regions
	if _, err := parseCFF2(syntheticCFF2WithVStore(badVstore)); err == nil {
		t.Fatal("VariationStore with out-of-range region index accepted")
	}
}

func TestCFF2BlendDecodes(t *testing.T) {
	f := parseSyntheticCFF2(t)
	for name, tc := range map[string]struct {
		coords []int16
		wantX  float32
	}{
		"default":      {nil, 500},
		"wght max":     {[]int16{16384, 0}, 600},
		"wght min":     {[]int16{-16384, 0}, 550},
		"wdth max":     {[]int16{0, 16384}, 700},
		"both max":     {[]int16{16384, 16384}, 800},
		"wght halfway": {[]int16{8192, 0}, 550},
	} {
		t.Run(name, func(t *testing.T) {
			if got := cff2G1WidthX(t, f, tc.coords); got != tc.wantX {
				t.Errorf("first line X=%v, want %v", got, tc.wantX)
			}
		})
	}
}

func TestCFF2OutlineExtractionAndVariations(t *testing.T) {
	parsed, err := getParser(defaultParserName).Parse(syntheticCFF2OTF())
	if err != nil {
		t.Fatal(err)
	}
	for r, want := range map[rune]uint16{'A': 1, ' ': 2, 'B': 3} {
		if gid := parsed.GlyphIndex(r); gid != want {
			t.Fatalf("GlyphIndex(%q)=%d, want %d", r, gid, want)
		}
	}
	e := NewOutlineExtractor()
	wght := [4]byte{'w', 'g', 'h', 't'}
	wdth := [4]byte{'w', 'd', 't', 'h'}

	const size = 20.0
	scale := size / 1000
	closeRect := func(got Rect, want Rect) bool {
		return math.Abs(got.MinX-want.MinX) < 1e-4 && math.Abs(got.MinY-want.MinY) < 1e-4 &&
			math.Abs(got.MaxX-want.MaxX) < 1e-4 && math.Abs(got.MaxY-want.MaxY) < 1e-4
	}
	wantBounds := func(maxXFontUnits float64) Rect {
		return Rect{MinX: 100 * scale, MinY: -650 * scale, MaxX: maxXFontUnits * scale, MaxY: -200 * scale}
	}

	// Static path: default instance.
	o, err := e.ExtractOutline(parsed, 1, size)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Segments) != 10 || o.Advance != float32(800*scale) || o.LSB != float32(100*scale) {
		t.Fatalf("default outline=%#v", o)
	}
	if !closeRect(o.Bounds, wantBounds(500)) {
		t.Errorf("default bounds=%v, want %v", o.Bounds, wantBounds(500))
	}
	if got := parsed.GlyphBounds(1, size); got != o.Bounds {
		t.Errorf("GlyphBounds=%v outline=%v", got, o.Bounds)
	}

	// Variation path: multiple axes and combinations.
	for name, tc := range map[string]struct {
		vars  []FontVariation
		wantX float64
	}{
		"wght 900":       {[]FontVariation{{Tag: wght, Value: 900}}, 600},
		"wght 100":       {[]FontVariation{{Tag: wght, Value: 100}}, 550},
		"wdth 200":       {[]FontVariation{{Tag: wdth, Value: 200}}, 700},
		"wght+wdth max":  {[]FontVariation{{Tag: wght, Value: 900}, {Tag: wdth, Value: 200}}, 800},
		"default values": {[]FontVariation{{Tag: wght, Value: 400}, {Tag: wdth, Value: 100}}, 500},
	} {
		t.Run(name, func(t *testing.T) {
			ov, err := e.ExtractOutlineHintedVar(parsed, 1, size, HintingNone, tc.vars)
			if err != nil {
				t.Fatal(err)
			}
			if len(ov.Segments) != 10 {
				t.Fatalf("segments=%d, want 10", len(ov.Segments))
			}
			if !closeRect(ov.Bounds, wantBounds(tc.wantX)) {
				t.Errorf("bounds=%v, want %v", ov.Bounds, wantBounds(tc.wantX))
			}
		})
	}

	// Empty glyph: outline carries only the advance.
	empty, err := e.ExtractOutline(parsed, 2, size)
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || !empty.IsEmpty() || empty.Advance != float32(250*scale) {
		t.Fatalf("empty glyph outline=%#v", empty)
	}
	if got := parsed.GlyphBounds(2, size); got != (Rect{}) {
		t.Errorf("empty glyph bounds=%v, want zero", got)
	}

	// Local subr glyph.
	local, err := e.ExtractOutline(parsed, 3, size)
	if err != nil {
		t.Fatal(err)
	}
	if len(local.Segments) != 5 || !closeRect(local.Bounds, Rect{MinX: 0, MinY: -300 * scale, MaxX: 200 * scale, MaxY: -100 * scale}) {
		t.Fatalf("local subr outline=%#v bounds=%v", local, local.Bounds)
	}

	// Hinted extraction and rasterization stay on the shared pipeline.
	if _, err = e.ExtractOutlineHinted(parsed, 1, size, HintingVertical); err != nil {
		t.Fatal(err)
	}
	if _, err = e.ExtractOutlineHintedVar(parsed, 1, size, HintingFull, []FontVariation{{Tag: wght, Value: 900}}); err != nil {
		t.Fatal(err)
	}
	rasterized := RasterizeGlyph(parsed, 1, 32)
	if rasterized == nil || rasterized.Mask == nil {
		t.Fatal("RasterizeGlyph returned no CFF2 mask")
	}
	nonEmpty := false
	for _, alpha := range rasterized.Mask.Pix {
		if alpha != 0 {
			nonEmpty = true
			break
		}
	}
	if !nonEmpty {
		t.Fatal("RasterizeGlyph returned an empty CFF2 mask")
	}

	// Face-level contract: same measurement path as glyf/CFF1, and missing
	// runes report no glyph (the emoji-fallback trigger).
	source, err := NewFontSource(syntheticCFF2OTF())
	if err != nil {
		t.Fatal(err)
	}
	face := source.Face(size, WithVariations(FontVariation{Tag: wght, Value: 900}))
	if !face.HasGlyph('A') || face.HasGlyph('€') {
		t.Fatalf("HasGlyph(A)=%v HasGlyph(€)=%v", face.HasGlyph('A'), face.HasGlyph('€'))
	}
	if got := face.Advance("A"); math.Abs(got-800*scale) > 1e-9 {
		t.Errorf("Face.Advance(A)=%v, want %v", got, 800*scale)
	}
	count := 0
	for g := range face.Glyphs("A B") {
		switch count {
		case 0:
			if g.GID != 1 || g.Bounds != parsed.GlyphBounds(1, size) {
				t.Errorf("glyph 0 = %#v", g)
			}
		case 1:
			if g.GID != 2 || g.Bounds != (Rect{}) {
				t.Errorf("glyph 1 = %#v", g)
			}
		case 2:
			if g.GID != 3 {
				t.Errorf("glyph 2 GID=%d", g.GID)
			}
		}
		count++
	}
	if count != 3 {
		t.Fatalf("Glyphs yielded %d glyphs, want 3", count)
	}

	// Repeated requests must not share mutable output: mutating one result
	// must not affect a later extraction of the same glyph.
	first, err := e.ExtractOutlineHintedVar(parsed, 1, size, HintingNone, []FontVariation{{Tag: wght, Value: 900}})
	if err != nil {
		t.Fatal(err)
	}
	first.Segments[0].Points[0].X = -9999
	second, err := e.ExtractOutlineHintedVar(parsed, 1, size, HintingNone, []FontVariation{{Tag: wght, Value: 900}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Segments[0].Points[0].X == -9999 {
		t.Fatal("extractions share mutable segment storage")
	}

	// Concurrent extraction with mixed variations (run under -race).
	const goroutines = 16
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vars := []FontVariation{{Tag: wght, Value: 100 + float32(i*50)}}
			oc, err := NewOutlineExtractor().ExtractOutlineHintedVar(parsed, 1, 18, HintingNone, vars)
			if err != nil {
				errs <- err
			} else if len(oc.Segments) != 10 {
				errs <- fmt.Errorf("got %d segments", len(oc.Segments))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestCFF2ScalingAndBounds(t *testing.T) {
	parsed, err := getParser(defaultParserName).Parse(syntheticCFF2OTF())
	if err != nil {
		t.Fatal(err)
	}
	e := NewOutlineExtractor()
	wght := [4]byte{'w', 'g', 'h', 't'}
	for _, size := range []float64{10, 20, 33.5} {
		scale := size / 1000
		o, err := e.ExtractOutlineHintedVar(parsed, 1, size, HintingNone, []FontVariation{{Tag: wght, Value: 900}})
		if err != nil {
			t.Fatal(err)
		}
		want := Rect{MinX: 100 * scale, MinY: -650 * scale, MaxX: 600 * scale, MaxY: -200 * scale}
		if math.Abs(o.Bounds.MinX-want.MinX) > 1e-4 || math.Abs(o.Bounds.MinY-want.MinY) > 1e-4 ||
			math.Abs(o.Bounds.MaxX-want.MaxX) > 1e-4 || math.Abs(o.Bounds.MaxY-want.MaxY) > 1e-4 {
			t.Errorf("size %v bounds=%v, want %v", size, o.Bounds, want)
		}
		if o.Advance != float32(800*scale) || o.LSB != float32(100*scale) {
			t.Errorf("size %v advance=%v lsb=%v", size, o.Advance, o.LSB)
		}
		if got := parsed.GlyphAdvance(1, size); got != 800*scale {
			t.Errorf("size %v GlyphAdvance=%v, want %v", size, got, 800*scale)
		}
	}
}

func TestCFF2ErrorBoundaries(t *testing.T) {
	f := parseSyntheticCFF2(t)
	blend := f.blendContext(nil)

	for name, tc := range map[string]struct {
		cs      []byte
		local   [][]byte
		global  [][]byte
		blend   *cff2BlendContext
		wantErr string
	}{
		"recursive global subr":  {t2prog(-107, byte(29)), nil, [][]byte{t2prog(-107, byte(29))}, nil, "cycle"},
		"subr without return":    {t2prog(-107, byte(29)), nil, [][]byte{t2prog(1, 2, byte(5))}, nil, "without return"},
		"return outside subr":    {t2prog(byte(11)), nil, nil, nil, "return outside subroutine"},
		"subr out of range":      {t2prog(5000, byte(29)), nil, [][]byte{{byte(11)}}, nil, "out of range"},
		"blend count zero":       {t2prog(1, 0, byte(16)), nil, nil, blend, "not positive"},
		"blend underflow":        {t2prog(5, 1, byte(16)), nil, nil, blend, "wants"},
		"blend non-integer":      {append([]byte{255, 0, 1, 0x80, 0}, byte(16)), nil, nil, blend, "not an integer"},
		"blend without vstore":   {t2prog(1, 1, byte(16)), nil, nil, nil, "without VariationStore"},
		"vsindex out of range":   {t2prog(5, byte(15)), nil, nil, blend, "out of range"},
		"vsindex without vstore": {t2prog(0, byte(15)), nil, nil, nil, "without VariationStore"},
		"reserved operator":      {t2prog(byte(14)), nil, nil, nil, "reserved"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeCFF2Charstring(tc.cs, tc.local, tc.global, tc.blend)
			if err == nil {
				t.Fatalf("expected %q error", tc.wantErr)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(tc.wantErr)) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}

	// Oversized point count: 257 hlineto ops × 256 segments each exceeds the
	// 65536 segment budget without tripping the operation limit.
	row := append(bytes.Repeat([]byte{140}, 256), byte(6))
	huge := bytes.Repeat(row, 257)
	if _, err := decodeCFF2Charstring(huge, nil, nil, nil); err == nil {
		t.Fatal("segment limit not enforced")
	}

	// Stack overflow: 514 operands exceed the CFF2 maxstack of 513.
	if _, err := decodeCFF2Charstring(bytes.Repeat([]byte{140}, 514), nil, nil, nil); err == nil {
		t.Fatal("operand stack limit not enforced")
	}

	// Missing glyph through the public extractor carries glyph context.
	parsed, err := getParser(defaultParserName).Parse(syntheticCFF2OTF())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewOutlineExtractor().ExtractOutline(parsed, 9, 16); err == nil {
		t.Fatal("out-of-range glyph accepted")
	}

	// CharStrings/maxp glyph-count mismatch is rejected deterministically.
	mismatch := &ownParsedFont{numGlyphs: 3, tables: map[string][]byte{"CFF2": syntheticCFF2()}}
	if _, err := mismatch.loadCFF2(); err == nil {
		t.Fatal("CharStrings/maxp glyph-count mismatch accepted")
	}

	// Malformed CFF2 table propagates a contextual error, not a panic.
	bad := &ownParsedFont{upem: 1000, numGlyphs: 4, tables: map[string][]byte{"CFF2": {2}}}
	if _, err := NewOutlineExtractor().ExtractOutline(bad, 1, 16); err == nil {
		t.Fatal("malformed CFF2 table accepted")
	}
	if bounds := bad.cffGlyphBounds(1, 16); bounds != (Rect{}) {
		t.Fatalf("malformed CFF2 bounds=%v, want empty", bounds)
	}
}
