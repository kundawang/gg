// Tests for the OpenType LookupList parsing layer (ot_lookup.go).
//
// Uses hand-built binary fixtures (valid, truncated, out-of-range, and
// huge-count variants) plus real fonts from testdata to verify:
//
//   - glyph results for valid multi-subtable lookups (golden comparison)
//   - error context: table type, lookup index, subtable index, byte range
//   - strict vs lenient parse modes
//   - parse determinism and input immutability
//   - no recursion/stack overflow on deeply corrupt extension tables
package text

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// binBuilder appends big-endian values to build binary fixtures.
type binBuilder struct{ b []byte }

func (bb *binBuilder) u16(v uint16) { bb.b = append(bb.b, byte(v>>8), byte(v)) }

func (bb *binBuilder) u32(v uint32) {
	bb.b = append(bb.b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func (bb *binBuilder) bytes() []byte { return bb.b }

// patch16 overwrites the big-endian uint16 at absolute offset at.
func patch16(data []byte, at int, v uint16) {
	data[at] = byte(v >> 8)
	data[at+1] = byte(v)
}

// buildValidGSUBFixture builds a valid GSUB table with two lookups:
//
//	lookup 0: type 1 (SingleSubst), 2 subtables
//	  subtable A: format 1, coverage [1,2,3], delta +10
//	  subtable B: format 2, coverage [5], substitutes [50]
//	lookup 1: type 1, flag UseMarkFilteringSet (markFilteringSet=3), 1 subtable
//	  format 1, coverage [7], delta +100
//
// Absolute offsets referenced by tests:
//
//	14  LookupList start
//	18  lookupOffset[1] in the LookupList offset array
//	20  lookup 0 start
//	24  lookup 0 subtableCount
//	28  lookup 0 subtable offset[1]
//	60  lookup 1 start
func buildValidGSUBFixture() []byte {
	bb := &binBuilder{}
	bb.u16(1)      // majorVersion
	bb.u16(0)      // minorVersion
	bb.u16(10)     // scriptListOffset
	bb.u16(12)     // featureListOffset
	bb.u16(14)     // lookupListOffset
	bb.u16(0)      // ScriptList: count=0
	bb.u16(0)      // FeatureList: count=0
	bb.u16(2)      // LookupList: lookupCount=2
	bb.u16(6)      //   lookupOffset[0] → abs 20
	bb.u16(46)     //   lookupOffset[1] → abs 60
	bb.u16(1)      // lookup0: type=1 (SingleSubst)
	bb.u16(0)      //   flag=0
	bb.u16(2)      //   subtableCount=2
	bb.u16(10)     //   subtableOffset[0] → abs 30
	bb.u16(26)     //   subtableOffset[1] → abs 46
	bb.u16(1)      // subtable A: SingleSubst format 1
	bb.u16(6)      //   coverageOffset=6
	bb.u16(10)     //   deltaGlyphID=+10
	bb.u16(1)      //   coverage format 1
	bb.u16(3)      //   glyphCount=3
	bb.u16(1)      //   glyphs[0]
	bb.u16(2)      //   glyphs[1]
	bb.u16(3)      //   glyphs[2]
	bb.u16(2)      // subtable B: SingleSubst format 2
	bb.u16(8)      //   coverageOffset=8
	bb.u16(1)      //   glyphCount=1
	bb.u16(50)     //   substitute[0]=50
	bb.u16(1)      //   coverage format 1
	bb.u16(1)      //   glyphCount=1
	bb.u16(5)      //   glyphs[0]=5
	bb.u16(1)      // lookup1: type=1 (SingleSubst)
	bb.u16(0x0010) //   flag=UseMarkFilteringSet
	bb.u16(1)      //   subtableCount=1
	bb.u16(10)     //   subtableOffset[0] → abs 70
	bb.u16(3)      //   markFilteringSet=3
	bb.u16(1)      // subtable: SingleSubst format 1
	bb.u16(6)      //   coverageOffset=6
	bb.u16(100)    //   deltaGlyphID=+100
	bb.u16(1)      //   coverage format 1
	bb.u16(1)      //   glyphCount=1
	bb.u16(7)      //   glyphs[0]=7
	return bb.bytes()
}

// buildValidGPOSFixture builds a valid GPOS table with one lookup:
//
//	lookup 0: type 1 (SinglePos) format 1, valueFormat=xAdvance,
//	          coverage [1], xAdvance=-30
func buildValidGPOSFixture() []byte {
	bb := &binBuilder{}
	bb.u16(1)      // majorVersion
	bb.u16(0)      // minorVersion
	bb.u16(10)     // scriptListOffset
	bb.u16(12)     // featureListOffset
	bb.u16(14)     // lookupListOffset
	bb.u16(0)      // ScriptList: count=0
	bb.u16(0)      // FeatureList: count=0
	bb.u16(1)      // LookupList: lookupCount=1
	bb.u16(4)      //   lookupOffset[0] → abs 18
	bb.u16(1)      // lookup0: type=1 (SinglePos)
	bb.u16(0)      //   flag=0
	bb.u16(1)      //   subtableCount=1
	bb.u16(8)      //   subtableOffset[0] → abs 26
	bb.u16(1)      // subtable: SinglePos format 1
	bb.u16(8)      //   coverageOffset=8
	bb.u16(0x0004) //   valueFormat=xAdvance
	bb.u16(0xFFE2) //   xAdvance=-30
	bb.u16(1)      //   coverage format 1
	bb.u16(1)      //   glyphCount=1
	bb.u16(1)      //   glyphs[0]=1
	return bb.bytes()
}

func glyphIDs(glyphs []shapingGlyph) []uint16 {
	ids := make([]uint16, len(glyphs))
	for i, g := range glyphs {
		ids[i] = g.gid
	}
	return ids
}

// TestOTLookupParseValidFixture verifies structural parsing of a valid
// multi-subtable GSUB fixture in both modes, including retention of the
// UseMarkFilteringSet extension field.
func TestOTLookupParseValidFixture(t *testing.T) {
	data := buildValidGSUBFixture()

	strict, err := parseGSUBMode(data, otParseStrict)
	if err != nil {
		t.Fatalf("strict parse of valid fixture failed: %v", err)
	}
	if strict == nil {
		t.Fatal("strict parse returned nil table")
	}
	if len(strict.lookups) != 2 {
		t.Fatalf("got %d lookups, want 2", len(strict.lookups))
	}
	if len(strict.parseErrs) != 0 {
		t.Fatalf("valid fixture produced parse errors: %v", strict.parseErrs)
	}

	lu0 := strict.lookups[0]
	if lu0.lookupType != 1 || lu0.lookupFlag != 0 {
		t.Errorf("lookup0 header = type %d flag %#x, want type 1 flag 0", lu0.lookupType, lu0.lookupFlag)
	}
	if len(lu0.subtables) != 2 {
		t.Fatalf("lookup0 has %d subtables, want 2", len(lu0.subtables))
	}
	if lu0.hasMarkFilteringSet {
		t.Error("lookup0 must not have markFilteringSet")
	}

	lu1 := strict.lookups[1]
	if !lu1.hasMarkFilteringSet || lu1.markFilteringSet != 3 {
		t.Errorf("lookup1 markFilteringSet = (%v, %d), want (true, 3)",
			lu1.hasMarkFilteringSet, lu1.markFilteringSet)
	}

	// Lenient mode must produce an identical result for a valid table.
	lenient := parseGSUB(data)
	if !reflect.DeepEqual(strict.lookups, lenient.lookups) {
		t.Error("strict and lenient parses of valid fixture differ")
	}
}

// TestOTLookupGlyphGolden verifies glyph substitution results for the valid
// multi-subtable fixture against hand-computed golden output.
func TestOTLookupGlyphGolden(t *testing.T) {
	g := parseGSUB(buildValidGSUBFixture())

	glyphs := []shapingGlyph{
		{gid: 1, cluster: 0},
		{gid: 5, cluster: 1},
		{gid: 7, cluster: 2},
		{gid: 9, cluster: 3},
	}
	// Lookup 0, subtable A: gid 1 → 1+10=11 (coverage [1,2,3]).
	// Lookup 0, subtable B: gid 5 → 50 (coverage [5]).
	// Lookup 1:             gid 7 → 7+100=107 (coverage [7]).
	for i := range g.lookups {
		glyphs = g.applyLookup(&g.lookups[i], glyphs)
	}
	want := []uint16{11, 50, 107, 9}
	if got := glyphIDs(glyphs); !reflect.DeepEqual(got, want) {
		t.Errorf("glyphs = %v, want golden %v", got, want)
	}
	// Clusters must survive substitution.
	for i, g := range glyphs {
		if g.cluster != i {
			t.Errorf("glyph %d cluster = %d, want %d", i, g.cluster, i)
		}
	}
}

// TestGPOSGlyphGolden verifies GPOS positioning for the valid fixture.
func TestGPOSGlyphGolden(t *testing.T) {
	g := parseGPOS(buildValidGPOSFixture())
	if len(g.parseErrs) != 0 {
		t.Fatalf("valid GPOS fixture produced parse errors: %v", g.parseErrs)
	}
	glyphs := []shapingGlyph{{gid: 1, cluster: 0}, {gid: 2, cluster: 1}}
	adj := make([]gposAdjustment, len(glyphs))
	g.applyLookup(&g.lookups[0], glyphs, adj)
	if adj[0].xAdvance != -30 {
		t.Errorf("adj[0].xAdvance = %d, want -30", adj[0].xAdvance)
	}
	if adj[1].xAdvance != 0 {
		t.Errorf("adj[1].xAdvance = %d, want 0 (glyph 2 not covered)", adj[1].xAdvance)
	}
}

// TestOTLookupTruncatedListHeader verifies that a truncated LookupList
// header is reported at list level (lookup index -1).
func TestOTLookupTruncatedListHeader(t *testing.T) {
	for _, data := range [][]byte{nil, {0x00}} {
		res, err := parseLookupListEx(data, "GSUB", otParseLenient)
		if err != nil {
			t.Fatalf("lenient parse returned error: %v", err)
		}
		if len(res.lookups) != 0 {
			t.Errorf("lenient: got %d lookups, want 0", len(res.lookups))
		}
		if len(res.errors) != 1 {
			t.Fatalf("lenient: got %d errors, want 1", len(res.errors))
		}
		e := res.errors[0]
		if e.Table != "GSUB" || e.LookupIndex != -1 || e.SubtableIndex != -1 {
			t.Errorf("error context = (%q, %d, %d), want (GSUB, -1, -1)",
				e.Table, e.LookupIndex, e.SubtableIndex)
		}

		if _, err := parseLookupListEx(data, "GSUB", otParseStrict); err == nil {
			t.Error("strict: expected error for truncated header")
		} else {
			var otErr *OTLookupError
			if !errors.As(err, &otErr) {
				t.Errorf("strict error type = %T, want *OTLookupError", err)
			}
		}
	}
}

// TestOTLookupHugeCounts verifies count-overflow validation for the
// LookupList count and for a lookup's subtable count.
func TestOTLookupHugeCounts(t *testing.T) {
	// LookupList declares 0xFFFF lookups but carries no offset array.
	bb := &binBuilder{}
	bb.u16(0xFFFF)
	bb.u16(0)
	res, err := parseLookupListEx(bb.bytes(), "GSUB", otParseLenient)
	if err != nil {
		t.Fatalf("lenient parse returned error: %v", err)
	}
	if len(res.lookups) != 0 || len(res.errors) != 1 {
		t.Fatalf("got %d lookups / %d errors, want 0/1", len(res.lookups), len(res.errors))
	}
	if !strings.Contains(res.errors[0].Reason, "count") {
		t.Errorf("reason %q does not mention count overflow", res.errors[0].Reason)
	}

	// Valid table, but lookup 0 declares 0xFFFF subtables.
	data := buildValidGSUBFixture()
	patch16(data, 24, 0xFFFF) // lookup0 subtableCount
	g := parseGSUB(data)
	if len(g.lookups) != 2 {
		t.Fatalf("got %d lookups, want 2 (lookup 1 must survive)", len(g.lookups))
	}
	if len(g.lookups[0].subtables) != 0 {
		t.Errorf("lookup0 has %d subtables, want 0", len(g.lookups[0].subtables))
	}
	if len(g.parseErrs) != 1 {
		t.Fatalf("got %d parse errors, want 1", len(g.parseErrs))
	}
	e := g.parseErrs[0]
	if e.LookupIndex != 0 || e.SubtableIndex != -1 {
		t.Errorf("error context = lookup %d subtable %d, want 0/-1", e.LookupIndex, e.SubtableIndex)
	}
	if !strings.Contains(e.Reason, "subtable count") {
		t.Errorf("reason %q does not mention subtable count", e.Reason)
	}
	// Lookup 1 must still apply cleanly.
	glyphs := []shapingGlyph{{gid: 7, cluster: 0}}
	glyphs = g.applyLookup(&g.lookups[1], glyphs)
	if glyphs[0].gid != 107 {
		t.Errorf("lookup1 substitution = %d, want 107", glyphs[0].gid)
	}
}

// TestOTLookupOffsetOutOfRange verifies that an out-of-range lookup offset
// yields an empty placeholder (stable indices) plus a contextual error,
// without affecting the other lookup.
func TestOTLookupOffsetOutOfRange(t *testing.T) {
	data := buildValidGSUBFixture()
	patch16(data, 18, 0x7FFE) // lookupOffset[1] → far beyond table size

	g, err := parseGSUBMode(data, otParseLenient)
	if err != nil {
		t.Fatalf("lenient parse returned error: %v", err)
	}
	if len(g.lookups) != 2 {
		t.Fatalf("got %d lookups, want 2 (placeholder preserves indices)", len(g.lookups))
	}
	if len(g.lookups[1].subtables) != 0 || g.lookups[1].lookupType != 0 {
		t.Errorf("lookup1 placeholder = %+v, want empty", g.lookups[1])
	}
	if len(g.parseErrs) != 1 {
		t.Fatalf("got %d parse errors, want 1", len(g.parseErrs))
	}
	e := g.parseErrs[0]
	if e.Table != "GSUB" || e.LookupIndex != 1 || e.SubtableIndex != -1 {
		t.Errorf("error context = (%q, %d, %d), want (GSUB, 1, -1)",
			e.Table, e.LookupIndex, e.SubtableIndex)
	}
	if e.Offset != 0x7FFE {
		t.Errorf("error offset = %#x, want 0x7ffe", e.Offset)
	}
	if !strings.Contains(e.Error(), "GSUB") || !strings.Contains(e.Error(), "lookup 1") {
		t.Errorf("error string %q missing context", e.Error())
	}

	// Lookup 0 must be fully functional despite its corrupt sibling.
	glyphs := []shapingGlyph{{gid: 1, cluster: 0}, {gid: 5, cluster: 1}}
	glyphs = g.applyLookup(&g.lookups[0], glyphs)
	if got, want := glyphIDs(glyphs), []uint16{11, 50}; !reflect.DeepEqual(got, want) {
		t.Errorf("glyphs = %v, want %v", got, want)
	}

	// Strict mode fails the whole table.
	if _, err := parseGSUBMode(data, otParseStrict); err == nil {
		t.Fatal("strict: expected error")
	} else {
		var otErr *OTLookupError
		if !errors.As(err, &otErr) || otErr.LookupIndex != 1 {
			t.Errorf("strict error = %v, want OTLookupError at lookup 1", err)
		}
	}
}

// TestOTLookupSubtableOutOfRange verifies that a corrupt subtable offset is
// skipped with context while the sibling subtable still applies.
func TestOTLookupSubtableOutOfRange(t *testing.T) {
	data := buildValidGSUBFixture()
	patch16(data, 28, 0x7FFE) // lookup0 subtableOffset[1] → out of range

	g := parseGSUB(data)
	if len(g.parseErrs) != 1 {
		t.Fatalf("got %d parse errors, want 1", len(g.parseErrs))
	}
	e := g.parseErrs[0]
	if e.LookupIndex != 0 || e.SubtableIndex != 1 {
		t.Errorf("error context = lookup %d subtable %d, want 0/1", e.LookupIndex, e.SubtableIndex)
	}
	// Byte range: subtable offset 0x7FFE relative to lookup0 (abs 20),
	// rebased to the LookupList data (abs 14) → 6 + 0x7FFE.
	if e.Offset != 6+0x7FFE {
		t.Errorf("error offset = %#x, want %#x", e.Offset, 6+0x7FFE)
	}
	if len(g.lookups[0].subtables) != 1 {
		t.Fatalf("lookup0 has %d subtables, want 1 (bad one skipped)", len(g.lookups[0].subtables))
	}

	// Only subtable A applies: gid 1 → 11; gid 5 stays (subtable B skipped).
	glyphs := []shapingGlyph{{gid: 1, cluster: 0}, {gid: 5, cluster: 1}}
	glyphs = g.applyLookup(&g.lookups[0], glyphs)
	if got, want := glyphIDs(glyphs), []uint16{11, 5}; !reflect.DeepEqual(got, want) {
		t.Errorf("glyphs = %v, want %v", got, want)
	}
}

// TestOTLookupTruncatedLookup verifies a lookup pointing at a truncated
// tail of the table.
func TestOTLookupTruncatedLookup(t *testing.T) {
	data := buildValidGSUBFixture()
	// Point lookup 1 at the last 2 bytes of the table (abs 80, list-relative 66).
	patch16(data, 18, 66)

	g := parseGSUB(data)
	if len(g.lookups) != 2 {
		t.Fatalf("got %d lookups, want 2", len(g.lookups))
	}
	if len(g.parseErrs) != 1 {
		t.Fatalf("got %d parse errors, want 1", len(g.parseErrs))
	}
	e := g.parseErrs[0]
	if e.LookupIndex != 1 {
		t.Errorf("error lookup index = %d, want 1", e.LookupIndex)
	}
	// The lookup type fits in the 2 available bytes; the failing read is
	// the lookupFlag field at list-relative offset 66+2.
	if e.Offset != 68 {
		t.Errorf("error offset = %d, want 68 (list-relative)", e.Offset)
	}
	if len(g.lookups[1].subtables) != 0 {
		t.Errorf("truncated lookup1 has %d subtables, want 0", len(g.lookups[1].subtables))
	}
}

// TestOTLookupMarkFilteringSetTruncated verifies that a lookup whose
// UseMarkFilteringSet extension field is cut off is reported.
func TestOTLookupMarkFilteringSetTruncated(t *testing.T) {
	bb := &binBuilder{}
	bb.u16(1)      // lookupCount=1
	bb.u16(4)      // lookupOffset[0] → abs 4
	bb.u16(1)      // lookup type=1
	bb.u16(0x0010) // flag=UseMarkFilteringSet
	bb.u16(1)      // subtableCount=1
	bb.u16(10)     // subtableOffset[0]
	// markFilteringSet uint16 missing — data ends here.

	res, err := parseLookupListEx(bb.bytes(), "GPOS", otParseLenient)
	if err != nil {
		t.Fatalf("lenient parse returned error: %v", err)
	}
	if len(res.errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(res.errors))
	}
	e := res.errors[0]
	if e.Table != "GPOS" || e.LookupIndex != 0 {
		t.Errorf("error context = (%q, %d), want (GPOS, 0)", e.Table, e.LookupIndex)
	}
	if res.lookups[0].hasMarkFilteringSet {
		t.Error("markFilteringSet must not be marked valid when truncated")
	}
}

// TestOTLookupParseDeterministic verifies that parsing does not mutate its
// input and that repeated parses produce identical results.
func TestOTLookupParseDeterministic(t *testing.T) {
	fixtures := map[string][]byte{
		"valid":           buildValidGSUBFixture(),
		"subtable-oob":    func() []byte { d := buildValidGSUBFixture(); patch16(d, 28, 0x7FFE); return d }(),
		"huge-subt-count": func() []byte { d := buildValidGSUBFixture(); patch16(d, 24, 0xFFFF); return d }(),
	}
	for name, data := range fixtures {
		pristine := append([]byte(nil), data...)
		r1, err1 := parseLookupListEx(data[14:], "GSUB", otParseLenient)
		r2, err2 := parseLookupListEx(data[14:], "GSUB", otParseLenient)
		if err1 != nil || err2 != nil {
			t.Fatalf("%s: lenient parse returned error: %v / %v", name, err1, err2)
		}
		if !reflect.DeepEqual(r1, r2) {
			t.Errorf("%s: repeated parses differ", name)
		}
		if !reflect.DeepEqual(data, pristine) {
			t.Errorf("%s: parser mutated its input", name)
		}
	}
}

// TestOTLookupExtensionNoRecursion verifies that deeply corrupt extension
// (type 7) chains terminate without recursion or stack overflow.
func TestOTLookupExtensionNoRecursion(t *testing.T) {
	// Extension subtable pointing at itself with extType=7 (extension):
	// applySubtable has no case 7, so it is a no-op — no recursion.
	self := &binBuilder{}
	self.u16(1) // format 1
	self.u16(7) // extType=7 (extension again)
	self.u32(0) // extOffset=0 → points at itself

	// Extension subtable with a huge extOffset → skipped by bounds check.
	huge := &binBuilder{}
	huge.u16(1)
	huge.u16(1)
	huge.u32(0xFFFFFFFF)

	// A lookup with thousands of self-referential extension subtables:
	// the apply loop is iterative and must terminate quickly.
	const n = 10000
	sts := make([]otSubtable, 0, n+2)
	for range n {
		sts = append(sts, otSubtable{data: self.bytes()})
	}
	sts = append(sts, otSubtable{data: huge.bytes()})

	g := &gsubTable{lookups: []otLookup{{lookupType: 7, subtables: sts}}}
	glyphs := []shapingGlyph{{gid: 1, cluster: 0}, {gid: 2, cluster: 1}}
	out := g.applyLookup(&g.lookups[0], glyphs)
	if got, want := glyphIDs(out), []uint16{1, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("glyphs = %v, want unchanged %v", got, want)
	}
}

// loadRawTable extracts a raw table from a testdata font file.
func loadRawTable(t *testing.T, path, tag string) []byte {
	t.Helper()
	fontData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read font: %v", err)
	}
	tables, err := parseFontTables(fontData)
	if err != nil {
		t.Fatalf("parse font tables: %v", err)
	}
	return tables[tag]
}

// TestOTLookupRealFonts verifies that real, valid fonts parse cleanly in
// strict mode and that strict and lenient modes agree, and compares glyph
// results between the two parsed tables (multi-subtable real fonts).
func TestOTLookupRealFonts(t *testing.T) {
	fonts := []string{
		"testdata/goregular.ttf",
		"testdata/notoserif_autohint_shaping.ttf",
	}
	for _, path := range fonts {
		for _, tag := range []string{"GSUB", "GPOS"} {
			raw := loadRawTable(t, path, tag)
			if raw == nil {
				continue // font has no such table
			}
			if tag == "GSUB" {
				gs, err := parseGSUBMode(raw, otParseStrict)
				if err != nil {
					t.Errorf("%s %s: strict parse failed: %v", path, tag, err)
					continue
				}
				gl := parseGSUB(raw)
				if len(gl.parseErrs) != 0 {
					t.Errorf("%s %s: lenient parse errors: %v", path, tag, gl.parseErrs)
				}
				if !reflect.DeepEqual(gs.lookups, gl.lookups) {
					t.Errorf("%s %s: strict and lenient lookups differ", path, tag)
				}
				// Glyph results must be identical between the two parses.
				// (applyLookup mutates the buffer in place, so each parse
				// gets its own copy.)
				mkGlyphs := func() []shapingGlyph {
					return []shapingGlyph{{gid: 1, cluster: 0}, {gid: 2, cluster: 1}, {gid: 3, cluster: 2}}
				}
				want := mkGlyphs()
				for i := range gs.lookups {
					want = gs.applyLookup(&gs.lookups[i], want)
				}
				got := mkGlyphs()
				for i := range gl.lookups {
					got = gl.applyLookup(&gl.lookups[i], got)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s %s: glyph results differ between modes", path, tag)
				}
			} else {
				gp, err := parseGPOSMode(raw, otParseStrict)
				if err != nil {
					t.Errorf("%s %s: strict parse failed: %v", path, tag, err)
					continue
				}
				gl := parseGPOS(raw)
				if len(gl.parseErrs) != 0 {
					t.Errorf("%s %s: lenient parse errors: %v", path, tag, gl.parseErrs)
				}
				if !reflect.DeepEqual(gp.lookups, gl.lookups) {
					t.Errorf("%s %s: strict and lenient lookups differ", path, tag)
				}
			}
		}
	}
}

// TestOTLookupErrorString verifies the error message carries full context.
func TestOTLookupErrorString(t *testing.T) {
	e := &OTLookupError{
		Table:         "GPOS",
		LookupIndex:   3,
		SubtableIndex: 1,
		Offset:        100,
		Length:        6,
		Reason:        "subtable offset 500 out of range (lookup data size 128)",
	}
	s := e.Error()
	for _, want := range []string{"GPOS", "lookup 3", "subtable 1", "[100, 106)", "out of range"} {
		if !strings.Contains(s, want) {
			t.Errorf("error string %q missing %q", s, want)
		}
	}
}
