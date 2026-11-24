// Coverage for nested-lookup paths that the font-based golden tests do not
// reach: Type 9 extensions invoked from a contextual record, class-based
// PairPos used as a nested lookup, nested chaining lookups, and the error
// formatting of GPOSError.
package text

import (
	"testing"
)

// singlePosFmt1 builds a Type 1 format-1 SinglePos subtable matching gid 1
// and applying the given value record (xAdvance only, valueFormat 0x0004).
func singlePosFmt1(gid uint16, xAdvance int16) []byte {
	cov := coverage1(gid)
	payload := make([]byte, 0, 8+len(cov))
	payload = append(payload,
		0, 1, // format 1
		0, 8, // coverage offset
		0, 4, // valueFormat = xAdvance
		byte(uint16(xAdvance)>>8), byte(uint16(xAdvance)),
	)
	return append(payload, cov...)
}

// pairPosFmt2 builds a Type 2 format-2 PairPos subtable: coverage gid 1,
// classDef assigning gids 1 and 2 to class 1, one class pair (1,1) carrying
// xAdvance -70 on the first glyph.
func pairPosFmt2() []byte {
	cov := coverage1(1)
	// ClassDef format 2: ranges gid 1..2 -> class 1.
	classDef := []byte{
		0, 2, // format 2
		0, 1, // range count
		0, 1, 0, 2, // start 1, end 2
		0, 1, // class 1
	}
	out := make([]byte, 0, 40)
	// Header fields; offsets patched after layout.
	out = append(out,
		0, 2, // format 2
		0, 0, // coverage offset (patch)
		0, 4, // valueFormat1 = xAdvance
		0, 0, // valueFormat2 = 0
		0, 0, // classDef1 offset (patch)
		0, 0, // classDef2 offset (patch, reuse classDef1)
		0, 2, // class1Count
		0, 2, // class2Count
	)
	// Class value array: 2x2 records of 2 bytes each (valueFormat1 size 2,
	// valueFormat2 size 0). Only cell (1,1) gets -70.
	negAdv := int16(-70)
	cells := []uint16{0, 0, 0, uint16(negAdv)}
	for _, c := range cells {
		out = append(out, byte(c>>8), byte(c))
	}
	covOff := uint16(len(out))
	out = append(out, cov...)
	cdOff := uint16(len(out))
	out = append(out, classDef...)
	patchU16 := func(off int, v uint16) {
		out[off] = byte(v >> 8)
		out[off+1] = byte(v)
	}
	patchU16(2, covOff)
	patchU16(8, cdOff)
	patchU16(10, cdOff)
	return out
}

// extensionFmt9 wraps an inner subtable in a Type 9 format-1 extension.
func extensionFmt9(extType uint16, inner []byte) []byte {
	payload := make([]byte, 0, 8+len(inner))
	payload = append(payload,
		0, 1, // extension format 1
		byte(extType>>8), byte(extType),
		0, 0, 0, 8, // extension offset = 8
	)
	return append(payload, inner...)
}

// TestGPOSContext_NestedPairPosFormat2 drives a class-based PairPos through
// a contextual lookup so applyPairPosAtFormat2 is exercised.
func TestGPOSContext_NestedPairPosFormat2(t *testing.T) {
	// Lookup 0: ctx fmt3 matching gid 1, record (seq 0 → lookup 1).
	// Lookup 1: PairPos format 2.
	f := gposFixture{
		lookupTypes: []uint16{7, 2},
		lookupPayloads: [][]byte{
			contextFmt3Payload(1),
			pairPosFmt2(),
		},
	}
	engine, adj := engineFor(t, f, 2) // gids 1, 2
	engine.applyNestedLookup(0, 0)
	if engine.err != nil {
		t.Fatalf("unexpected error: %v", engine.err)
	}
	if adj[0].xAdvance != -70 {
		t.Errorf("xAdvance = %d, want -70 from nested class PairPos", adj[0].xAdvance)
	}
}

// TestGPOSContext_NestedExtension drives a Type 9 extension from a
// contextual PosLookupRecord (applyNestedExtension path).
func TestGPOSContext_NestedExtension(t *testing.T) {
	// Lookup 0: ctx fmt3 gid 1 → lookup 1.
	// Lookup 1: Type 9 wrapping SinglePos (xAdvance +33 on gid 1).
	f := gposFixture{
		lookupTypes: []uint16{7, 9},
		lookupPayloads: [][]byte{
			contextFmt3Payload(1),
			extensionFmt9(1, singlePosFmt1(1, 33)),
		},
	}
	engine, adj := engineFor(t, f, 2)
	engine.applyNestedLookup(0, 0)
	if engine.err != nil {
		t.Fatalf("unexpected error: %v", engine.err)
	}
	if adj[0].xAdvance != 33 {
		t.Errorf("xAdvance = %d, want 33 via nested extension", adj[0].xAdvance)
	}
}

// TestGPOSContext_NestedChaining invokes a Type 8 lookup from within a
// contextual record (applyChainingAt path).
func TestGPOSContext_NestedChaining(t *testing.T) {
	// Chain fmt3: no backtrack, one input coverage (gid 1), no lookahead,
	// one record (seq 0 → lookup 2, SinglePos xAdvance +17).
	chainPayload := func() []byte {
		cov := coverage1(1)
		out := make([]byte, 0, 32)
		out = append(out,
			0, 3, // format 3
			0, 0, // backtrackGlyphCount
			0, 1, // inputGlyphCount
			0, 0, // input coverage offset (patch at byte 6)
			0, 0, // lookAheadGlyphCount
			0, 1, // posCount
			0, 0, // sequence index
			0, 2, // lookup list index 2
		)
		covOff := uint16(len(out))
		out = append(out, cov...)
		out[6] = byte(covOff >> 8)
		out[7] = byte(covOff)
		return out
	}
	f := gposFixture{
		lookupTypes: []uint16{7, 8, 1},
		lookupPayloads: [][]byte{
			contextFmt3Payload(1), // lookup 0 → lookup 1
			chainPayload(),        // lookup 1 → lookup 2
			singlePosFmt1(1, 17),  // lookup 2
		},
	}
	engine, adj := engineFor(t, f, 2)
	engine.applyNestedLookup(0, 0)
	if engine.err != nil {
		t.Fatalf("unexpected error: %v", engine.err)
	}
	if adj[0].xAdvance != 17 {
		t.Errorf("xAdvance = %d, want 17 through nested chaining", adj[0].xAdvance)
	}
}

// TestGPOSError_Formatting exercises both branches of GPOSError.Error.
func TestGPOSError_Formatting(t *testing.T) {
	plain := &GPOSError{Reason: "text: test reason"}
	if got := plain.Error(); got != "text: test reason" {
		t.Errorf("plain Error() = %q", got)
	}
	typed := &GPOSError{Reason: "text: test reason", LookupType: 8}
	if got := typed.Error(); got != "text: test reason (lookup type 8)" {
		t.Errorf("typed Error() = %q", got)
	}
}
