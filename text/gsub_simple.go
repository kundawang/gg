// Simple GSUB lookup types — Single (1), Multiple (2), Alternate (3) and
// Ligature (4) — driven through the flag-aware contextual engine.
//
// Two entry shapes are needed:
//
//   - runSimpleLookup: feature-level application over the whole buffer,
//     honoring the lookup's flags (IgnoreMarks, mark filtering sets, ...).
//   - applySimpleAtAnchor: SubstLookupRecord recursion at one anchor
//     position. HarfBuzz's recursive dispatch bypasses the top-level
//     property gate, so only coverage decides whether the record fires;
//     flag-aware skipping still applies while walking ligature components.
package text

import "encoding/binary"

// runSimpleLookup applies types 1-4 over the full buffer.
func (e *gsubEngine) runSimpleLookup(lu *otLookup) bool {
	applied := false
	i := 0
	for i < len(e.glyphs) && e.lastErr == nil {
		gid := e.glyphs[i].gid
		// Top-level flag gate: skipped glyphs are passed over entirely.
		if !eligibleAtGate(lu, e.gdef, gid) {
			i++
			continue
		}
		consumed, did := e.applySimpleAt(lu, i)
		if did {
			applied = true
		}
		if consumed > 1 {
			// Ligature consumed component glyphs; stay on the result slot.
			i++
			continue
		}
		i++
	}
	return applied
}

// applySimpleAt applies every subtable of lu to the glyph at anchor.
// Returns the number of buffer slots consumed (1 for 1:1 substitutions)
// and whether any substitution happened. Subtable application is skipped
// once the operation budget is exhausted.
func (e *gsubEngine) applySimpleAt(lu *otLookup, anchor int) (int, bool) {
	appliedHere := false
	consumed := 1
	for _, st := range lu.subtables {
		if e.lastErr != nil {
			break
		}
		n, did := e.applySimpleSubtableAt(lu.lookupType, st, anchor)
		if did {
			appliedHere = true
			consumed = n
		}
	}
	return consumed, appliedHere
}

// applySimpleSubtableAt dispatches one subtable against an anchor.
func (e *gsubEngine) applySimpleSubtableAt(typ uint16, data []byte, anchor int) (int, bool) {
	if !e.charge() || len(data) < 6 {
		return 1, false
	}
	switch typ {
	case 1:
		return 1, e.applySingleAt(data, anchor)
	case 2:
		return 1, e.applyMultipleAt(data, anchor)
	case 3:
		return 1, e.applyAlternateAt(data, anchor)
	case 4:
		return e.applyLigatureAt(data, anchor)
	default:
		return 1, false
	}
}

// --- Type 1: Single ---

func (e *gsubEngine) applySingleAt(data []byte, anchor int) bool {
	format := binary.BigEndian.Uint16(data[0:2])
	covOff := int(binary.BigEndian.Uint16(data[2:4]))
	if covOff <= 0 || covOff >= len(data) {
		return false
	}
	cov := parseCoverage(data[covOff:])
	if cov == nil {
		return false
	}
	gid := e.glyphs[anchor].gid
	idx, ok := cov.contains(gid)
	if !ok {
		return false
	}
	switch format {
	case 1:
		delta := int16(binary.BigEndian.Uint16(data[4:6]))
		e.glyphs[anchor].gid = uint16(int32(gid) + int32(delta))
		return true
	case 2:
		count := int(binary.BigEndian.Uint16(data[4:6]))
		if idx >= count || len(data) < 6+count*2 {
			return false
		}
		e.glyphs[anchor].gid = binary.BigEndian.Uint16(data[6+idx*2 : 8+idx*2])
		return true
	default:
		return false
	}
}

// --- Type 2: Multiple ---

func (e *gsubEngine) applyMultipleAt(data []byte, anchor int) bool {
	if binary.BigEndian.Uint16(data[0:2]) != 1 {
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
	idx, ok := cov.contains(e.glyphs[anchor].gid)
	if !ok {
		return false
	}
	seqCount := int(binary.BigEndian.Uint16(data[4:6]))
	if idx >= seqCount || len(data) < 6+seqCount*2 {
		return false
	}
	seqOff := int(binary.BigEndian.Uint16(data[6+idx*2 : 8+idx*2]))
	if seqOff <= 0 || seqOff >= len(data) {
		return false
	}
	seq := data[seqOff:]
	if len(seq) < 2 {
		return false
	}
	subCount := int(binary.BigEndian.Uint16(seq[0:2]))
	if subCount == 0 || len(seq) < 2+subCount*2 {
		return false
	}
	cluster := e.glyphs[anchor].cluster
	out := make([]shapingGlyph, subCount)
	for j := range subCount {
		out[j] = shapingGlyph{
			gid:     binary.BigEndian.Uint16(seq[2+j*2 : 4+j*2]),
			cluster: cluster,
		}
	}
	e.glyphs = sliceReplace(e.glyphs, anchor, 1, out)
	return true
}

// --- Type 3: Alternate (always chooses alternate 0) ---

func (e *gsubEngine) applyAlternateAt(data []byte, anchor int) bool {
	if binary.BigEndian.Uint16(data[0:2]) != 1 {
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
	idx, ok := cov.contains(e.glyphs[anchor].gid)
	if !ok {
		return false
	}
	setCount := int(binary.BigEndian.Uint16(data[4:6]))
	if idx >= setCount || len(data) < 6+setCount*2 {
		return false
	}
	setOff := int(binary.BigEndian.Uint16(data[6+idx*2 : 8+idx*2]))
	if setOff <= 0 || setOff >= len(data) {
		return false
	}
	set := data[setOff:]
	if len(set) < 4 || binary.BigEndian.Uint16(set[0:2]) == 0 {
		return false
	}
	e.glyphs[anchor].gid = binary.BigEndian.Uint16(set[2:4])
	return true
}

// --- Type 4: Ligature ---

// applyLigatureAt attempts every Ligature in the coverage's LigatureSet at
// anchor. Component matching uses the flag-aware cursor, so marks skipped by
// IgnoreMarks do not block ligature formation. Returns slots consumed.
func (e *gsubEngine) applyLigatureAt(data []byte, anchor int) (int, bool) {
	setData, ok := e.openLigatureSet(data, anchor)
	if !ok {
		return 1, false
	}
	ligCount := int(binary.BigEndian.Uint16(setData[0:2]))
	if len(setData) < 2+ligCount*2 {
		return 1, false
	}

	for li := range ligCount {
		ligOff := int(binary.BigEndian.Uint16(setData[2+li*2 : 4+li*2]))
		if ligOff <= 0 || ligOff >= len(setData) {
			continue
		}
		if positions, ligGlyph, ok := e.matchLigatureEntry(setData[ligOff:], anchor); ok {
			e.collapseLigature(anchor, ligGlyph, positions)
			return len(positions), true
		}
	}
	return 1, false
}

// openLigatureSet validates the subtable header and returns the LigatureSet
// data selected by the anchor glyph's coverage index.
func (e *gsubEngine) openLigatureSet(data []byte, anchor int) ([]byte, bool) {
	if binary.BigEndian.Uint16(data[0:2]) != 1 {
		return nil, false
	}
	covOff := int(binary.BigEndian.Uint16(data[2:4]))
	if covOff <= 0 || covOff >= len(data) {
		return nil, false
	}
	cov := parseCoverage(data[covOff:])
	if cov == nil {
		return nil, false
	}
	idx, ok := cov.contains(e.glyphs[anchor].gid)
	if !ok {
		return nil, false
	}
	setCount := int(binary.BigEndian.Uint16(data[4:6]))
	if idx >= setCount || len(data) < 6+setCount*2 {
		return nil, false
	}
	ligSetOff := int(binary.BigEndian.Uint16(data[6+idx*2 : 8+idx*2]))
	if ligSetOff <= 0 || ligSetOff >= len(data) {
		return nil, false
	}
	setData := data[ligSetOff:]
	if len(setData) < 2 {
		return nil, false
	}
	return setData, true
}

// matchLigatureEntry checks one Ligature record and returns the matched
// component positions (including the anchor) on success.
func (e *gsubEngine) matchLigatureEntry(lig []byte, anchor int) ([]int, uint16, bool) {
	if len(lig) < 4 {
		return nil, 0, false
	}
	ligGlyph := binary.BigEndian.Uint16(lig[0:2])
	compCount := int(binary.BigEndian.Uint16(lig[2:4]))
	if compCount < 2 || compCount > gsubMaxContextLen ||
		len(lig) < 4+(compCount-1)*2 {
		return nil, 0, false
	}
	positions := make([]int, 1, compCount)
	positions[0] = anchor
	cur := anchor
	lu := e.flagCarrier
	for j := 1; j < compCount; j++ {
		want := binary.BigEndian.Uint16(lig[4+(j-1)*2 : 6+(j-1)*2])
		next := e.nextGlyph(lu, cur)
		if next < 0 || e.glyphs[next].gid != want {
			return nil, 0, false
		}
		positions = append(positions, next)
		cur = next
	}
	return positions, ligGlyph, true
}

// collapseLigature writes ligGlyph at anchor and removes the other component
// slots, keeping any skipped marks in place while merging clusters.
func (e *gsubEngine) collapseLigature(anchor int, ligGlyph uint16, positions []int) {
	last := positions[len(positions)-1]
	cluster := mergeClusters(e.glyphs, anchor, last+1)
	e.glyphs[anchor].gid = ligGlyph
	e.glyphs[anchor].cluster = cluster
	for k := len(positions) - 1; k >= 1; k-- {
		e.glyphs = sliceReplace(e.glyphs, positions[k], 1, nil)
	}
}
