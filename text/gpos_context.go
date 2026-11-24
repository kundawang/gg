// GPOS contextual and chaining contextual positioning (Lookup Types 7 and 8).
//
// Implements OpenType GPOS:
//
//   - Type 7: Contextual Positioning, formats 1/2/3
//   - Type 8: Chaining Contextual Positioning, formats 1/2/3
//
// A contextual lookup itself never moves glyphs. It matches a sequence of
// glyphs (coverage, class, or coverage-array based) and then applies one or
// more referenced "nested" lookups at positions of the matched sequence via
// PosLookupRecords. Because every adjustment is accumulated onto the same
// per-glyph gposAdjustment array, the result composes with Type 1/2 lookups
// and any later feature: unmatched glyphs are neither consumed nor touched,
// and a glyph matched by several rules simply accumulates adjustments.
//
// Safety: all table access goes through bounds-checked readers. Malformed
// offsets, truncated tables and cycles/deep nesting terminate the lookup with
// a contextual error (collected on the engine) or a safe stop, never a panic.
// A per-shaping operation budget (gposMaxOps) bounds total work even on
// adversarial tables.
//
// Reference: https://learn.microsoft.com/en-us/typography/opentype/spec/gpos
package text

import "encoding/binary"

// GPOS contextual engine limits. These protect against adversarial or
// malformed fonts and mirror the kind of budgets HarfBuzz uses internally.
const (
	// gposMaxNestDepth bounds nested-lookup recursion (context → context).
	// Real-world fonts nest at most a couple of levels.
	gposMaxNestDepth = 8
	// gposMaxOps bounds the total number of matching/application operations
	// performed by one applyGPOS call.
	gposMaxOps = 64_000
)

// GPOS contextual error values. They are returned through gposEngine.err and
// are always wrapped with context by the caller; a non-nil error means the
// font table is malformed and positioning for that lookup was abandoned
// safely (partial adjustments already written are left in place).
var (
	errGPOSMalformed = newGPOSError("text: malformed GPOS contextual table")
	errGPOSDepth     = newGPOSError("text: GPOS nested lookup depth exceeded")
	errGPOSMaxOps    = newGPOSError("text: GPOS operation budget exceeded")
)

// GPOSError describes a failure while applying a contextual GPOS lookup.
// It carries the lookup type and a reason so callers and tests can
// distinguish malformed tables from safe-stop budget exhaustion.
type GPOSError struct {
	LookupType uint16
	Reason     string
}

func newGPOSError(reason string) *GPOSError {
	return &GPOSError{Reason: reason}
}

func (e *GPOSError) Error() string {
	if e.LookupType != 0 {
		return e.Reason + " (lookup type " + itoaUint16(e.LookupType) + ")"
	}
	return e.Reason
}

func itoaUint16(v uint16) string {
	if v == 0 {
		return "0"
	}
	var buf [5]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// gposEngine carries the mutable state for one applyGPOS invocation that
// contextual lookups need: the adjustment sink, an operation budget and the
// first safety error encountered (after which work stops).
type gposEngine struct {
	g      *gposTable
	glyphs []shapingGlyph
	adj    []gposAdjustment
	ops    int
	depth  int
	err    error
	// activeLookups detects lookup cycles (A→B→A) independently of depth.
	activeLookups map[uint16]bool
	// anchorOnly restricts rule matching to a single buffer position. It is
	// set when a contextual lookup is itself invoked from a PosLookupRecord
	// (nested contextual lookup), whose rule anchor is fixed by the parent.
	anchorOnly bool
	anchorPos  int
}

// anchorAllowed reports whether a rule may start at pos. In a normal
// top-level walk every position is a candidate; a nested contextual lookup
// only fires at the parent's anchor.
func (e *gposEngine) anchorAllowed(pos int) bool {
	return !e.anchorOnly || pos == e.anchorPos
}

// consumeStep charges one matching operation against the budget. Each
// candidate glyph position costs one step; once the budget is exhausted it
// records errGPOSMaxOps and returns false so callers unwind.
func (e *gposEngine) consumeStep() bool {
	if e.err != nil {
		return false
	}
	e.ops++
	if e.ops > gposMaxOps {
		e.err = errGPOSMaxOps
		return false
	}
	return true
}

// fail records the first contextual error.
func (e *gposEngine) fail(err error) {
	if e.err == nil {
		e.err = err
	}
}

// --- Bounds-checked readers ---
//
// Every contextual table access uses these helpers. A failed read returns
// (0, false) and the caller abandons the current (sub)table, so a bad offset
// in one rule can never panic or read neighboring memory.

// gposU16 reads a big-endian uint16 at off.
func gposU16(data []byte, off int) (uint16, bool) {
	if off < 0 || off+2 > len(data) {
		return 0, false
	}
	return binary.BigEndian.Uint16(data[off : off+2]), true
}

// gposU32 reads a big-endian uint32 at off.
func gposU32(data []byte, off int) (uint32, bool) {
	if off < 0 || off+4 > len(data) {
		return 0, false
	}
	return binary.BigEndian.Uint32(data[off : off+4]), true
}

// gposSubtable returns data[off:] after validating the offset.
func gposSubtable(data []byte, off int) ([]byte, bool) {
	if off <= 0 || off >= len(data) {
		return nil, false
	}
	return data[off:], true
}

// gposCoverage parses a coverage table reached via a uint16 offset.
func gposCoverage(data []byte, off int) (otCoverage, bool) {
	sub, ok := gposSubtable(data, off)
	if !ok {
		return nil, false
	}
	cov := parseCoverage(sub)
	return cov, cov != nil
}

// gposClassDef parses a class-definition table reached via a uint16 offset.
// A zero offset is legal for an absent ClassDef (every glyph is class 0);
// represent that with nil, which classOfOrZero handles.
func gposClassDef(data []byte, off int) (otClassDef, bool) {
	if off == 0 {
		return nil, true
	}
	sub, ok := gposSubtable(data, off)
	if !ok {
		return nil, false
	}
	cd := parseClassDef(sub)
	if cd == nil {
		return nil, false
	}
	return cd, true
}

// classOfOrZero returns the glyph class, treating a nil ClassDef as all-zero.
func classOfOrZero(cd otClassDef, gid uint16) uint16 {
	if cd == nil {
		return 0
	}
	return cd.classOf(gid)
}

// applyContextual dispatches a Type 7 (contextual) subtable.
func (e *gposEngine) applyContextual(data []byte) {
	format, ok := gposU16(data, 0)
	if !ok {
		e.fail(errGPOSMalformed)
		return
	}
	switch format {
	case 1:
		e.applyContextFormat1(data)
	case 2:
		e.applyContextFormat2(data)
	case 3:
		e.applyContextFormat3(data)
	default:
		// Unknown format: ignore, matching the tolerant behavior of the
		// existing Type 1/2 dispatch.
	}
}

// applyChaining dispatches a Type 8 (chaining contextual) subtable.
func (e *gposEngine) applyChaining(data []byte) {
	format, ok := gposU16(data, 0)
	if !ok {
		e.fail(errGPOSMalformed)
		return
	}
	switch format {
	case 1:
		e.applyChainFormat1(data)
	case 2:
		e.applyChainFormat2(data)
	case 3:
		e.applyChainFormat3(data)
	default:
	}
}

// applyLookupRecord applies one PosLookupRecord against the matched sequence.
// sequenceIndex positions are relative to the start of the matched input
// sequence at inputStart (absolute buffer index).
func (e *gposEngine) applyLookupRecord(inputStart int, rec posLookupRecord) {
	if !e.consumeStep() {
		return
	}
	target := inputStart + rec.sequenceIndex
	if target < 0 || target >= len(e.glyphs) {
		// A record pointing outside the buffer cannot apply; the rule still
		// matched, so this is not an error — just skip the record.
		return
	}
	if int(rec.lookupIndex) >= len(e.g.lookups) {
		// Bad lookup index: malformed table.
		e.fail(errGPOSMalformed)
		return
	}
	e.applyNestedLookup(rec.lookupIndex, target)
}

// posLookupRecord is a parsed SequenceIndex + LookupListIndex pair.
type posLookupRecord struct {
	sequenceIndex int
	lookupIndex   uint16
}

// parsePosLookupRecords reads count PosLookupRecords starting at off.
// Each record is two uint16 values (SequenceIndex, LookupListIndex).
func parsePosLookupRecords(data []byte, off, count int) ([]posLookupRecord, bool) {
	recs := make([]posLookupRecord, 0, count)
	pos := off
	for range count {
		si, ok1 := gposU16(data, pos)
		li, ok2 := gposU16(data, pos+2)
		if !ok1 || !ok2 {
			return nil, false
		}
		recs = append(recs, posLookupRecord{
			sequenceIndex: int(si),
			lookupIndex:   li,
		})
		pos += 4
	}
	return recs, true
}

// applyNestedLookup applies a lookup referenced by a contextual rule. It
// enforces depth and cycle limits and dispatches through the standard lookup
// machinery so extension lookups and further contextual lookups work.
func (e *gposEngine) applyNestedLookup(index uint16, atPosition int) {
	if e.err != nil {
		return
	}
	if e.activeLookups[index] {
		// Cycle: stop this branch safely without erroring the whole shaping,
		// since the rest of the buffer may still shape correctly.
		return
	}
	if e.depth >= gposMaxNestDepth {
		e.fail(errGPOSDepth)
		return
	}
	lu := &e.g.lookups[index]

	// Mark the lookup active for cycle detection.
	if e.activeLookups == nil {
		e.activeLookups = make(map[uint16]bool)
	}
	e.activeLookups[index] = true
	e.depth++

	switch lu.lookupType {
	case 1:
		for _, st := range lu.subtables {
			applySinglePosAt(st, e.glyphs, e.adj, atPosition)
		}
	case 2:
		for _, st := range lu.subtables {
			applyPairPosAt(st, e.glyphs, e.adj, atPosition)
		}
	case 7:
		for _, st := range lu.subtables {
			e.applyContextualAt(st, atPosition)
		}
	case 8:
		for _, st := range lu.subtables {
			e.applyChainingAt(st, atPosition)
		}
	case 9:
		e.applyNestedExtension(lu, atPosition)
	default:
		// Types 3-6 (cursive/mark) are not implemented yet; a contextual
		// reference to one is a safe no-op rather than an error.
	}

	e.depth--
	delete(e.activeLookups, index)
}

// applyTopLevelExtension resolves a top-level Type 9 extension subtable and
// dispatches the wrapped table over the whole buffer (the extension lookup
// itself was selected by a feature, so it is not anchored to one position).
func (e *gposEngine) applyTopLevelExtension(st []byte) {
	if len(st) < 8 {
		e.fail(errGPOSMalformed)
		return
	}
	format, ok1 := gposU16(st, 0)
	if !ok1 {
		e.fail(errGPOSMalformed)
		return
	}
	if format != 1 {
		return
	}
	extType, ok2 := gposU16(st, 2)
	extOff, ok3 := gposU32(st, 4)
	// Offset zero would alias the extension header itself; anything at or
	// past the end cannot contain a complete subtable.
	if !ok2 || !ok3 || extOff == 0 || int(extOff) >= len(st) {
		e.fail(errGPOSMalformed)
		return
	}
	inner := st[extOff:]
	switch extType {
	case 1:
		applySinglePos(inner, e.glyphs, e.adj)
	case 2:
		applyPairPos(inner, e.glyphs, e.adj)
	case 7:
		e.applyContextual(inner)
	case 8:
		e.applyChaining(inner)
	default:
		// Extensions of unimplemented types (3-6) are safe no-ops.
	}
}

// applyNestedExtension resolves a Type 9 extension wrapper and applies the
// wrapped subtable at the given position. Only format-1 extensions exist.
func (e *gposEngine) applyNestedExtension(lu *otLookup, atPosition int) {
	for _, st := range lu.subtables {
		if len(st) < 8 {
			e.fail(errGPOSMalformed)
			continue
		}
		format, ok1 := gposU16(st, 0)
		if !ok1 {
			e.fail(errGPOSMalformed)
			continue
		}
		if format != 1 {
			continue
		}
		extType, ok2 := gposU16(st, 2)
		extOff, ok3 := gposU32(st, 4)
		if !ok2 || !ok3 || extOff == 0 || int(extOff) >= len(st) {
			e.fail(errGPOSMalformed)
			continue
		}
		inner := st[extOff:]
		switch extType {
		case 1:
			applySinglePosAt(inner, e.glyphs, e.adj, atPosition)
		case 2:
			applyPairPosAt(inner, e.glyphs, e.adj, atPosition)
		case 7:
			e.applyContextualAt(inner, atPosition)
		case 8:
			e.applyChainingAt(inner, atPosition)
		default:
		}
	}
}

// --- Position-scoped Type 1/2 application ---
//
// Contextual lookups invoke a nested lookup at one specific position of the
// matched sequence, not over the whole buffer. These wrappers parse the same
// subtable formats as the top-level Type 1/2 engines but only adjust the
// target glyph (and, for PairPos, its successor).

// applySinglePosAt applies a SinglePos subtable to the glyph at pos.
func applySinglePosAt(data []byte, glyphs []shapingGlyph, adj []gposAdjustment, pos int) {
	if pos < 0 || pos >= len(glyphs) || len(data) < 6 {
		return
	}
	covOff, ok := gposU16(data, 2)
	if !ok {
		return
	}
	cov, ok := gposCoverage(data, int(covOff))
	if !ok {
		return
	}
	valueFormat, ok := gposU16(data, 4)
	if !ok {
		return
	}
	idx, covered := cov.contains(glyphs[pos].gid)
	if !covered {
		return
	}
	format, ok := gposU16(data, 0)
	if !ok {
		return
	}
	switch format {
	case 1:
		vr, _ := parseValueRecord(data, 6, valueFormat)
		accumulateAdjustment(&adj[pos], vr)
	case 2:
		vrSize := valueRecordSize(valueFormat)
		valCount, ok := gposU16(data, 6)
		if !ok || idx >= int(valCount) {
			return
		}
		vr, _ := parseValueRecord(data, 8+idx*vrSize, valueFormat)
		accumulateAdjustment(&adj[pos], vr)
	}
}

// accumulateAdjustment adds a parsed value record onto an adjustment slot.
func accumulateAdjustment(dst *gposAdjustment, vr otValueRecord) {
	dst.xPlacement += vr.xPlacement
	dst.yPlacement += vr.yPlacement
	dst.xAdvance += vr.xAdvance
	dst.yAdvance += vr.yAdvance
}

// applyPairPosAt applies a PairPos subtable with the first glyph anchored at
// pos (the pair is glyphs[pos], glyphs[pos+1]).
func applyPairPosAt(data []byte, glyphs []shapingGlyph, adj []gposAdjustment, pos int) {
	if pos < 0 || pos+1 >= len(glyphs) || len(data) < 2 {
		return
	}
	format, ok := gposU16(data, 0)
	if !ok {
		return
	}
	switch format {
	case 1:
		applyPairPosAtFormat1(data, glyphs, adj, pos)
	case 2:
		applyPairPosAtFormat2(data, glyphs, adj, pos)
	}
}

// applyPairPosAtFormat1 is the position-scoped PairPos format 1.
func applyPairPosAtFormat1(data []byte, glyphs []shapingGlyph, adj []gposAdjustment, pos int) {
	if len(data) < 10 {
		return
	}
	covOff, _ := gposU16(data, 2)
	vf1, _ := gposU16(data, 4)
	vf2, _ := gposU16(data, 6)
	pairSetCount, ok := gposU16(data, 8)
	if !ok || int(covOff) >= len(data) {
		return
	}
	cov, ok := gposCoverage(data, int(covOff))
	if !ok {
		return
	}
	idx, covered := cov.contains(glyphs[pos].gid)
	if !covered || idx >= int(pairSetCount) {
		return
	}
	psOff, ok := gposU16(data, 10+idx*2)
	if !ok || int(psOff) >= len(data) {
		return
	}
	ps := data[psOff:]
	if len(ps) < 2 {
		return
	}
	pairValueCount, _ := gposU16(ps, 0)
	vr1Size := valueRecordSize(vf1)
	vr2Size := valueRecordSize(vf2)
	recordSize := 2 + vr1Size + vr2Size
	found := searchPairSet(ps[2:], int(pairValueCount), recordSize, glyphs[pos+1].gid)
	if found < 0 {
		return
	}
	recordOff := 2 + found*recordSize
	vr1, n1 := parseValueRecord(ps, recordOff+2, vf1)
	accumulateAdjustment(&adj[pos], vr1)
	if vf2 != 0 {
		vr2, _ := parseValueRecord(ps, recordOff+2+n1, vf2)
		accumulateAdjustment(&adj[pos+1], vr2)
	}
}

// applyPairPosAtFormat2 is the position-scoped PairPos format 2.
func applyPairPosAtFormat2(data []byte, glyphs []shapingGlyph, adj []gposAdjustment, pos int) {
	if len(data) < 16 {
		return
	}
	covOff, _ := gposU16(data, 2)
	vf1, _ := gposU16(data, 4)
	vf2, _ := gposU16(data, 6)
	cd1Off, _ := gposU16(data, 8)
	cd2Off, _ := gposU16(data, 10)
	class1Count, _ := gposU16(data, 12)
	class2Count, _ := gposU16(data, 14)
	if int(covOff) >= len(data) || int(cd1Off) >= len(data) || int(cd2Off) >= len(data) {
		return
	}
	cov, ok := gposCoverage(data, int(covOff))
	if !ok {
		return
	}
	if _, covered := cov.contains(glyphs[pos].gid); !covered {
		return
	}
	cd1, ok := gposClassDef(data, int(cd1Off))
	if !ok {
		return
	}
	cd2, ok := gposClassDef(data, int(cd2Off))
	if !ok {
		return
	}
	c1 := int(classOfOrZero(cd1, glyphs[pos].gid))
	c2 := int(classOfOrZero(cd2, glyphs[pos+1].gid))
	if c1 >= int(class1Count) || c2 >= int(class2Count) {
		return
	}
	recordSize := valueRecordSize(vf1) + valueRecordSize(vf2)
	recordOff := 16 + (c1*int(class2Count)+c2)*recordSize
	vr1, n1 := parseValueRecord(data, recordOff, vf1)
	accumulateAdjustment(&adj[pos], vr1)
	if vf2 != 0 {
		vr2, _ := parseValueRecord(data, recordOff+n1, vf2)
		accumulateAdjustment(&adj[pos+1], vr2)
	}
}
