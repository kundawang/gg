package text

import (
	"encoding/binary"
	"math"
	"testing"
)

// buildVhea builds a 36-byte vhea table with the fields exercised by
// parseVheaTable.
func buildVhea(ascender, descender, lineGap, advHeightMax int16, numLong uint16) []byte {
	data := make([]byte, 36)
	binary.BigEndian.PutUint32(data[0:4], 0x00010000) // version 1.0
	binary.BigEndian.PutUint16(data[4:6], uint16(ascender))
	binary.BigEndian.PutUint16(data[6:8], uint16(descender))
	binary.BigEndian.PutUint16(data[8:10], uint16(lineGap))
	binary.BigEndian.PutUint16(data[10:12], uint16(advHeightMax))
	binary.BigEndian.PutUint16(data[34:36], numLong)
	return data
}

// buildVmtx builds a vmtx table with numLong long metrics and a TSB tail
// covering numGlyphs total.
func buildVmtx(longMetrics [][2]int16, tailTSB []int16) []byte {
	data := make([]byte, len(longMetrics)*4+len(tailTSB)*2)
	off := 0
	for _, m := range longMetrics {
		binary.BigEndian.PutUint16(data[off:off+2], uint16(m[0]))
		binary.BigEndian.PutUint16(data[off+2:off+4], uint16(m[1]))
		off += 4
	}
	for _, tsb := range tailTSB {
		binary.BigEndian.PutUint16(data[off:off+2], uint16(tsb))
		off += 2
	}
	return data
}

// TestParseVheaTable parses a synthetic vhea and checks every mapped field.
func TestParseVheaTable(t *testing.T) {
	data := buildVhea(880, -120, 0, 1000, 3)

	got, ok := parseVheaTable(data)
	if !ok {
		t.Fatal("parseVheaTable rejected valid table")
	}
	if got.vertTypoAscender != 880 || got.vertTypoDescender != -120 {
		t.Errorf("typo metrics = %d/%d", got.vertTypoAscender, got.vertTypoDescender)
	}
	if got.advanceHeightMax != 1000 {
		t.Errorf("advanceHeightMax = %d", got.advanceHeightMax)
	}
	if got.numberOfVMetrics != 3 {
		t.Errorf("numberOfVMetrics = %d", got.numberOfVMetrics)
	}
}

// TestParseVheaTable_Truncated rejects tables shorter than 36 bytes.
func TestParseVheaTable_Truncated(t *testing.T) {
	if _, ok := parseVheaTable(make([]byte, 35)); ok {
		t.Error("truncated vhea should fail")
	}
}

// TestParseVmtx_LongMetricsAndTail verifies long metrics parse and that
// glyphs beyond the long count inherit the last advance plus their own TSB.
func TestParseVmtx_LongMetricsAndTail(t *testing.T) {
	long := [][2]int16{{1000, 100}, {900, 90}}
	tail := []int16{80, 70}
	data := buildVmtx(long, tail)

	advances, tsbs, ok := parseVmtx(data, 2, 4)
	if !ok {
		t.Fatal("parseVmtx rejected valid table")
	}
	if len(advances) != 2 || len(tsbs) != 4 {
		t.Fatalf("unexpected sizes: adv=%d tsb=%d", len(advances), len(tsbs))
	}
	if advances[0] != 1000 || advances[1] != 900 {
		t.Errorf("advances = %v", advances)
	}
	wantTSB := []int16{100, 90, 80, 70}
	for i, want := range wantTSB {
		if tsbs[i] != want {
			t.Errorf("tsb[%d] = %d, want %d", i, tsbs[i], want)
		}
	}

	// Monospace tail: gid 2 and 3 inherit advance 900.
	if got := vmtxAdvance(advances, 2, 2); got != 900 {
		t.Errorf("tail advance = %d, want 900", got)
	}
	if got := vmtxAdvance(advances, 2, 3); got != 900 {
		t.Errorf("tail advance = %d, want 900", got)
	}
	if got := vmtxAdvance(advances, 2, 0); got != 1000 {
		t.Errorf("long advance = %d, want 1000", got)
	}
}

// TestParseVmtx_TruncatedTail tolerates a missing TSB tail rather than
// failing outright, matching parseHmtx behavior.
func TestParseVmtx_TruncatedTail(t *testing.T) {
	long := [][2]int16{{1000, 100}}
	data := buildVmtx(long, nil) // declares 3 glyphs but tail is absent

	advances, tsbs, ok := parseVmtx(data, 1, 3)
	if !ok {
		t.Fatal("truncated tail should still return long metrics")
	}
	if len(advances) != 1 || len(tsbs) != 3 {
		t.Fatalf("unexpected sizes: adv=%d tsb=%d", len(advances), len(tsbs))
	}
	if tsbs[1] != 0 || tsbs[2] != 0 {
		t.Errorf("missing tail TSBs should default to zero: %v", tsbs)
	}
}

// TestParseVmtx_Invalid verifies degenerate inputs fail.
func TestParseVmtx_Invalid(t *testing.T) {
	if _, _, ok := parseVmtx(nil, 2, 4); ok {
		t.Error("nil vmtx should fail")
	}
	if _, _, ok := parseVmtx([]byte{0, 0}, 0, 4); ok {
		t.Error("zero long metrics should fail")
	}
}

// TestVerticalMetrics_FallbackWithoutTables verifies that a font without
// vhea/vmtx (goregular) reports no vertical metrics and synthesizes
// em-square values through the VerticalMetricProvider interface.
func TestVerticalMetrics_FallbackWithoutTables(t *testing.T) {
	face := layoutTestFace(t)
	parsed := face.Source().Parsed()

	vmp, ok := parsed.(VerticalMetricProvider)
	if !ok {
		t.Fatal("ownParsedFont should implement VerticalMetricProvider")
	}
	if vmp.HasVerticalMetrics() {
		t.Fatal("goregular has no vertical tables; HasVerticalMetrics should be false")
	}

	size := face.Size()
	// Use the 'A' glyph (non-zero advance) to validate synthesized metrics.
	gid := parsed.GlyphIndex('A')
	if gid == 0 {
		t.Fatal("test font must contain an A glyph")
	}
	// VerticalOrigin shares the raw hmtx advance source used by GlyphAdvance.
	wantAdv := parsed.GlyphAdvance(gid, size)
	if got := vmp.VerticalAdvance(gid, size); math.Abs(got-size) > 1e-9 {
		t.Errorf("synthesized advance = %f, want size %f", got, size)
	}
	ox, oy := vmp.VerticalOrigin(gid, size)
	if math.Abs(ox-wantAdv/2) > 1e-9 {
		t.Errorf("synthesized originX = %f, want %f", ox, wantAdv/2)
	}
	if math.Abs(oy+size/2) > 1e-9 {
		t.Errorf("synthesized originY = %f, want %f", oy, -size/2)
	}
	if gap := vmp.VerticalLineGap(size); gap != 0 {
		t.Errorf("fallback line gap = %f, want 0", gap)
	}
}
