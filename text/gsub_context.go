// Contextual GSUB lookup types:
//
//   - Type 5 ContextSubst, formats 1 (glyph rules), 2 (class rules),
//     3 (coverage sequence).
//   - Type 6 ChainContextSubst, formats 1/2/3 with backtrack and lookahead
//     sequences.
//   - Type 8 ReverseChainSingleSubst, format 1 (coverage-based single
//     substitution applied from the end of the buffer).
//
// All sequence matching uses the flag-aware cursor so IgnoreMarks and the
// Ignore* lookup flags behave exactly as for simple lookups.
package text

import "encoding/binary"

// --- Type 5: ContextSubst ---

// runContextLookup scans the buffer for context rules and applies their
// SubstLookupRecords in stored order.
func (e *gsubEngine) runContextLookup(lu *otLookup, depth int) bool {
	e.flagCarrier = lu
	applied := false
	anchor := 0
	for anchor < len(e.glyphs) && e.lastErr == nil {
		if !eligibleAtGate(lu, e.gdef, e.glyphs[anchor].gid) {
			anchor++
			continue
		}
		for _, st := range lu.subtables {
			if e.lastErr != nil {
				break
			}
			if e.matchContextSubtable(st, anchor, depth) {
				applied = true
			}
		}
		// Rules only substitute; the anchor glyph itself stays in place,
		// so always advance one slot.
		anchor++
	}
	return applied
}

// matchContextSubtable dispatches a ContextSubst subtable by format.
func (e *gsubEngine) matchContextSubtable(data []byte, anchor, depth int) bool {
	if !e.charge() || len(data) < 6 {
		return false
	}
	format := binary.BigEndian.Uint16(data[0:2])
	// Format 3 has no leading coverage offset; it carries one coverage per
	// input position internally.
	if format == 3 {
		return e.contextF3(data, anchor, depth)
	}
	covOff := int(binary.BigEndian.Uint16(data[2:4]))
	if covOff <= 0 || covOff >= len(data) {
		return false
	}
	cov := parseCoverage(data[covOff:])
	if cov == nil {
		return false
	}
	covIdx, ok := cov.contains(e.glyphs[anchor].gid)
	if !ok {
		return false
	}
	switch format {
	case 1:
		return e.contextF1(data, covIdx, anchor, depth)
	case 2:
		return e.contextF2(data, anchor, depth)
	default:
		return false // unknown format: safe stop
	}
}

// contextF1 handles SimpleContext (rule sets indexed by coverage index).
func (e *gsubEngine) contextF1(data []byte, covIdx, anchor, depth int) bool {
	setCount := int(binary.BigEndian.Uint16(data[4:6]))
	if covIdx >= setCount || len(data) < 6+setCount*2 {
		return false
	}
	setOff := int(binary.BigEndian.Uint16(data[6+covIdx*2 : 8+covIdx*2]))
	if setOff <= 0 || setOff >= len(data) {
		return false
	}
	setData := data[setOff:]
	if len(setData) < 2 {
		return false
	}
	ruleCount := int(binary.BigEndian.Uint16(setData[0:2]))
	if len(setData) < 2+ruleCount*2 {
		return false
	}
	for r := range ruleCount {
		ruleOff := int(binary.BigEndian.Uint16(setData[2+r*2 : 4+r*2]))
		if ruleOff <= 0 || ruleOff >= len(setData) {
			continue
		}
		rule := setData[ruleOff:]
		if e.matchRuleAndApply(rule, anchor, depth, nil) {
			return true
		}
	}
	return false
}

// matchRuleAndApply validates a SubRule's glyph input sequence and, on match,
// runs its SubstLookupRecords. classDef is nil for glyph rules; when non-nil
// the input values are class IDs instead of glyph IDs.
func (e *gsubEngine) matchRuleAndApply(rule []byte, anchor, depth int, classDef otClassDef) bool {
	if len(rule) < 4 {
		return false
	}
	glyphCount := int(binary.BigEndian.Uint16(rule[0:2]))
	substCount := int(binary.BigEndian.Uint16(rule[2:4]))
	if glyphCount < 1 || glyphCount > gsubMaxContextLen {
		return false
	}
	inputEnd := 4 + (glyphCount-1)*2
	if len(rule) < inputEnd+substCount*4 {
		return false
	}

	positions := make([]int, 1, glyphCount)
	positions[0] = anchor
	cur := anchor
	lu := e.flagCarrier
	for j := 1; j < glyphCount; j++ {
		want := binary.BigEndian.Uint16(rule[4+(j-1)*2 : 6+(j-1)*2])
		next := e.nextGlyph(lu, cur)
		if next < 0 {
			return false
		}
		if classDef != nil {
			if classDef.classOf(e.glyphs[next].gid) != want {
				return false
			}
		} else if e.glyphs[next].gid != want {
			return false
		}
		positions = append(positions, next)
		cur = next
	}
	return e.applySubstRecords(rule[inputEnd:], substCount, positions, depth)
}

// contextF2 handles ClassBasedContext.
func (e *gsubEngine) contextF2(data []byte, anchor, depth int) bool {
	// Layout: format(2) coverageOff(2) classDefOff(2) setCount(2) setOff[].
	if len(data) < 8 {
		return false
	}
	classDefOff := int(binary.BigEndian.Uint16(data[4:6]))
	setCount := int(binary.BigEndian.Uint16(data[6:8]))
	if classDefOff <= 0 || classDefOff >= len(data) || len(data) < 8+setCount*2 {
		return false
	}
	classDef := parseClassDef(data[classDefOff:])
	if classDef == nil {
		return false
	}
	class := int(classDef.classOf(e.glyphs[anchor].gid))
	if class <= 0 || class >= setCount {
		return false
	}
	setOff := int(binary.BigEndian.Uint16(data[8+class*2 : 10+class*2]))
	if setOff <= 0 || setOff >= len(data) {
		return false
	}
	setData := data[setOff:]
	if len(setData) < 2 {
		return false
	}
	ruleCount := int(binary.BigEndian.Uint16(setData[0:2]))
	if len(setData) < 2+ruleCount*2 {
		return false
	}
	for r := range ruleCount {
		ruleOff := int(binary.BigEndian.Uint16(setData[2+r*2 : 4+r*2]))
		if ruleOff <= 0 || ruleOff >= len(setData) {
			continue
		}
		rule := setData[ruleOff:]
		if e.matchRuleAndApply(rule, anchor, depth, classDef) {
			return true
		}
	}
	return false
}

// contextF3 handles CoverageBasedContext: one coverage per input position.
// Layout: format(2) glyphCount(2) substCount(2) coverageOff[glyphCount]
// followed by SubstLookupRecord[substCount]. There is no leading coverage
// offset in format 3.
func (e *gsubEngine) contextF3(data []byte, anchor, depth int) bool {
	if len(data) < 6 {
		return false
	}
	glyphCount := int(binary.BigEndian.Uint16(data[2:4]))
	substCount := int(binary.BigEndian.Uint16(data[4:6]))
	if glyphCount < 1 || glyphCount > gsubMaxContextLen || len(data) < 6+glyphCount*2 {
		return false
	}
	coverages := make([]otCoverage, glyphCount)
	for i := range glyphCount {
		off := int(binary.BigEndian.Uint16(data[6+i*2 : 8+i*2]))
		if off <= 0 || off >= len(data) {
			return false
		}
		cov := parseCoverage(data[off:])
		if cov == nil {
			return false
		}
		coverages[i] = cov
	}
	recordStart := 6 + glyphCount*2
	if len(data) < recordStart+substCount*4 {
		return false
	}

	// Anchor coverage check.
	if _, ok := coverages[0].contains(e.glyphs[anchor].gid); !ok {
		return false
	}
	positions := make([]int, 1, glyphCount)
	positions[0] = anchor
	cur := anchor
	lu := e.flagCarrier
	for j := 1; j < glyphCount; j++ {
		next := e.nextGlyph(lu, cur)
		if next < 0 {
			return false
		}
		if _, ok := coverages[j].contains(e.glyphs[next].gid); !ok {
			return false
		}
		positions = append(positions, next)
		cur = next
	}
	return e.applySubstRecords(data[recordStart:], substCount, positions, depth)
}

// applySubstRecords runs SubstLookupRecords already validated for length.
// Records are applied in stored order; SequenceIndex selects a matched
// position. Earlier records may change the buffer length (multiple/ligature
// substitutions); later positions are shifted by the length delta, matching
// HarfBuzz's match_positions adjustment in apply_lookup.
func (e *gsubEngine) applySubstRecords(records []byte, count int, positions []int, depth int) bool {
	applied := false
	for r := range count {
		seqIdx := int(binary.BigEndian.Uint16(records[r*4 : r*4+2]))
		lookupIdx := int(binary.BigEndian.Uint16(records[r*4+2 : r*4+4]))
		if seqIdx >= len(positions) {
			continue
		}
		anchor := positions[seqIdx]
		if anchor < 0 || anchor >= len(e.glyphs) {
			continue // earlier recursed lookup removed this slot
		}

		origLen := len(e.glyphs)
		if e.invokeNested(lookupIdx, anchor, depth+1) {
			applied = true
		}
		delta := len(e.glyphs) - origLen
		if delta == 0 {
			continue
		}
		// Growth shifts positions at or after the anchor; a contraction
		// keeps the anchor itself and shifts only later positions.
		for i := range positions {
			switch {
			case delta > 0 && positions[i] >= anchor:
				positions[i] += delta
			case delta < 0 && positions[i] > anchor:
				positions[i] += delta
			}
		}
	}
	return applied
}

// invokeNested runs lookupIdx against a single anchor. It mimics HarfBuzz's
// recursive dispatch: the nested lookup applies at the anchor without a
// top-level mark/property gate (coverage decides), while its own flags still
// govern component/sequence walking.
func (e *gsubEngine) invokeNested(lookupIdx, anchor, depth int) bool {
	// Range check first: a dangling lookup reference must be a safe skip
	// even when it appears repeatedly inside a cyclic rule.
	if lookupIdx < 0 || lookupIdx >= len(e.table.lookups) {
		return false
	}
	if depth >= gsubMaxNesting {
		e.fail(errGSUBMaxNesting)
		return false
	}
	lu := &e.table.lookups[lookupIdx]
	if len(lu.subtables) == 0 {
		return false
	}
	if !e.charge() {
		return false
	}

	savedCarrier := e.flagCarrier
	e.flagCarrier = lu
	applied := false
	switch lu.lookupType {
	case 7:
		applied = e.applyExtensionLookup(lu, depth)
	case 1, 2, 3, 4:
		for _, st := range lu.subtables {
			if e.lastErr != nil {
				break
			}
			if _, did := e.applySimpleSubtableAt(lu.lookupType, st, anchor); did {
				applied = true
			}
		}
	case 5:
		applied = e.matchNestedContext(lu, anchor, depth)
	case 6:
		applied = e.matchNestedChain(lu, anchor, depth)
	case 8:
		applied = e.applyReverseAt(lu, anchor)
	default:
		// Unknown nested type: safe no-op.
	}
	e.flagCarrier = savedCarrier
	return applied
}

// matchNestedContext evaluates a context lookup at a single anchor rather
// than scanning the whole buffer.
func (e *gsubEngine) matchNestedContext(lu *otLookup, anchor, depth int) bool {
	for _, st := range lu.subtables {
		if e.lastErr != nil {
			break
		}
		if e.matchContextSubtable(st, anchor, depth) {
			return true
		}
	}
	return false
}
