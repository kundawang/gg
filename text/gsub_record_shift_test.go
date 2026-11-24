package text

import (
	"encoding/binary"
	"testing"
)

// buildMultipleSubst builds a GSUB Type 2 subtable mapping anchorGid to the
// supplied replacement glyph sequence.
func buildMultipleSubst(t *testing.T, anchorGid uint16, replacements []uint16) []byte {
	t.Helper()
	// Sections: [0:6] header [6:8] seqOff [8:..] sequence, then coverage.
	const (
		seqOff   = 8 // offset of Sequence table
		seqStart = 8
	)
	seqLen := 2 + 2*len(replacements)
	covOff := seqStart + seqLen
	data := make([]byte, covOff+6)
	binary.BigEndian.PutUint16(data[0:2], 1) // format 1
	binary.BigEndian.PutUint16(data[2:4], uint16(covOff))
	binary.BigEndian.PutUint16(data[4:6], 1) // sequenceCount
	binary.BigEndian.PutUint16(data[6:8], seqOff)
	binary.BigEndian.PutUint16(data[seqStart:seqStart+2], uint16(len(replacements)))
	for i, gid := range replacements {
		binary.BigEndian.PutUint16(data[seqStart+2+i*2:seqStart+4+i*2], gid)
	}
	c := data[covOff:]
	binary.BigEndian.PutUint16(c[0:2], 1)
	binary.BigEndian.PutUint16(c[2:4], 1)
	binary.BigEndian.PutUint16(c[4:6], anchorGid)
	return data
}

// TestGSUBRecord_PositionShiftAfterExpansion verifies that a SubstLookupRecord
// targeting a later input position still lands correctly after an earlier
// record expanded the buffer via a Multiple substitution.
func TestGSUBRecord_PositionShiftAfterExpansion(t *testing.T) {
	// L0: glyph 10 -> [100, 101] (type 2, expands the buffer).
	// L1: glyph 11 -> 102 (type 1 format 2).
	multi := buildMultipleSubst(t, 10, []uint16{100, 101})
	single := buildSingleSubst102()

	// L2: Context F1 on anchor 10, rule input [10, 11], records:
	//   seq 0 -> L0 (expansion), seq 1 -> L1 (must hit the original 11).
	ruleData := buildContextF1Bytes(t, []uint16{10, 11},
		[]uint16{0, 0, 1, 1})

	g := newMinimalGSUB([]otLookup{
		{lookupType: 2, subtables: [][]byte{multi}},
		{lookupType: 1, subtables: [][]byte{single}},
		{lookupType: 5, subtables: [][]byte{ruleData}},
	})
	e := &gsubEngine{
		table:  g,
		glyphs: []shapingGlyph{{gid: 10, cluster: 0}, {gid: 11, cluster: 1}},
	}
	e.applyLookupIndex(2)
	if e.lastErr != nil {
		t.Fatalf("unexpected error: %v", e.lastErr)
	}
	// Expected: 10 -> 100,101 and the original second glyph 11 -> 102.
	want := []uint16{100, 101, 102}
	got := make([]uint16, len(e.glyphs))
	for i, gl := range e.glyphs {
		got[i] = gl.gid
	}
	if !uint16Equal(got, want) {
		t.Fatalf("record position shift: got %v, want %v", got, want)
	}
	// All output glyphs must carry valid source clusters.
	for i, gl := range e.glyphs {
		if gl.cluster < 0 || gl.cluster > 1 {
			t.Errorf("glyph %d has implausible cluster %d", i, gl.cluster)
		}
	}
}

// buildSingleSubst102 builds a Type 1 format-2 subtable: 11 -> 102.
func buildSingleSubst102() []byte {
	// Layout: [0:10] header + substitute array, [10:16] coverage.
	data := make([]byte, 16)
	binary.BigEndian.PutUint16(data[0:2], 2) // format 2
	binary.BigEndian.PutUint16(data[2:4], 10)
	binary.BigEndian.PutUint16(data[4:6], 1) // glyphCount
	binary.BigEndian.PutUint16(data[6:8], 102)
	binary.BigEndian.PutUint16(data[10:12], 1)
	binary.BigEndian.PutUint16(data[12:14], 1)
	binary.BigEndian.PutUint16(data[14:16], 11)
	return data
}

// TestGSUBRecord_LigatureThenRecord verifies contraction shifting: a ligature
// in record 0 removes a slot, and record 1 targeting a later sequence index
// still resolves to the surviving glyph.
func TestGSUBRecord_LigatureThenRecord(t *testing.T) {
	// L0: ligature 10 + 11 -> 120.
	lig := buildLigatureSubst10x11()
	// L1: single 12 -> 130.
	single := buildSingleSubst12to130()
	// L2: context on 10, rule [10, 11, 12], records seq0->L0, seq2->L1.
	ruleData := buildContextF1Bytes(t, []uint16{10, 11, 12},
		[]uint16{0, 0, 2, 1})

	g := newMinimalGSUB([]otLookup{
		{lookupType: 4, subtables: [][]byte{lig}},
		{lookupType: 1, subtables: [][]byte{single}},
		{lookupType: 5, subtables: [][]byte{ruleData}},
	})
	e := &gsubEngine{
		table:  g,
		glyphs: []shapingGlyph{{gid: 10}, {gid: 11}, {gid: 12}},
	}
	e.applyLookupIndex(2)
	want := []uint16{120, 130}
	got := make([]uint16, len(e.glyphs))
	for i, gl := range e.glyphs {
		got[i] = gl.gid
	}
	if !uint16Equal(got, want) {
		t.Fatalf("ligature contraction shift: got %v, want %v", got, want)
	}
}

// buildLigatureSubst10x11 builds a Type 4 subtable: 10 + 11 -> 120.
func buildLigatureSubst10x11() []byte {
	// [0:6] header, [6:8] ligSetOff=8, [8:12] set, [12:20] ligature,
	// [20:26] coverage.
	data := make([]byte, 26)
	binary.BigEndian.PutUint16(data[0:2], 1)
	binary.BigEndian.PutUint16(data[2:4], 20) // coverage offset
	binary.BigEndian.PutUint16(data[4:6], 1)
	binary.BigEndian.PutUint16(data[6:8], 8)
	// LigatureSet at 8.
	binary.BigEndian.PutUint16(data[8:10], 1)
	binary.BigEndian.PutUint16(data[10:12], 4)
	// Ligature at 12.
	binary.BigEndian.PutUint16(data[12:14], 120) // lig glyph
	binary.BigEndian.PutUint16(data[14:16], 2)   // comp count
	binary.BigEndian.PutUint16(data[16:18], 11)  // second component
	// Coverage at 20.
	binary.BigEndian.PutUint16(data[20:22], 1)
	binary.BigEndian.PutUint16(data[22:24], 1)
	binary.BigEndian.PutUint16(data[24:26], 10)
	return data
}

// buildSingleSubst12to130 builds a Type 1 format-2 subtable: 12 -> 130.
func buildSingleSubst12to130() []byte {
	data := make([]byte, 16)
	binary.BigEndian.PutUint16(data[0:2], 2)
	binary.BigEndian.PutUint16(data[2:4], 10)
	binary.BigEndian.PutUint16(data[4:6], 1)
	binary.BigEndian.PutUint16(data[6:8], 130)
	binary.BigEndian.PutUint16(data[10:12], 1)
	binary.BigEndian.PutUint16(data[12:14], 1)
	binary.BigEndian.PutUint16(data[14:16], 12)
	return data
}
