// Safety tests for contextual GPOS: malformed tables, bad offsets, lookup
// cycles, excessive nesting and the operation budget must all terminate
// safely (never panic) and either report a contextual error or stop with the
// adjustments accumulated so far.
//
// The fixtures are minimal hand-built GPOS binaries. They contain just a
// ScriptList/FeatureList/LookupList wrapper around the subtable under test,
// which is enough for parseGPOS + applyGPOS to reach the contextual engine.
package text

import (
	"encoding/binary"
	"errors"
	"testing"
)

// gposFixture assembles a minimal version-1.0 GPOS table whose single
// feature (tag 'kern') references the given lookups. One subtable per lookup
// is serialized; all other offsets are filled in by the builder.
type gposFixture struct {
	lookupTypes    []uint16
	lookupPayloads [][]byte // subtable bytes per lookup (one subtable each)
}

// build serializes the fixture into a complete GPOS table.
func (f gposFixture) build(t *testing.T) []byte {
	t.Helper()
	n := len(f.lookupTypes)

	// --- LookupList ---
	lookupList := make([]byte, 2, 2+n*2)
	binary.BigEndian.PutUint16(lookupList[0:], uint16(n))
	lookupBodies := make([][]byte, n)
	lookupOffsets := make([]uint16, n)
	cursor := 2 + n*2
	for i := range n {
		lookupOffsets[i] = uint16(cursor)
		payload := f.lookupPayloads[i]
		body := make([]byte, 0, 8+len(payload))
		header := make([]byte, 8) // type, flag, subtableCount, subtableOffset
		binary.BigEndian.PutUint16(header[0:], f.lookupTypes[i])
		binary.BigEndian.PutUint16(header[2:], 0) // flags
		binary.BigEndian.PutUint16(header[4:], 1) // one subtable
		binary.BigEndian.PutUint16(header[6:], 8) // subtable starts after header
		body = append(body, header...)
		body = append(body, payload...)
		lookupBodies[i] = body
		cursor += len(body)
	}
	for _, off := range lookupOffsets {
		lookupList = append(lookupList, byte(off>>8), byte(off))
	}
	for _, body := range lookupBodies {
		lookupList = append(lookupList, body...)
	}

	// --- FeatureList: one feature 'kern' referencing lookup 0..n-1 ---
	featureBody := make([]byte, 0, 4+2*n)
	featureBody = append(featureBody, 0, 0, byte(n>>8), byte(n))
	for i := range n {
		featureBody = append(featureBody, byte(i>>8), byte(i))
	}
	featureList := make([]byte, 0, 8+len(featureBody))
	featureList = append(featureList,
		0, 1, // featureCount
		'k', 'e', 'r', 'n',
		0, 8, // feature table offset
	)
	featureList = append(featureList, featureBody...)

	// --- ScriptList: DFLT script, default LangSys referencing feature 0 ---
	langSys := []byte{0, 0, 0xFF, 0xFF, 0, 1, 0, 0}
	scriptBody := make([]byte, 0, 4+len(langSys))
	scriptBody = append(scriptBody, 0, 4, 0, 0) // default LangSys at offset 4
	scriptBody = append(scriptBody, langSys...)
	scriptList := make([]byte, 0, 8+len(scriptBody))
	scriptList = append(scriptList,
		0, 1, // scriptCount
		'D', 'F', 'L', 'T',
		0, 8, // script table offset
	)
	scriptList = append(scriptList, scriptBody...)

	// --- Header ---
	total := 10 + len(scriptList) + len(featureList) + len(lookupList)
	out := make([]byte, 10, total)
	out[0], out[1] = 0, 1 // version 1.0
	binary.BigEndian.PutUint16(out[4:], 10)
	binary.BigEndian.PutUint16(out[6:], uint16(10+len(scriptList)))
	binary.BigEndian.PutUint16(out[8:], uint16(10+len(scriptList)+len(featureList)))
	out = append(out, scriptList...)
	out = append(out, featureList...)
	out = append(out, lookupList...)
	return out
}

// engineFor builds a fixture and returns a ready engine and adjustment array.
func engineFor(t *testing.T, f gposFixture, glyphCount int) (*gposEngine, []gposAdjustment) {
	t.Helper()
	g := parseGPOS(f.build(t))
	if g == nil {
		t.Fatal("parseGPOS returned nil for fixture")
	}
	glyphs := make([]shapingGlyph, glyphCount)
	for i := range glyphs {
		glyphs[i] = shapingGlyph{gid: uint16(i + 1), cluster: i}
	}
	adj := make([]gposAdjustment, glyphCount)
	return &gposEngine{g: g, glyphs: glyphs, adj: adj}, adj
}

// u16 appends a big-endian uint16.
func u16(v uint16) []byte { return []byte{byte(v >> 8), byte(v)} }

// coverage1 builds a format-1 coverage table containing gids.
func coverage1(gids ...uint16) []byte {
	b := make([]byte, 0, 4+2*len(gids))
	b = append(b, u16(1)...)
	b = append(b, u16(uint16(len(gids)))...)
	for _, g := range gids {
		b = append(b, u16(g)...)
	}
	return b
}

// contextFmt3Payload builds a Type 7 format-3 subtable with one coverage
// (matching gid 1) and a single record pointing at targetLookup.
func contextFmt3Payload(targetLookup uint16) []byte {
	const covOffset = 12 // 6 header + 2 coverage-array + 4 record
	payload := make([]byte, 0, covOffset+len(coverage1(1)))
	payload = append(payload,
		0, 3, 0, 1, 0, 1, // format 3, one glyph, one record
		0, byte(covOffset), // coverage offset
		0, 0, byte(targetLookup>>8), byte(targetLookup), // record
	)
	return append(payload, coverage1(1)...)
}

// requireGPOSError fails the test unless err wraps a *GPOSError whose Reason
// matches want. It uses errors.As so future error wrapping stays supported.
func requireGPOSError(t *testing.T, err error, want *GPOSError) {
	t.Helper()
	var gpe *GPOSError
	if !errors.As(err, &gpe) {
		t.Fatalf("err = %v, want GPOSError %q", err, want.Reason)
	}
	if gpe.Reason != want.Reason {
		t.Errorf("GPOSError reason = %q, want %q", gpe.Reason, want.Reason)
	}
}

// TestGPOSContext_TruncatedSubtables feeds truncated versions of the
// format-1 contextual and chaining headers and asserts the engine fails with
// a contextual error instead of panicking.
func TestGPOSContext_TruncatedSubtables(t *testing.T) {
	formats := map[uint16][]byte{
		7: {0, 1, 0, 4, 0, 1, 0xFF}, // ctx fmt1: truncated rule-set offset array
		8: {0, 1, 0, 4, 0, 1, 0xFF}, // chain fmt1: same shape
	}
	for lookupType, payload := range formats {
		t.Run("type"+itoaUint16(lookupType), func(t *testing.T) {
			f := gposFixture{
				lookupTypes:    []uint16{lookupType},
				lookupPayloads: [][]byte{payload},
			}
			engine, adj := engineFor(t, f, 3)
			if lookupType == 7 {
				engine.applyContextual(payload)
			} else {
				engine.applyChaining(payload)
			}
			if engine.err == nil {
				t.Error("expected contextual error on truncated table, got nil")
			}
			// Existing adjustment slots must remain zero-valued.
			for i, a := range adj {
				if a != (gposAdjustment{}) {
					t.Errorf("slot %d corrupted after malformed table: %+v", i, a)
				}
			}
		})
	}
}

// TestGPOSContext_BadCoverageOffset uses a coverage offset pointing past the
// subtable end; matching must stop with errGPOSMalformed, and the checked
// entry point must surface that error to the caller.
func TestGPOSContext_BadCoverageOffset(t *testing.T) {
	// Context fmt1 header with coverage offset 0x7FFF (out of range).
	payload := []byte{0, 1, 0x7F, 0xFF, 0, 0}
	f := gposFixture{
		lookupTypes:    []uint16{7},
		lookupPayloads: [][]byte{payload},
	}
	g := parseGPOS(f.build(t))
	glyphs := []shapingGlyph{{gid: 1, cluster: 0}, {gid: 2, cluster: 1}}
	_, err := g.applyGPOSChecked(glyphs,
		[4]byte{'D', 'F', 'L', 'T'}, [4]byte{}, [][4]byte{{'k', 'e', 'r', 'n'}})
	requireGPOSError(t, err, errGPOSMalformed)
	// The legacy entry point keeps its signature and never returns errors.
	_ = g.applyGPOS(glyphs,
		[4]byte{'D', 'F', 'L', 'T'}, [4]byte{}, [][4]byte{{'k', 'e', 'r', 'n'}})
}

// TestGPOSContext_BadNestedLookupIndex builds a format-3 context rule whose
// PosLookupRecord names a non-existent lookup; the engine must flag a
// malformed table rather than index out of range.
func TestGPOSContext_BadNestedLookupIndex(t *testing.T) {
	payload := contextFmt3Payload(99) // lookup 99 does not exist
	f := gposFixture{
		lookupTypes:    []uint16{7},
		lookupPayloads: [][]byte{payload},
	}
	engine, _ := engineFor(t, f, 2)
	engine.applyContextual(payload)
	requireGPOSError(t, engine.err, errGPOSMalformed)
}

// TestGPOSContext_LookupCycle builds two contextual lookups that reference
// each other. The cycle guard must stop recursion without panicking or
// spinning, and must not exhaust the whole budget on one rule.
func TestGPOSContext_LookupCycle(t *testing.T) {
	f := gposFixture{
		lookupTypes: []uint16{7, 7},
		lookupPayloads: [][]byte{
			contextFmt3Payload(1), // lookup 0 → 1
			contextFmt3Payload(0), // lookup 1 → 0
		},
	}
	engine, adj := engineFor(t, f, 4)
	// Apply lookup 0 directly; the cycle guard returns without error.
	engine.applyNestedLookup(0, 0)
	if engine.err != nil {
		t.Errorf("cycle should be a safe stop, got error: %v", engine.err)
	}
	for i, a := range adj {
		if a != (gposAdjustment{}) {
			t.Errorf("cycle produced adjustments at %d: %+v", i, a)
		}
	}
}

// TestGPOSContext_DepthLimit chains ten contextual lookups (0→1→2→…→9) to
// exceed gposMaxNestDepth (8). The engine must report errGPOSDepth.
func TestGPOSContext_DepthLimit(t *testing.T) {
	const n = 10
	payloads := make([][]byte, n)
	types := make([]uint16, n)
	for i := range n {
		types[i] = 7
		target := uint16(i + 1)
		if i == n-1 {
			// Last lookup points at itself so the chain keeps trying.
			target = uint16(n - 1)
		}
		payloads[i] = contextFmt3Payload(target)
	}
	f := gposFixture{lookupTypes: types, lookupPayloads: payloads}
	engine, _ := engineFor(t, f, 2)
	engine.applyNestedLookup(0, 0)
	requireGPOSError(t, engine.err, errGPOSDepth)
}

// TestGPOSContext_MaxOps builds a format-3 coverage-array rule over a long
// glyph buffer so the per-shaping operation budget is exceeded; the engine
// must stop and report errGPOSMaxOps with no panic.
func TestGPOSContext_MaxOps(t *testing.T) {
	// Coverage format 2 covering gids 1..1000 so every buffer position matches.
	cov := []byte{
		0, 2, // format 2
		0, 1, // one range
		0, 1, 0x03, 0xE8, // start 1, end 1000
		0, 0, // startCoverageIndex 0
	}
	// No records here: coverage array ends at byte 8, so the coverage offset
	// is 8 (6 header + 2 coverage-offset array).
	payload := make([]byte, 0, 8+len(cov))
	payload = append(payload,
		0, 3, 0, 1, // format 3, glyphCount 1
		0, 0, // posCount 0 (pure matching; each position still costs an op)
		0, 8, // coverage offset
	)
	payload = append(payload, cov...)
	f := gposFixture{
		lookupTypes:    []uint16{7},
		lookupPayloads: [][]byte{payload},
	}
	engine, _ := engineFor(t, f, gposMaxOps+10)
	engine.applyContextual(payload)
	requireGPOSError(t, engine.err, errGPOSMaxOps)
}

// TestGPOSContext_BadExtensionOffset feeds a Type 9 extension subtable whose
// extension offset is zero (would alias the header) and one whose offset is
// past the end; both must be reported as malformed.
func TestGPOSContext_BadExtensionOffset(t *testing.T) {
	extSubtable := func(extOff uint32) []byte {
		b := make([]byte, 8)
		binary.BigEndian.PutUint16(b[0:], 1)      // extension format 1
		binary.BigEndian.PutUint16(b[2:], 7)      // wraps a Type 7 table
		binary.BigEndian.PutUint32(b[4:], extOff) // (bad) extension offset
		return b
	}
	for name, payload := range map[string][]byte{
		"zero":  extSubtable(0),
		"large": extSubtable(0x7FFFFFFF),
	} {
		t.Run(name, func(t *testing.T) {
			f := gposFixture{
				lookupTypes:    []uint16{9},
				lookupPayloads: [][]byte{payload},
			}
			engine, _ := engineFor(t, f, 2)
			engine.applyTopLevelExtension(payload)
			requireGPOSError(t, engine.err, errGPOSMalformed)
		})
	}
}

// TestGPOSContext_ClassDefOffsetZero verifies that a format-2 contextual
// subtable with a zero (absent) ClassDef offset treats every glyph as class 0
// and therefore matches nothing, without erroring.
func TestGPOSContext_ClassDefOffsetZero(t *testing.T) {
	cov := coverage1(1)
	header := []byte{
		0, 2, // format 2
		0, 0, // coverage offset (patched below)
		0, 0, // classDef offset = 0 → absent
		0, 1, // posClassSetCount = 1
		0, 0, // class set 0 offset = 0 (no rules)
	}
	covOff := uint16(len(header))
	payload := make([]byte, 0, len(header)+len(cov))
	payload = append(payload, header...)
	binary.BigEndian.PutUint16(payload[2:], covOff)
	payload = append(payload, cov...)
	f := gposFixture{
		lookupTypes:    []uint16{7},
		lookupPayloads: [][]byte{payload},
	}
	engine, adj := engineFor(t, f, 3)
	engine.applyContextual(payload)
	if engine.err != nil {
		t.Errorf("absent classDef must not error, got: %v", engine.err)
	}
	for i, a := range adj {
		if a != (gposAdjustment{}) {
			t.Errorf("slot %d unexpectedly adjusted: %+v", i, a)
		}
	}
}
