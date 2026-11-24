// GPOS Type 7/8 subtable format implementations.
//
// Each format has its own matcher but they share one rule-application
// contract: walk every glyph position as a potential sequence start, test
// the rule's input/backtrack/lookahead sequences, and on a full match apply
// the PosLookupRecords in order. Glyphs are never consumed — a position can
// therefore match more than one rule/lookup and adjustments accumulate
// deterministically in lookup order.

package text

// ruleApplier tests one rule at buffer pos and, on match, applies its
// lookup records. Returning true means the rule matched.
type ruleApplier func(rule []byte, pos int) bool

// runRuleSet walks the rule sets attached to the coverage/class selected at
// each glyph position and applies the first matching rule of each set.
// It contains the shared loop of Type 7/8 formats 1 and 2.
func (e *gposEngine) runRuleSet(
	data []byte,
	ruleSetOffsetBase int,
	selectSet func(pos int) (setIndex int, ok bool),
	applyRule ruleApplier,
) {
	for pos := range e.glyphs {
		if !e.consumeStep() || e.err != nil {
			return
		}
		if !e.anchorAllowed(pos) {
			continue
		}
		setIndex, ok := selectSet(pos)
		if !ok {
			continue
		}
		if !e.applyFirstMatchingRule(data, ruleSetOffsetBase, setIndex, pos, applyRule) {
			return
		}
	}
}

// applyFirstMatchingRule resolves a rule set and applies the first rule that
// matches at pos. Returns false when a malformed table aborted processing.
func (e *gposEngine) applyFirstMatchingRule(
	data []byte, offsetBase, setIndex, pos int, applyRule ruleApplier,
) bool {
	setOff, ok := gposU16(data, offsetBase+setIndex*2)
	if !ok {
		e.fail(errGPOSMalformed)
		return false
	}
	if setOff == 0 {
		return true // class/coverage entry explicitly has no rule set
	}
	if int(setOff) >= len(data) {
		e.fail(errGPOSMalformed)
		return false
	}
	set := data[setOff:]
	ruleCount, ok := gposU16(set, 0)
	if !ok || len(set) < 2+int(ruleCount)*2 {
		e.fail(errGPOSMalformed)
		return false
	}
	for ri := range int(ruleCount) {
		ruleOff, ok := gposU16(set, 2+ri*2)
		if !ok || int(ruleOff) >= len(set) {
			e.fail(errGPOSMalformed)
			return false
		}
		if applyRule(set[ruleOff:], pos) {
			break // first matching rule in the set wins
		}
		if e.err != nil {
			return false
		}
	}
	return true
}

// applyRecords applies the PosLookupRecords of a matched rule at inputStart
// and reports whether processing may continue.
func (e *gposEngine) applyRecords(inputStart int, recs []posLookupRecord) bool {
	for _, rec := range recs {
		e.applyLookupRecord(inputStart, rec)
		if e.err != nil {
			return false
		}
	}
	return true
}

// matchGlyphSequence verifies glyphs[pos+1 .. pos+gc-1] against the glyph IDs
// stored in rule at ruleOff (Type 7 format 1 input arrays).
func (e *gposEngine) matchGlyphSequence(rule []byte, ruleOff, gc, pos int) bool {
	for j := 1; j < gc; j++ {
		want, ok := gposU16(rule, ruleOff+(j-1)*2)
		if !ok {
			e.fail(errGPOSMalformed)
			return false
		}
		if e.glyphs[pos+j].gid != want {
			return false
		}
	}
	return true
}

// matchClassSequence verifies glyphs[pos+1 .. pos+gc-1] against class values
// stored in rule at ruleOff (Type 7 format 2 class arrays).
func (e *gposEngine) matchClassSequence(rule []byte, ruleOff, gc, pos int, cd otClassDef) bool {
	for j := 1; j < gc; j++ {
		wantClass, ok := gposU16(rule, ruleOff+(j-1)*2)
		if !ok {
			e.fail(errGPOSMalformed)
			return false
		}
		if classOfOrZero(cd, e.glyphs[pos+j].gid) != wantClass {
			return false
		}
	}
	return true
}

// ruleHeader reads glyphCount and posCount and validates the sequence fits
// the buffer. gc==0 means malformed.
func (e *gposEngine) ruleHeader(rule []byte, pos int) (gc, pc int, ok bool) {
	glyphCount, ok1 := gposU16(rule, 0)
	posCount, ok2 := gposU16(rule, 2)
	if !ok1 || !ok2 || glyphCount == 0 {
		e.fail(errGPOSMalformed)
		return 0, 0, false
	}
	gc = int(glyphCount)
	if pos+gc > len(e.glyphs) {
		return 0, 0, false // sequence runs past end — no match, not an error
	}
	return gc, int(posCount), true
}

// ---------------------------------------------------------------------------
// Type 7 — Contextual Positioning
// ---------------------------------------------------------------------------

// ContextPos Format 1:
//
//	uint16 format
//	uint16 coverageOffset
//	uint16 posRuleSetCount
//	uint16 posRuleSetOffsets[posRuleSetCount]
//	PosRuleSet: uint16 posRuleCount + uint16 posRuleOffsets[]
//	PosRule:
//	  uint16 glyphCount
//	  uint16 posCount
//	  uint16 input[glyphCount-1]
//	  PosLookupRecord[posCount]
func (e *gposEngine) applyContextFormat1(data []byte) {
	cov, ruleSetCount, ok := e.parseCoverageRuleSetHeader(data, 6)
	if !ok {
		return
	}
	e.runRuleSet(data, 6, func(pos int) (int, bool) {
		covIdx, covered := cov.contains(e.glyphs[pos].gid)
		if !covered || covIdx >= ruleSetCount {
			return 0, false
		}
		return covIdx, true
	}, e.matchContextRule1)
}

// parseCoverageRuleSetHeader parses the common header of formats 1:
// coverage at offset field 2 and a rule-set count at countOff, validating the
// offset array that follows the header.
func (e *gposEngine) parseCoverageRuleSetHeader(data []byte, headerSize int) (otCoverage, int, bool) {
	if len(data) < headerSize {
		e.fail(errGPOSMalformed)
		return nil, 0, false
	}
	covOff, _ := gposU16(data, 2)
	count, ok := gposU16(data, headerSize-2)
	if !ok || len(data) < headerSize+int(count)*2 {
		e.fail(errGPOSMalformed)
		return nil, 0, false
	}
	cov, ok := gposCoverage(data, int(covOff))
	if !ok {
		e.fail(errGPOSMalformed)
		return nil, 0, false
	}
	return cov, int(count), true
}

// matchContextRule1 tests and applies a PosRule starting at buffer pos.
func (e *gposEngine) matchContextRule1(rule []byte, pos int) bool {
	gc, pc, ok := e.ruleHeader(rule, pos)
	if !ok {
		return false
	}
	if !e.matchGlyphSequence(rule, 4, gc, pos) {
		return false // no match, or malformed table (e.err set stops the caller)
	}
	recs, parsed := parsePosLookupRecords(rule, 4+(gc-1)*2, pc)
	if !parsed {
		e.fail(errGPOSMalformed)
		return false
	}
	return e.applyRecords(pos, recs)
}

// ContextPos Format 2 (class-based):
//
//	uint16 format, coverageOffset, classDefOffset, posClassSetCount
//	uint16 posClassSetOffsets[]   (offset may be 0 → no rules for that class)
//	PosClassSet → PosClassRule[]; PosClassRule holds class input sequence.
func (e *gposEngine) applyContextFormat2(data []byte) {
	if len(data) < 8 {
		e.fail(errGPOSMalformed)
		return
	}
	cov, cd, setCount, ok := e.parseContextClassHeader(data)
	if !ok {
		return
	}
	e.runRuleSet(data, 8, func(pos int) (int, bool) {
		if _, covered := cov.contains(e.glyphs[pos].gid); !covered {
			return 0, false
		}
		class := int(classOfOrZero(cd, e.glyphs[pos].gid))
		if class == 0 || class >= setCount {
			return 0, false // class 0 glyphs never have rules (spec)
		}
		return class, true
	}, func(rule []byte, pos int) bool {
		return e.matchContextClassRule(rule, pos, cd)
	})
}

// parseContextClassHeader parses coverage, ClassDef and set count for
// Type 7 format 2.
func (e *gposEngine) parseContextClassHeader(data []byte) (otCoverage, otClassDef, int, bool) {
	covOff, _ := gposU16(data, 2)
	cdOff, _ := gposU16(data, 4)
	setCount, ok := gposU16(data, 6)
	if !ok || len(data) < 8+int(setCount)*2 {
		e.fail(errGPOSMalformed)
		return nil, nil, 0, false
	}
	cov, ok := gposCoverage(data, int(covOff))
	if !ok {
		e.fail(errGPOSMalformed)
		return nil, nil, 0, false
	}
	cd, ok := gposClassDef(data, int(cdOff))
	if !ok {
		e.fail(errGPOSMalformed)
		return nil, nil, 0, false
	}
	return cov, cd, int(setCount), true
}

// matchContextClassRule tests a PosClassRule (class sequence) at pos.
func (e *gposEngine) matchContextClassRule(rule []byte, pos int, cd otClassDef) bool {
	gc, pc, ok := e.ruleHeader(rule, pos)
	if !ok {
		return false
	}
	if !e.matchClassSequence(rule, 4, gc, pos, cd) {
		return false // no match, or malformed table (e.err set stops the caller)
	}
	recs, parsed := parsePosLookupRecords(rule, 4+(gc-1)*2, pc)
	if !parsed {
		e.fail(errGPOSMalformed)
		return false
	}
	return e.applyRecords(pos, recs)
}

// ContextPos Format 3 (coverage arrays):
//
//	uint16 format, glyphCount, posCount
//	uint16 coverageOffsets[glyphCount]
//	PosLookupRecord[posCount]
func (e *gposEngine) applyContextFormat3(data []byte) {
	coverages, recs, ok := e.parseCoverageArrayRule(data)
	if !ok {
		return
	}
	gc := len(coverages)
	for pos := 0; pos+gc <= len(e.glyphs); pos++ {
		if !e.consumeStep() || e.err != nil {
			return
		}
		if !e.anchorAllowed(pos) || !e.coveragesMatch(coverages, pos, 1) {
			continue
		}
		if !e.applyRecords(pos, recs) {
			return
		}
	}
}

// parseCoverageArrayRule parses a Type 7 format-3 subtable into its coverage
// array and lookup records.
func (e *gposEngine) parseCoverageArrayRule(data []byte) ([]otCoverage, []posLookupRecord, bool) {
	if len(data) < 6 {
		e.fail(errGPOSMalformed)
		return nil, nil, false
	}
	glyphCount, ok1 := gposU16(data, 2)
	posCount, ok2 := gposU16(data, 4)
	if !ok1 || !ok2 || glyphCount == 0 || len(data) < 6+int(glyphCount)*2 {
		e.fail(errGPOSMalformed)
		return nil, nil, false
	}
	gc := int(glyphCount)
	coverages := make([]otCoverage, gc)
	for j := range gc {
		off, ok := gposU16(data, 6+j*2)
		if !ok {
			e.fail(errGPOSMalformed)
			return nil, nil, false
		}
		cov, ok := gposCoverage(data, int(off))
		if !ok {
			e.fail(errGPOSMalformed)
			return nil, nil, false
		}
		coverages[j] = cov
	}
	recs, ok := parsePosLookupRecords(data, 6+gc*2, int(posCount))
	if !ok {
		e.fail(errGPOSMalformed)
		return nil, nil, false
	}
	return coverages, recs, true
}

// coveragesMatch checks that coverages[k] contains glyphs[start+k*step] for
// every k. A step of 1 scans a contiguous input sequence.
func (e *gposEngine) coveragesMatch(coverages []otCoverage, start, step int) bool {
	for j, cov := range coverages {
		if _, covered := cov.contains(e.glyphs[start+j*step].gid); !covered {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Type 8 — Chaining Contextual Positioning
// ---------------------------------------------------------------------------

// ChainContextPos Format 1 (glyph sequences):
// backtrack sequence is matched ending just before pos; input starts at pos;
// lookahead starts right after the input sequence.
//
// ChainPosRule binary layout:
//
//	uint16 backtrackGlyphCount
//	uint16 backtrack[backtrackGlyphCount]   // stored nearest-to-farthest
//	uint16 inputGlyphCount
//	uint16 input[inputGlyphCount-1]
//	uint16 lookAheadGlyphCount
//	uint16 lookAhead[lookAheadGlyphCount]   // stored nearest-to-farthest
//	uint16 posCount
//	PosLookupRecord[posCount]
func (e *gposEngine) applyChainFormat1(data []byte) {
	cov, ruleSetCount, ok := e.parseCoverageRuleSetHeader(data, 6)
	if !ok {
		return
	}
	e.runRuleSet(data, 6, func(pos int) (int, bool) {
		covIdx, covered := cov.contains(e.glyphs[pos].gid)
		if !covered || covIdx >= ruleSetCount {
			return 0, false
		}
		return covIdx, true
	}, e.matchChainRule1)
}

// chainSequences holds the three parsed sections of a chaining rule.
type chainSequences struct {
	backtrack []uint16 // nearest-to-farthest glyph IDs or classes
	input     []uint16 // glyphs after the first coverage/class anchor
	lookahead []uint16 // nearest-to-farthest, starts after the input
}

// parseChainRuleSections walks the variable-length backtrack/input/lookahead
// sections of a ChainPosRule or ChainPosClassRule and returns them together
// with the offset of the posCount field.
func (e *gposEngine) parseChainRuleSections(rule []byte) (chainSequences, int, bool) {
	var sec chainSequences
	p := 0
	btCount, ok := gposU16(rule, p)
	if !ok {
		e.fail(errGPOSMalformed)
		return sec, 0, false
	}
	p += 2
	bt := int(btCount)
	sec.backtrack = make([]uint16, bt)
	for j := range bt {
		v, ok := gposU16(rule, p+j*2)
		if !ok {
			e.fail(errGPOSMalformed)
			return sec, 0, false
		}
		sec.backtrack[j] = v
	}
	p += bt * 2

	inCount, ok := gposU16(rule, p)
	if !ok || inCount == 0 {
		e.fail(errGPOSMalformed)
		return sec, 0, false
	}
	p += 2
	ic := int(inCount)
	sec.input = make([]uint16, ic-1)
	for j := range ic - 1 {
		v, ok := gposU16(rule, p+j*2)
		if !ok {
			e.fail(errGPOSMalformed)
			return sec, 0, false
		}
		sec.input[j] = v
	}
	p += (ic - 1) * 2

	laCount, ok := gposU16(rule, p)
	if !ok {
		e.fail(errGPOSMalformed)
		return sec, 0, false
	}
	p += 2
	la := int(laCount)
	sec.lookahead = make([]uint16, la)
	for j := range la {
		v, ok := gposU16(rule, p+j*2)
		if !ok {
			e.fail(errGPOSMalformed)
			return sec, 0, false
		}
		sec.lookahead[j] = v
	}
	p += la * 2
	return sec, p, true
}

// matchChainGlyphSections checks the three sections against glyph IDs.
// pos is the input anchor; the input count is len(input)+1.
func (e *gposEngine) matchChainGlyphSections(sec chainSequences, pos int) bool {
	bt, ic, la := len(sec.backtrack), len(sec.input)+1, len(sec.lookahead)
	if pos < bt || pos+ic > len(e.glyphs) || pos+ic+la > len(e.glyphs) {
		return false
	}
	for j, want := range sec.backtrack {
		if e.glyphs[pos-1-j].gid != want {
			return false
		}
	}
	for j, want := range sec.input {
		if e.glyphs[pos+1+j].gid != want {
			return false
		}
	}
	laStart := pos + ic
	for j, want := range sec.lookahead {
		if e.glyphs[laStart+j].gid != want {
			return false
		}
	}
	return true
}

// matchChainClassSections checks the three sections against class values.
func (e *gposEngine) matchChainClassSections(
	sec chainSequences, pos int, btCD, inCD, laCD otClassDef,
) bool {
	bt, ic, la := len(sec.backtrack), len(sec.input)+1, len(sec.lookahead)
	if pos < bt || pos+ic > len(e.glyphs) || pos+ic+la > len(e.glyphs) {
		return false
	}
	for j, want := range sec.backtrack {
		if classOfOrZero(btCD, e.glyphs[pos-1-j].gid) != want {
			return false
		}
	}
	for j, want := range sec.input {
		if classOfOrZero(inCD, e.glyphs[pos+1+j].gid) != want {
			return false
		}
	}
	laStart := pos + ic
	for j, want := range sec.lookahead {
		if classOfOrZero(laCD, e.glyphs[laStart+j].gid) != want {
			return false
		}
	}
	return true
}

// matchChainRule1 tests and applies a ChainPosRule with its input sequence
// starting at buffer pos.
func (e *gposEngine) matchChainRule1(rule []byte, pos int) bool {
	sec, recordsOff, ok := e.parseChainRuleSections(rule)
	if !ok {
		return false
	}
	if !e.matchChainGlyphSections(sec, pos) {
		return false
	}
	return e.applyChainRecords(rule, recordsOff, pos)
}

// applyChainRecords reads posCount and the lookup records at recordsOff and
// applies them at the input anchor pos.
func (e *gposEngine) applyChainRecords(rule []byte, recordsOff, pos int) bool {
	posCount, ok := gposU16(rule, recordsOff)
	if !ok {
		e.fail(errGPOSMalformed)
		return false
	}
	recs, parsed := parsePosLookupRecords(rule, recordsOff+2, int(posCount))
	if !parsed {
		e.fail(errGPOSMalformed)
		return false
	}
	return e.applyRecords(pos, recs)
}

// ChainContextPos Format 2 (class-based). Layout:
//
//	uint16 format, coverageOffset
//	uint16 backtrackClassDefOffset, inputClassDefOffset, lookAheadClassDefOffset
//	uint16 chainPosClassSetCount, uint16 chainPosClassSetOffsets[]
//	ChainPosClassRule mirrors format 1 but with class values.
func (e *gposEngine) applyChainFormat2(data []byte) {
	h, ok := e.parseChainClassHeader(data)
	if !ok {
		return
	}
	e.runRuleSet(data, 12, func(pos int) (int, bool) {
		if _, covered := h.cov.contains(e.glyphs[pos].gid); !covered {
			return 0, false
		}
		class := int(classOfOrZero(h.input, e.glyphs[pos].gid))
		if class == 0 || class >= h.setCount {
			return 0, false
		}
		return class, true
	}, func(rule []byte, pos int) bool {
		return e.matchChainClassRule(rule, pos, h.backtrack, h.input, h.la)
	})
}

// chainClassHeader holds the parsed header of a Type 8 format-2 subtable.
type chainClassHeader struct {
	cov                  otCoverage
	backtrack, input, la otClassDef
	setCount             int
}

// parseChainClassHeader parses coverage and the three ClassDefs of Type 8
// format 2 plus the rule-set count.
func (e *gposEngine) parseChainClassHeader(data []byte) (chainClassHeader, bool) {
	var h chainClassHeader
	if len(data) < 12 {
		e.fail(errGPOSMalformed)
		return h, false
	}
	covOff, _ := gposU16(data, 2)
	btOff, _ := gposU16(data, 4)
	inOff, _ := gposU16(data, 6)
	laOff, _ := gposU16(data, 8)
	count, ok := gposU16(data, 10)
	if !ok || len(data) < 12+int(count)*2 {
		e.fail(errGPOSMalformed)
		return h, false
	}
	h.setCount = int(count)
	var parsed bool
	if h.cov, parsed = gposCoverage(data, int(covOff)); !parsed {
		e.fail(errGPOSMalformed)
		return h, false
	}
	if h.backtrack, parsed = gposClassDef(data, int(btOff)); !parsed {
		e.fail(errGPOSMalformed)
		return h, false
	}
	if h.input, parsed = gposClassDef(data, int(inOff)); !parsed {
		e.fail(errGPOSMalformed)
		return h, false
	}
	if h.la, parsed = gposClassDef(data, int(laOff)); !parsed {
		e.fail(errGPOSMalformed)
		return h, false
	}
	return h, true
}

// matchChainClassRule tests a ChainPosClassRule at pos.
func (e *gposEngine) matchChainClassRule(rule []byte, pos int, btCD, inCD, laCD otClassDef) bool {
	sec, recordsOff, ok := e.parseChainRuleSections(rule)
	if !ok {
		return false
	}
	if !e.matchChainClassSections(sec, pos, btCD, inCD, laCD) {
		return false
	}
	return e.applyChainRecords(rule, recordsOff, pos)
}

// ChainContextPos Format 3 (coverage arrays). Layout:
//
//	uint16 format
//	uint16 backtrackGlyphCount, backtrackCoverageOffsets[]
//	uint16 inputGlyphCount,      inputCoverageOffsets[]
//	uint16 lookAheadGlyphCount,  lookAheadCoverageOffsets[]
//	uint16 posCount, PosLookupRecord[]
//
// Backtrack coverages are ordered nearest-to-farthest (coverage 0 matches
// glyphs[pos-1]); lookahead coverages are ordered nearest-to-farthest
// starting right after the input sequence.
func (e *gposEngine) applyChainFormat3(data []byte) {
	btCovs, inCovs, laCovs, recs, ok := e.parseChainFormat3(data)
	if !ok {
		return
	}
	ic := len(inCovs)
	for pos := 0; pos+ic <= len(e.glyphs); pos++ {
		if !e.consumeStep() || e.err != nil {
			return
		}
		if !e.anchorAllowed(pos) {
			continue
		}
		if !e.chainFormat3Matches(btCovs, inCovs, laCovs, pos) {
			continue
		}
		if !e.applyRecords(pos, recs) {
			return
		}
	}
}

// parseChainFormat3 parses the three coverage arrays and lookup records of a
// Type 8 format-3 subtable.
func (e *gposEngine) parseChainFormat3(data []byte) (
	btCovs, inCovs, laCovs []otCoverage, recs []posLookupRecord, ok bool,
) {
	if len(data) < 4 {
		e.fail(errGPOSMalformed)
		return nil, nil, nil, nil, false
	}
	p := 2
	btCovs, p, ok = e.parseCoverageArray(data, p)
	if !ok {
		return nil, nil, nil, nil, false
	}
	inCovs, p, ok = e.parseCoverageArray(data, p)
	if !ok || len(inCovs) == 0 {
		e.fail(errGPOSMalformed)
		return nil, nil, nil, nil, false
	}
	laCovs, p, ok = e.parseCoverageArray(data, p)
	if !ok {
		return nil, nil, nil, nil, false
	}
	posCount, ok := gposU16(data, p)
	if !ok {
		e.fail(errGPOSMalformed)
		return nil, nil, nil, nil, false
	}
	recs, parsed := parsePosLookupRecords(data, p+2, int(posCount))
	if !parsed {
		e.fail(errGPOSMalformed)
		return nil, nil, nil, nil, false
	}
	return btCovs, inCovs, laCovs, recs, true
}

// parseCoverageArray reads a count-prefixed coverage array (used by all
// three sections of Type 8 format 3) and returns the coverages plus the
// offset immediately after the array.
func (e *gposEngine) parseCoverageArray(data []byte, off int) ([]otCoverage, int, bool) {
	count, ok := gposU16(data, off)
	if !ok {
		e.fail(errGPOSMalformed)
		return nil, 0, false
	}
	n := int(count)
	base := off + 2
	if len(data) < base+n*2 {
		e.fail(errGPOSMalformed)
		return nil, 0, false
	}
	covs := make([]otCoverage, n)
	for j := range n {
		subOff, ok := gposU16(data, base+j*2)
		if !ok {
			e.fail(errGPOSMalformed)
			return nil, 0, false
		}
		cov, ok := gposCoverage(data, int(subOff))
		if !ok {
			e.fail(errGPOSMalformed)
			return nil, 0, false
		}
		covs[j] = cov
	}
	return covs, base + n*2, true
}

// chainFormat3Matches evaluates input, backtrack and lookahead coverages at
// the candidate anchor pos.
func (e *gposEngine) chainFormat3Matches(btCovs, inCovs, laCovs []otCoverage, pos int) bool {
	ic, la := len(inCovs), len(laCovs)
	if pos < len(btCovs) || pos+ic+la > len(e.glyphs) {
		return false
	}
	if !e.coveragesMatch(inCovs, pos, 1) {
		return false
	}
	// Backtrack coverage j matches glyphs[pos-1-j].
	for j, cov := range btCovs {
		if _, covered := cov.contains(e.glyphs[pos-1-j].gid); !covered {
			return false
		}
	}
	laStart := pos + ic
	for j, cov := range laCovs {
		if _, covered := cov.contains(e.glyphs[laStart+j].gid); !covered {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Position-scoped entry points used by nested contextual lookups
// ---------------------------------------------------------------------------

// applyContextualAt evaluates a Type 7 subtable only at one buffer position.
func (e *gposEngine) applyContextualAt(data []byte, pos int) {
	// The format engines walk all positions; anchorOnly restricts them to pos.
	savedOnly, savedPos := e.anchorOnly, e.anchorPos
	e.anchorOnly, e.anchorPos = true, pos
	e.applyContextual(data)
	e.anchorOnly, e.anchorPos = savedOnly, savedPos
}

// applyChainingAt evaluates a Type 8 subtable only at one buffer position.
func (e *gposEngine) applyChainingAt(data []byte, pos int) {
	savedOnly, savedPos := e.anchorOnly, e.anchorPos
	e.anchorOnly, e.anchorPos = true, pos
	e.applyChaining(data)
	e.anchorOnly, e.anchorPos = savedOnly, savedPos
}
