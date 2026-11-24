package text

import (
	"encoding/binary"
	"testing"
)

// buildContextF1Bytes assembles a minimal ContextSubst format 1 subtable
// anchored on glyph 10, with one rule matching inputGlyphs and applying
// records. records is a flat sequence of (sequenceIndex, lookupIndex) pairs.
func buildContextF1Bytes(t *testing.T, inputGlyphs, records []uint16) []byte {
	t.Helper()
	const anchorGid uint16 = 10
	// Fixed section layout:
	//   [0:6]   format, coverageOff, ruleSetCount
	//   [6:10]  ruleSetOff
	//   [10:14] RuleSet: ruleCount, ruleOff
	//   [14:..] SubRule
	//   after   Coverage (format 1, single anchor glyph)
	const (
		headerEnd  = 6
		ruleSetOff = 10 // offset of the RuleSet from the subtable start
		ruleSetEnd = 14
		ruleOff    = 4 // offset of the SubRule relative to the RuleSet
	)
	ruleLen := 4 + (len(inputGlyphs)-1)*2 + len(records)*2
	covOff := ruleSetEnd + ruleLen

	total := covOff + 6
	data := make([]byte, total)
	binary.BigEndian.PutUint16(data[0:2], 1) // context format 1
	binary.BigEndian.PutUint16(data[2:4], uint16(covOff))
	binary.BigEndian.PutUint16(data[4:6], 1) // ruleSetCount
	binary.BigEndian.PutUint16(data[6:8], uint16(ruleSetOff))
	// RuleSet.
	binary.BigEndian.PutUint16(data[10:12], 1) // ruleCount
	binary.BigEndian.PutUint16(data[12:14], uint16(ruleOff))
	// SubRule.
	r := data[ruleSetEnd:]
	binary.BigEndian.PutUint16(r[0:2], uint16(len(inputGlyphs)))
	binary.BigEndian.PutUint16(r[2:4], uint16(len(records)/2))
	pos := 4
	for i := 1; i < len(inputGlyphs); i++ {
		binary.BigEndian.PutUint16(r[pos:pos+2], inputGlyphs[i])
		pos += 2
	}
	for i := 0; i < len(records); i += 2 {
		binary.BigEndian.PutUint16(r[pos:pos+2], records[i])
		binary.BigEndian.PutUint16(r[pos+2:pos+4], records[i+1])
		pos += 4
	}
	// Coverage format 1 with one anchor glyph.
	c := data[covOff:]
	binary.BigEndian.PutUint16(c[0:2], 1)
	binary.BigEndian.PutUint16(c[2:4], 1)
	binary.BigEndian.PutUint16(c[4:6], anchorGid)
	return data
}

// newMinimalGSUB builds a gsubTable whose lookups are the supplied entries.
// Lookup i is addressable by index i; subtables are used verbatim.
func newMinimalGSUB(lookups []otLookup) *gsubTable {
	return &gsubTable{lookups: lookups}
}

// TestGSUBHardening_BadOffsets covers truncated/garbage subtables and a bad
// nested lookup index: every path must stop safely without touching the
// buffer or panicking.
func TestGSUBHardening_BadOffsets(t *testing.T) {
	base := []shapingGlyph{{gid: 10, cluster: 0}, {gid: 11, cluster: 1}}

	cases := []struct {
		name string
		typ  uint16
		data []byte
	}{
		{"truncated_header", 5, []byte{0, 1, 0}},
		{"context_bad_coverage_off", 5, []byte{0, 1, 0xFF, 0xFF, 0, 1, 0, 6}},
		{"context_unknown_format", 5, []byte{0, 99, 0, 6, 0, 1, 0, 6}},
		{"chain_bad_coverage_off", 6, []byte{0, 1, 0xFF, 0xFE, 0, 0, 0, 0}},
		{"chain_unknown_format", 6, []byte{0, 7, 0, 4, 0, 0}},
		{"reverse_bad_coverage_off", 8, []byte{0, 1, 0xFF, 0xFD, 0, 0}},
		{"reverse_unknown_format", 8, []byte{0, 5, 0, 4, 0, 0}},
		{"single_truncated", 1, []byte{0, 1, 0, 6, 0, 5}},
		{"ligature_truncated", 4, []byte{0, 1, 0, 6, 0, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newMinimalGSUB([]otLookup{{
				lookupType: tc.typ,
				subtables:  [][]byte{tc.data},
			}})
			e := &gsubEngine{table: g, glyphs: append([]shapingGlyph(nil), base...)}
			// Must not panic and must leave the buffer intact.
			e.applyLookupIndex(0)
			if len(e.glyphs) != len(base) {
				t.Fatalf("buffer length changed: got %d, want %d", len(e.glyphs), len(base))
			}
			for i := range base {
				if e.glyphs[i].gid != base[i].gid {
					t.Fatalf("glyph %d mutated: got %d, want %d", i, e.glyphs[i].gid, base[i].gid)
				}
			}
		})
	}

	t.Run("nested_bad_lookup_index", func(t *testing.T) {
		// One record: sequence index 0, lookup index 9 (out of range).
		ruleData := buildContextF1Bytes(t, []uint16{10, 11},
			[]uint16{0, 9})
		g := newMinimalGSUB([]otLookup{{
			lookupType: 5,
			subtables:  [][]byte{ruleData},
		}})
		e := &gsubEngine{table: g, glyphs: append([]shapingGlyph(nil), base...)}
		e.applyLookupIndex(0)
		if e.lastErr != nil {
			t.Fatalf("bad nested index should be a safe skip, not error: %v", e.lastErr)
		}
		if e.glyphs[0].gid != 10 || e.glyphs[1].gid != 11 {
			t.Fatalf("buffer mutated by invalid nested lookup: %+v", e.glyphs)
		}
	})

	t.Run("extension_bad_offset", func(t *testing.T) {
		st := make([]byte, 8)
		binary.BigEndian.PutUint16(st[0:2], 1) // extension format 1
		binary.BigEndian.PutUint16(st[2:4], 1) // wraps a single subst
		binary.BigEndian.PutUint32(st[4:8], 0xFFFFFF)
		g := newMinimalGSUB([]otLookup{{lookupType: 7, subtables: [][]byte{st}}})
		e := &gsubEngine{table: g, glyphs: append([]shapingGlyph(nil), base...)}
		e.applyLookupIndex(0)
		if e.glyphs[0].gid != 10 {
			t.Fatal("extension with bad offset must not modify the buffer")
		}
	})
}

// TestGSUBHardening_EmptyCoverage verifies that coverages containing no
// glyphs never match and never error.
func TestGSUBHardening_EmptyCoverage(t *testing.T) {
	// Context F3 with two empty coverage tables.
	cov1 := []byte{0, 1, 0, 0} // coverage format 1, zero glyphs
	cov2 := []byte{0, 1, 0, 0}
	headerLen := 6 + 2*2
	data := make([]byte, 0, headerLen+len(cov1)+len(cov2))
	header := make([]byte, headerLen)
	binary.BigEndian.PutUint16(header[0:2], 3) // context format 3
	binary.BigEndian.PutUint16(header[2:4], 2) // glyphCount
	binary.BigEndian.PutUint16(header[4:6], 0) // substCount
	binary.BigEndian.PutUint16(header[6:8], uint16(headerLen))
	binary.BigEndian.PutUint16(header[8:10], uint16(headerLen+len(cov1)))
	data = append(data, header...)
	data = append(data, cov1...)
	data = append(data, cov2...)

	g := newMinimalGSUB([]otLookup{{lookupType: 5, subtables: [][]byte{data}}})
	e := &gsubEngine{
		table:  g,
		glyphs: []shapingGlyph{{gid: 10}, {gid: 11}},
	}
	e.applyLookupIndex(0)
	if e.lastErr != nil {
		t.Fatalf("empty coverage must not error: %v", e.lastErr)
	}
	if e.glyphs[0].gid != 10 || e.glyphs[1].gid != 11 {
		t.Fatal("empty coverage must leave glyphs unchanged")
	}
}

// TestGSUBHardening_OversizedContext makes sure implausibly large rule
// counts are rejected by the context-length guard instead of consuming huge
// memory or running away.
func TestGSUBHardening_OversizedContext(t *testing.T) {
	// Context F1 rule claiming 60000 input glyphs.
	ruleData := buildContextF1Bytes(t, []uint16{10, 11}, []uint16{0, 0})
	// Corrupt the SubRule glyph count (SubRule starts at byte 14).
	binary.BigEndian.PutUint16(ruleData[14:16], 60000)
	g := newMinimalGSUB([]otLookup{{lookupType: 5, subtables: [][]byte{ruleData}}})
	e := &gsubEngine{
		table:  g,
		glyphs: []shapingGlyph{{gid: 10}, {gid: 11}},
	}
	e.applyLookupIndex(0)
	if len(e.glyphs) != 2 {
		t.Fatalf("oversized rule must not alter the buffer, got %d glyphs", len(e.glyphs))
	}
}

// TestGSUBHardening_MaxOps exercises the operation budget with a
// multiple-substitution lookup that grows the buffer on every pass.
func TestGSUBHardening_MaxOps(t *testing.T) {
	// Single-subst subtable is enough: a self-recursing chain would need
	// nested lookups, so drive the budget directly through charge().
	e := &gsubEngine{glyphs: []shapingGlyph{{gid: 1}}}
	for range gsubMaxOps {
		if !e.charge() {
			t.Fatal("budget should allow exactly gsubMaxOps charges")
		}
	}
	if e.charge() {
		t.Fatal("charge must return false once the budget is exhausted")
	}
	if e.lastErr == nil {
		t.Fatal("exhausting the budget must record an error")
	}
}

// TestGDEF_Parsing verifies glyph classes and mark filtering sets.
func TestGDEF_Parsing(t *testing.T) {
	// GlyphClassDef format 1: startGlyph=10, classes [1, 3, 0, 3].
	classDef := []byte{
		0, 1, // format 1
		0, 10, // startGlyphID
		0, 4, // glyphCount
		0, 1, 0, 3, 0, 0, 0, 3,
	}
	// One mark set covering glyph 11.
	cov := []byte{0, 1, 0, 1, 0, 11}
	markSets := make([]byte, 0, 8+len(cov))
	header := make([]byte, 8)
	binary.BigEndian.PutUint16(header[0:2], 1) // format
	binary.BigEndian.PutUint16(header[2:4], 1) // set count
	binary.BigEndian.PutUint32(header[4:8], 8) // coverage offset
	markSets = append(markSets, header...)
	markSets = append(markSets, cov...)

	gdefData := make([]byte, 0, 14+len(classDef)+len(markSets))
	gdefHeader := make([]byte, 14)
	binary.BigEndian.PutUint16(gdefHeader[0:2], 1)
	binary.BigEndian.PutUint16(gdefHeader[2:4], 2) // version 1.2
	binary.BigEndian.PutUint16(gdefHeader[4:6], 14)
	binary.BigEndian.PutUint16(gdefHeader[12:14], 14+uint16(len(classDef)))
	gdefData = append(gdefData, gdefHeader...)
	gdefData = append(gdefData, classDef...)
	gdefData = append(gdefData, markSets...)

	gdef := parseGDEF(gdefData)
	if gdef == nil {
		t.Fatal("parseGDEF returned nil")
	}
	if !gdef.isMark(11) || !gdef.isMark(13) {
		t.Error("gid 11 and 13 must be marks")
	}
	if gdef.isMark(10) {
		t.Error("gid 10 is a base, not a mark")
	}
	if !gdef.markSetCovers(0, 11) {
		t.Error("mark set 0 must cover gid 11")
	}
	if gdef.markSetCovers(0, 13) {
		t.Error("mark set 0 must not cover gid 13")
	}
	if gdef.markSetCovers(5, 11) {
		t.Error("out-of-range mark set index must never match")
	}
}

// TestGDEF_NilSafety ensures a missing GDEF never panics and classifies
// nothing as a mark.
func TestGDEF_NilSafety(t *testing.T) {
	var gdef *gdefTable
	if gdef.isMark(42) {
		t.Error("nil GDEF must report no marks")
	}
	if gdef.markSetCovers(0, 42) {
		t.Error("nil GDEF mark set must never match")
	}
	if gdef.glyphClass(42) != 0 {
		t.Error("nil GDEF glyph class must be 0")
	}
}
