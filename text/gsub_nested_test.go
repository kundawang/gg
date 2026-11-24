package text

import (
	"encoding/binary"
	"testing"
)

// buildChainF1Bytes builds a ChainContextSubst format 1 subtable: anchor 10,
// backtrack [backGid], input rest [inGid], lookahead [laGid], with one
// SubstLookupRecord (sequence index 0 -> nestedLookup).
func buildChainF1Bytes(t *testing.T, backGid, inGid, laGid uint16, nestedLookup uint16) []byte {
	t.Helper()
	// Layout:
	//  [0:6]   header (format, coverageOff, ruleSetCount)
	//  [6:10]  ruleSetOff
	//  [10:14] RuleSet (ruleCount, ruleOff)
	//  [14:..] ChainSubRule
	//  after   Coverage
	// ChainSubRule: btCount(2) backtrack(2) inCount(2) input(2) laCount(2)
	// lookahead(2) substCount(2) record(4) = 18 bytes.
	const (
		ruleSetEnd = 14
		ruleLen    = 18
	)
	covOff := ruleSetEnd + ruleLen
	data := make([]byte, covOff+6)
	binary.BigEndian.PutUint16(data[0:2], 1) // chain format 1
	binary.BigEndian.PutUint16(data[2:4], uint16(covOff))
	binary.BigEndian.PutUint16(data[4:6], 1) // ruleSetCount
	binary.BigEndian.PutUint16(data[6:8], 10)
	binary.BigEndian.PutUint16(data[10:12], 1) // ruleCount
	binary.BigEndian.PutUint16(data[12:14], 4) // ruleOff within RuleSet
	r := data[ruleSetEnd:]
	binary.BigEndian.PutUint16(r[0:2], 1) // backtrackGlyphCount
	binary.BigEndian.PutUint16(r[2:4], backGid)
	binary.BigEndian.PutUint16(r[4:6], 2) // inputGlyphCount (anchor + 1)
	binary.BigEndian.PutUint16(r[6:8], inGid)
	binary.BigEndian.PutUint16(r[8:10], 1) // lookAheadGlyphCount
	binary.BigEndian.PutUint16(r[10:12], laGid)
	binary.BigEndian.PutUint16(r[12:14], 1) // substCount
	binary.BigEndian.PutUint16(r[14:16], 0) // sequenceIndex
	binary.BigEndian.PutUint16(r[16:18], nestedLookup)
	// Coverage: anchor 10.
	c := data[covOff:]
	binary.BigEndian.PutUint16(c[0:2], 1)
	binary.BigEndian.PutUint16(c[2:4], 1)
	binary.BigEndian.PutUint16(c[4:6], 10)
	return data
}

// TestGSUBNested_ChainAndReverse covers nested invocation of a type 6 chain
// lookup and a type 8 reverse lookup through SubstLookupRecords.
func TestGSUBNested_ChainAndReverse(t *testing.T) {
	// L0: single 10 -> 100 (format 2).
	single := buildSingleSubst102like(10, 100)

	// L1: chain F1 — backtrack 20, input [10,11], lookahead 21; record seq0 -> L0.
	chain := buildChainF1Bytes(t, 20, 11, 21, 0)

	// L2: context F1 on anchor 10, rule [10,11], record seq0 -> L1 (nested chain).
	contextSt := buildContextF1Bytes(t, []uint16{10, 11}, []uint16{0, 1})

	g := newMinimalGSUB([]otLookup{
		{lookupType: 1, subtables: [][]byte{single}},
		{lookupType: 6, subtables: [][]byte{chain}},
		{lookupType: 5, subtables: [][]byte{contextSt}},
	})

	t.Run("nested_chain_fires", func(t *testing.T) {
		e := &gsubEngine{
			table:  g,
			glyphs: []shapingGlyph{{gid: 20}, {gid: 10}, {gid: 11}, {gid: 21}},
		}
		e.applyLookupIndex(2)
		if e.lastErr != nil {
			t.Fatalf("unexpected error: %v", e.lastErr)
		}
		want := []uint16{20, 100, 11, 21}
		got := glyphIDs(e.glyphs)
		if !uint16Equal(got, want) {
			t.Fatalf("nested chain: got %v, want %v", got, want)
		}
	})

	t.Run("nested_chain_negative", func(t *testing.T) {
		// Wrong lookahead (22 instead of 21): the nested chain must not fire.
		e := &gsubEngine{
			table:  g,
			glyphs: []shapingGlyph{{gid: 20}, {gid: 10}, {gid: 11}, {gid: 22}},
		}
		e.applyLookupIndex(2)
		want := []uint16{20, 10, 11, 22}
		got := glyphIDs(e.glyphs)
		if !uint16Equal(got, want) {
			t.Fatalf("nested chain negative: got %v, want %v", got, want)
		}
	})
}

// buildReverseF1Bytes builds a ReverseChainSingleSubst format 1 subtable:
// anchor 10 with backtrack [backGid] and no lookahead, substituted to 100.
func buildReverseF1Bytes(t *testing.T, backGid, substitute uint16) []byte {
	t.Helper()
	// Layout: [0:6] header, [6:8] btCount, [8:10] btCovOff, [10:12]
	// laCount(0), [12:14] glyphCount, [14:16] substitute, then coverages.
	const (
		btCovOff  = 16
		anchorCov = 22
	)
	data := make([]byte, anchorCov+6)
	binary.BigEndian.PutUint16(data[0:2], 1) // reverse format 1
	binary.BigEndian.PutUint16(data[2:4], anchorCov)
	binary.BigEndian.PutUint16(data[4:6], 1) // backtrackGlyphCount
	binary.BigEndian.PutUint16(data[6:8], btCovOff)
	binary.BigEndian.PutUint16(data[8:10], 0) // lookAheadGlyphCount
	binary.BigEndian.PutUint16(data[10:12], 1)
	binary.BigEndian.PutUint16(data[12:14], substitute)
	// Backtrack coverage.
	binary.BigEndian.PutUint16(data[btCovOff:btCovOff+2], 1)
	binary.BigEndian.PutUint16(data[btCovOff+2:btCovOff+4], 1)
	binary.BigEndian.PutUint16(data[btCovOff+4:btCovOff+6], backGid)
	// Anchor coverage.
	c := data[anchorCov:]
	binary.BigEndian.PutUint16(c[0:2], 1)
	binary.BigEndian.PutUint16(c[2:4], 1)
	binary.BigEndian.PutUint16(c[4:6], 10)
	return data
}

// TestGSUBNested_Reverse covers a reverse chain lookup reached through a
// SubstLookupRecord (nested type 8 at a single anchor).
func TestGSUBNested_Reverse(t *testing.T) {
	// L0: reverse: 10 with backtrack 20 -> 100.
	reverse := buildReverseF1Bytes(t, 20, 100)
	// L1: context F1 on 10, rule [10,11], record seq0 -> L0.
	contextSt := buildContextF1Bytes(t, []uint16{10, 11}, []uint16{0, 0})

	g := newMinimalGSUB([]otLookup{
		{lookupType: 8, subtables: [][]byte{reverse}},
		{lookupType: 5, subtables: [][]byte{contextSt}},
	})

	t.Run("nested_reverse_fires", func(t *testing.T) {
		e := &gsubEngine{
			table:  g,
			glyphs: []shapingGlyph{{gid: 20}, {gid: 10}, {gid: 11}},
		}
		e.applyLookupIndex(1)
		want := []uint16{20, 100, 11}
		if got := glyphIDs(e.glyphs); !uint16Equal(got, want) {
			t.Fatalf("nested reverse: got %v, want %v", got, want)
		}
	})

	t.Run("nested_reverse_negative", func(t *testing.T) {
		e := &gsubEngine{
			table:  g,
			glyphs: []shapingGlyph{{gid: 10}, {gid: 11}},
		}
		e.applyLookupIndex(1)
		want := []uint16{10, 11}
		if got := glyphIDs(e.glyphs); !uint16Equal(got, want) {
			t.Fatalf("nested reverse negative: got %v, want %v", got, want)
		}
	})
}

// buildSingleSubst102like builds a Type 1 format-2 subtable mapping from -> to.
func buildSingleSubst102like(from, to uint16) []byte {
	data := make([]byte, 16)
	binary.BigEndian.PutUint16(data[0:2], 2) // format 2
	binary.BigEndian.PutUint16(data[2:4], 10)
	binary.BigEndian.PutUint16(data[4:6], 1) // glyphCount
	binary.BigEndian.PutUint16(data[6:8], to)
	binary.BigEndian.PutUint16(data[10:12], 1)
	binary.BigEndian.PutUint16(data[12:14], 1)
	binary.BigEndian.PutUint16(data[14:16], from)
	return data
}

// glyphIDs projects the glyph IDs out of a shaping buffer.
func glyphIDs(glyphs []shapingGlyph) []uint16 {
	out := make([]uint16, len(glyphs))
	for i, gl := range glyphs {
		out[i] = gl.gid
	}
	return out
}
