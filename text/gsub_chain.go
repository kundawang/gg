// ChainContextSubst (GSUB type 6) and ReverseChainSingleSubst (type 8).
//
// Chain rules add backtrack (glyphs before the input sequence) and lookahead
// (glyphs after it) conditions. On disk the backtrack arrays are stored in
// reverse order (nearest the input first); the lookup record sequence indices
// address only the input positions.
//
// ReverseChainSingle is in-place only and applies from the end of the buffer;
// it performs a single 1:1 substitution selected by coverage index.
package text

import "encoding/binary"

// --- Type 6: ChainContextSubst ---

func (e *gsubEngine) runChainContextLookup(lu *otLookup, depth int) bool {
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
			if e.matchChainSubtable(st, anchor, depth) {
				applied = true
			}
		}
		anchor++
	}
	return applied
}

// matchNestedChain evaluates a chain lookup at a single anchor (nested call).
func (e *gsubEngine) matchNestedChain(lu *otLookup, anchor, depth int) bool {
	for _, st := range lu.subtables {
		if e.lastErr != nil {
			break
		}
		if e.matchChainSubtable(st, anchor, depth) {
			return true
		}
	}
	return false
}

func (e *gsubEngine) matchChainSubtable(data []byte, anchor, depth int) bool {
	if !e.charge() || len(data) < 6 {
		return false
	}
	format := binary.BigEndian.Uint16(data[0:2])
	// Format 3 stores all backtrack/input/lookahead coverages inline and has
	// no leading coverage offset.
	if format == 3 {
		return e.chainF3(data, anchor, depth)
	}
	covOff := int(binary.BigEndian.Uint16(data[2:4]))
	if covOff <= 0 || covOff >= len(data) {
		return false
	}
	cov := parseCoverage(data[covOff:])
	if cov == nil {
		return false
	}
	if _, ok := cov.contains(e.glyphs[anchor].gid); !ok {
		return false
	}
	switch format {
	case 1:
		return e.chainF1(data, anchor, depth)
	case 2:
		return e.chainF2(data, anchor, depth)
	default:
		return false
	}
}

// chainF1: glyph-based chain rules indexed by coverage index.
func (e *gsubEngine) chainF1(data []byte, anchor, depth int) bool {
	setCount := int(binary.BigEndian.Uint16(data[4:6]))
	if len(data) < 6+setCount*2 {
		return false
	}
	covOff := int(binary.BigEndian.Uint16(data[2:4]))
	cov := parseCoverage(data[covOff:])
	if cov == nil {
		return false
	}
	covIdx, ok := cov.contains(e.glyphs[anchor].gid)
	if !ok || covIdx >= setCount {
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
		if e.matchChainRule(setData[ruleOff:], anchor, depth, nil, nil, nil) {
			return true
		}
	}
	return false
}

// chainRuleLayout holds the parsed section offsets of a Chain(Sub)Rule.
type chainRuleLayout struct {
	backtrackCount int
	inputCount     int
	lookaheadCount int
	substCount     int
	// Byte offsets inside the rule data.
	backtrackOff int
	inputOff     int
	lookaheadOff int
	recordOff    int
}

// parseChainRuleHeader validates and describes a ChainSubRule / ChainSubClassRule.
func parseChainRuleHeader(rule []byte) (chainRuleLayout, bool) {
	var l chainRuleLayout
	if len(rule) < 4 {
		return l, false
	}
	l.backtrackCount = int(binary.BigEndian.Uint16(rule[0:2]))
	pos := 2
	if l.backtrackCount > gsubMaxContextLen || len(rule) < pos+l.backtrackCount*2 {
		return l, false
	}
	l.backtrackOff = pos
	pos += l.backtrackCount * 2

	if len(rule) < pos+2 {
		return l, false
	}
	l.inputCount = int(binary.BigEndian.Uint16(rule[pos : pos+2]))
	pos += 2
	if l.inputCount < 1 || l.inputCount > gsubMaxContextLen ||
		len(rule) < pos+(l.inputCount-1)*2 {
		return l, false
	}
	l.inputOff = pos
	pos += (l.inputCount - 1) * 2

	if len(rule) < pos+2 {
		return l, false
	}
	l.lookaheadCount = int(binary.BigEndian.Uint16(rule[pos : pos+2]))
	pos += 2
	if l.lookaheadCount > gsubMaxContextLen || len(rule) < pos+l.lookaheadCount*2 {
		return l, false
	}
	l.lookaheadOff = pos
	pos += l.lookaheadCount * 2

	if len(rule) < pos+2 {
		return l, false
	}
	l.substCount = int(binary.BigEndian.Uint16(rule[pos : pos+2]))
	pos += 2
	if len(rule) < pos+l.substCount*4 {
		return l, false
	}
	l.recordOff = pos
	return l, true
}

// matchChainRule validates a chain rule. inputDef/backtrackDef/lookaheadDef
// are nil for glyph-ID matching (format 1); otherwise they are ClassDefs and
// the stored values are class IDs.
func (e *gsubEngine) matchChainRule(
	rule []byte, anchor, depth int,
	btDef, inDef, laDef otClassDef,
) bool {
	l, ok := parseChainRuleHeader(rule)
	if !ok {
		return false
	}

	matchGlyph := func(def otClassDef, value uint16) func(uint16) bool {
		if def == nil {
			return func(gid uint16) bool { return gid == value }
		}
		return func(gid uint16) bool { return def.classOf(gid) == value }
	}

	// Backtrack: nearest first, walking backward from the anchor.
	cur := anchor
	for i := range l.backtrackCount {
		value := binary.BigEndian.Uint16(rule[l.backtrackOff+i*2 : l.backtrackOff+i*2+2])
		prev := e.prevGlyph(e.flagCarrier, cur)
		if prev < 0 || !matchGlyph(btDef, value)(e.glyphs[prev].gid) {
			return false
		}
		cur = prev
	}

	// Input: anchor is the implicit first position.
	positions := make([]int, 1, l.inputCount)
	positions[0] = anchor
	cur = anchor
	for j := 1; j < l.inputCount; j++ {
		value := binary.BigEndian.Uint16(rule[l.inputOff+(j-1)*2 : l.inputOff+j*2])
		next := e.nextGlyph(e.flagCarrier, cur)
		if next < 0 || !matchGlyph(inDef, value)(e.glyphs[next].gid) {
			return false
		}
		positions = append(positions, next)
		cur = next
	}

	// Lookahead: continue forward after the input sequence.
	for i := range l.lookaheadCount {
		value := binary.BigEndian.Uint16(rule[l.lookaheadOff+i*2 : l.lookaheadOff+i*2+2])
		next := e.nextGlyph(e.flagCarrier, cur)
		if next < 0 || !matchGlyph(laDef, value)(e.glyphs[next].gid) {
			return false
		}
		cur = next
	}

	return e.applySubstRecords(rule[l.recordOff:], l.substCount, positions, depth)
}

// parseChainClassDefs reads the three ClassDef tables of a chain F2 subtable.
func parseChainClassDefs(data []byte) (bt, in, la otClassDef, ok bool) {
	btDefOff := int(binary.BigEndian.Uint16(data[4:6]))
	inDefOff := int(binary.BigEndian.Uint16(data[6:8]))
	laDefOff := int(binary.BigEndian.Uint16(data[8:10]))
	if btDefOff <= 0 || btDefOff >= len(data) ||
		inDefOff <= 0 || inDefOff >= len(data) ||
		laDefOff <= 0 || laDefOff >= len(data) {
		return nil, nil, nil, false
	}
	bt = parseClassDef(data[btDefOff:])
	in = parseClassDef(data[inDefOff:])
	la = parseClassDef(data[laDefOff:])
	if bt == nil || in == nil || la == nil {
		return nil, nil, nil, false
	}
	return bt, in, la, true
}

// chainF2: class-based chain rules.
// Layout: format(2) coverageOff(2) btClassDefOff(2) inClassDefOff(2)
// laClassDefOff(2) setCount(2) chainSubClassSetOff[setCount].
func (e *gsubEngine) chainF2(data []byte, anchor, depth int) bool {
	if len(data) < 12 {
		return false
	}
	setCount := int(binary.BigEndian.Uint16(data[10:12]))
	if len(data) < 12+setCount*2 {
		return false
	}
	btDef, inDef, laDef, defsOK := parseChainClassDefs(data)
	if !defsOK {
		return false
	}
	class := int(inDef.classOf(e.glyphs[anchor].gid))
	if class <= 0 || class >= setCount {
		return false
	}
	setOff := int(binary.BigEndian.Uint16(data[12+class*2 : 14+class*2]))
	if setOff <= 0 || setOff >= len(data) {
		return false
	}
	return e.applyChainRuleSet(data[setOff:], anchor, depth, btDef, inDef, laDef)
}

// applyChainRuleSet walks every ChainSubRule of one ChainSubClassSet.
func (e *gsubEngine) applyChainRuleSet(setData []byte, anchor, depth int, btDef, inDef, laDef otClassDef) bool {
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
		if e.matchChainRule(setData[ruleOff:], anchor, depth, btDef, inDef, laDef) {
			return true
		}
	}
	return false
}

// readCoverageArray reads a count-prefixed array of offset16 coverage tables
// starting at *pos, advancing *pos past the group. A count of zero yields an
// empty slice. Any truncated or unparseable entry returns ok=false.
func (e *gsubEngine) readCoverageArray(data []byte, pos *int) (covs []otCoverage, ok bool) {
	if *pos+2 > len(data) {
		return nil, false
	}
	count := int(binary.BigEndian.Uint16(data[*pos : *pos+2]))
	*pos += 2
	if count > gsubMaxContextLen || *pos+count*2 > len(data) {
		return nil, false
	}
	covs = make([]otCoverage, count)
	for i := range count {
		off := int(binary.BigEndian.Uint16(data[*pos+i*2 : *pos+i*2+2]))
		if off <= 0 || off >= len(data) {
			return nil, false
		}
		covs[i] = parseCoverage(data[off:])
		if covs[i] == nil {
			return nil, false
		}
	}
	*pos += count * 2
	return covs, true
}

// chainF3: coverage-based chain rules.
// Layout: format(2) then three count+offset-array groups in storage order
// backtrack, input, lookahead; counts start at byte 2.
func (e *gsubEngine) chainF3(data []byte, anchor, depth int) bool {
	pos := 2
	btCovs, ok1 := e.readCoverageArray(data, &pos)
	inCovs, ok2 := e.readCoverageArray(data, &pos)
	laCovs, ok3 := e.readCoverageArray(data, &pos)
	if !ok1 || !ok2 || !ok3 || len(inCovs) == 0 {
		return false
	}
	if pos+2 > len(data) {
		return false
	}
	substCount := int(binary.BigEndian.Uint16(data[pos : pos+2]))
	pos += 2
	if pos+substCount*4 > len(data) {
		return false
	}
	positions, ok := e.chainF3ContextMatches(btCovs, inCovs, laCovs, anchor)
	if !ok {
		return false
	}
	return e.applySubstRecords(data[pos:], substCount, positions, depth)
}

// chainF3ContextMatches validates the backtrack, input and lookahead
// coverage sequences around anchor using the flag-aware cursor. It returns
// the matched input glyph positions on success.
func (e *gsubEngine) chainF3ContextMatches(btCovs, inCovs, laCovs []otCoverage, anchor int) ([]int, bool) {
	if _, ok := inCovs[0].contains(e.glyphs[anchor].gid); !ok {
		return nil, false
	}
	// Backtrack coverages are stored nearest-first.
	cur := anchor
	for i := range btCovs {
		prev := e.prevGlyph(e.flagCarrier, cur)
		if prev < 0 {
			return nil, false
		}
		if _, ok := btCovs[i].contains(e.glyphs[prev].gid); !ok {
			return nil, false
		}
		cur = prev
	}
	// Input positions.
	positions := make([]int, 1, len(inCovs))
	positions[0] = anchor
	cur = anchor
	for j := 1; j < len(inCovs); j++ {
		next := e.nextGlyph(e.flagCarrier, cur)
		if next < 0 {
			return nil, false
		}
		if _, ok := inCovs[j].contains(e.glyphs[next].gid); !ok {
			return nil, false
		}
		positions = append(positions, next)
		cur = next
	}
	// Lookahead.
	for i := range laCovs {
		next := e.nextGlyph(e.flagCarrier, cur)
		if next < 0 {
			return nil, false
		}
		if _, ok := laCovs[i].contains(e.glyphs[next].gid); !ok {
			return nil, false
		}
		cur = next
	}
	return positions, true
}

// --- Type 8: ReverseChainSingleSubst ---

// runReverseChainLookup applies the reverse chain single substitution over
// the whole buffer, scanning from the end. Reverse lookups never carry
// SubstLookupRecords and always perform a 1:1 replacement in place. The
// top-level lookup flag gate still applies at every anchor.
func (e *gsubEngine) runReverseChainLookup(lu *otLookup) bool {
	e.flagCarrier = lu
	applied := false
	for anchor := len(e.glyphs) - 1; anchor >= 0 && e.lastErr == nil; anchor-- {
		if !eligibleAtGate(lu, e.gdef, e.glyphs[anchor].gid) {
			continue
		}
		if e.applyReverseAt(lu, anchor) {
			applied = true
		}
	}
	return applied
}

// applyReverseAt evaluates all reverse subtables at one anchor.
func (e *gsubEngine) applyReverseAt(lu *otLookup, anchor int) bool {
	for _, st := range lu.subtables {
		if e.lastErr != nil {
			break
		}
		if e.applyReverseSubtable(st, anchor) {
			return true
		}
	}
	return false
}

func (e *gsubEngine) applyReverseSubtable(data []byte, anchor int) bool {
	if !e.charge() || len(data) < 6 {
		return false
	}
	format := binary.BigEndian.Uint16(data[0:2])
	if format != 1 {
		// Format 2 is the 24-bit-offset extension; not used in GSUB 1.0 and
		// safely treated as an unknown subtable.
		return false
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
	pos := 4

	btCovs, ok1 := e.readCoverageArray(data, &pos)
	laCovs, ok2 := e.readCoverageArray(data, &pos)
	if !ok1 || !ok2 {
		return false
	}
	if pos+2 > len(data) {
		return false
	}
	glyphCount := int(binary.BigEndian.Uint16(data[pos : pos+2]))
	pos += 2
	if covIdx >= glyphCount || pos+glyphCount*2 > len(data) {
		return false
	}
	if !e.reverseContextMatches(btCovs, laCovs, anchor) {
		return false
	}

	substitute := binary.BigEndian.Uint16(data[pos+covIdx*2 : pos+covIdx*2+2])
	e.glyphs[anchor].gid = substitute
	return true
}

// reverseContextMatches validates the backtrack (nearest-first) and lookahead
// coverage sequences surrounding anchor.
func (e *gsubEngine) reverseContextMatches(btCovs, laCovs []otCoverage, anchor int) bool {
	cur := anchor
	for i := range btCovs {
		prev := e.prevGlyph(e.flagCarrier, cur)
		if prev < 0 {
			return false
		}
		if _, ok := btCovs[i].contains(e.glyphs[prev].gid); !ok {
			return false
		}
		cur = prev
	}
	cur = anchor
	for i := range laCovs {
		next := e.nextGlyph(e.flagCarrier, cur)
		if next < 0 {
			return false
		}
		if _, ok := laCovs[i].contains(e.glyphs[next].gid); !ok {
			return false
		}
		cur = next
	}
	return true
}
