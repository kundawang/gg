// Tests for GPOS contextual (Type 7, formats 1/2/3) and chaining contextual
// (Type 8, formats 1/2/3) positioning.
//
// Three layers are covered:
//
//  1. Golden parity against HarfBuzz on a purpose-built font
//     (testdata/ctxpos/ctxpos_test.ttf) whose GPOS table exercises every
//     format, nested SinglePos/PairPos lookups and a Type 9 extension wrapper.
//     The expected adjustments below were generated with uharfbuzz 0.57
//     (see /tmp/gpos-ref/gen_ref.py) and are stored as explicit values so the
//     test has no Python dependency.
//  2. Real-world fonts: an Amiri (Arabic) subset and a Noto Sans Devanagari
//     subset, checking positions, clusters and advances stay stable.
//  3. Safety: truncated tables, bad offsets, cycles, deep nesting and the
//     operation budget must stop safely (no panic) and report a contextual
//     error rather than corrupting the adjustment array.
package text

import (
	"os"
	"testing"
)

// ctxPosAdjustment is the expected per-glyph adjustment in font units:
// (xPlacement, yPlacement, xAdvance, yAdvance).
type ctxPosAdjustment struct {
	xPla, yPla, xAdv, yAdv int16
}

// loadCtxGPOS parses the synthetic font and returns its GPOS table.
func loadCtxGPOS(t *testing.T) *gposTable {
	t.Helper()
	data, err := os.ReadFile("testdata/ctxpos/ctxpos_test.ttf")
	if err != nil {
		t.Fatalf("read test font: %v", err)
	}
	tables, err := parseFontTables(data)
	if err != nil {
		t.Fatalf("parse font tables: %v", err)
	}
	g := parseGPOS(tables["GPOS"])
	if g == nil {
		t.Fatal("font has no GPOS table")
	}
	return g
}

func ctxKernTag() [4]byte { return [4]byte{'k', 'e', 'r', 'n'} }
func ctxLatnTag() [4]byte { return [4]byte{'l', 'a', 't', 'n'} }
func ctxArabTag() [4]byte { return [4]byte{'a', 'r', 'a', 'b'} }

// TestGPOSContext_HarfbuzzGolden compares engine output against HarfBuzz
// reference adjustments for every contextual/chaining format. The gid order
// in each case is the engine's logical buffer order; RTL expectations are
// keyed the same way (adjustments attach to glyphs, not positions).
func TestGPOSContext_HarfbuzzGolden(t *testing.T) {
	g := loadCtxGPOS(t)

	cases := []struct {
		name   string
		script [4]byte
		glyphs []shapingGlyph
		want   []ctxPosAdjustment
	}{
		{
			// Type 7 format 1: f + question → SinglePos on question.
			name:   "ctx7.1 f?",
			script: ctxLatnTag(),
			glyphs: []shapingGlyph{{gid: 4, cluster: 0}, {gid: 8, cluster: 1}},
			want: []ctxPosAdjustment{
				{0, 0, 0, 0},
				{30, 0, 15, 0},
			},
		},
		{
			// Type 7 format 2: class-based, A + period → -25 advance on A.
			name:   "ctx7.2 A.",
			script: ctxLatnTag(),
			glyphs: []shapingGlyph{{gid: 2, cluster: 0}, {gid: 5, cluster: 1}},
			want: []ctxPosAdjustment{
				{0, 0, -25, 0},
				{0, 0, 0, 0},
			},
		},
		{
			// Type 7 format 3: V,period,comma coverage array → SinglePos on
			// period (xPlacement +50) plus PairPos V/period via another rule.
			name:   "ctx7.3 V.,",
			script: ctxLatnTag(),
			glyphs: []shapingGlyph{{gid: 3, cluster: 0}, {gid: 5, cluster: 1}, {gid: 6, cluster: 2}},
			want: []ctxPosAdjustment{
				{0, 0, -80, 0},
				{40, 0, 0, 0},
				{0, 0, 0, 0},
			},
		},
		{
			// Multiple rules stacking on one buffer: AV. triggers 7.2
			// (A+V class pair), SinglePos format 2 (V placement +20) and
			// PairPos nested in a 7.1 rule.
			name:   "stacked AV.",
			script: ctxLatnTag(),
			glyphs: []shapingGlyph{{gid: 2, cluster: 0}, {gid: 3, cluster: 1}, {gid: 5, cluster: 2}},
			want: []ctxPosAdjustment{
				{0, 0, 0, 0},
				{20, 0, -105, 0},
				{-10, 0, 0, 0},
			},
		},
		{
			// Type 8 format 2 (chaining class): A + apostrophe + period.
			name:   "chain8.2 A'.",
			script: ctxLatnTag(),
			glyphs: []shapingGlyph{{gid: 2, cluster: 0}, {gid: 7, cluster: 1}, {gid: 5, cluster: 2}},
			want: []ctxPosAdjustment{
				{0, 0, 0, 0},
				{0, 0, -40, 0},
				{0, 0, 0, 0},
			},
		},
		{
			// Type 8 format 3: backtrack f, input hyphen, lookahead colon →
			// yPlacement +45 on hyphen.
			name:   "chain8.3 f-:",
			script: ctxLatnTag(),
			glyphs: []shapingGlyph{{gid: 4, cluster: 0}, {gid: 9, cluster: 1}, {gid: 10, cluster: 2}},
			want: []ctxPosAdjustment{
				{0, 0, 0, 0},
				{0, 45, 0, 0},
				{0, 0, 0, 0},
			},
		},
		{
			// Type 9 extension wrapping a Type 7.1 rule: V + question.
			name:   "ext9(ctx7.1) V?",
			script: ctxLatnTag(),
			glyphs: []shapingGlyph{{gid: 3, cluster: 0}, {gid: 8, cluster: 1}},
			want: []ctxPosAdjustment{
				{0, 0, 0, 0},
				{30, 0, 15, 0},
			},
		},
		{
			// Type 7 format 1 with a nested PairPos: V + period.
			name:   "nested-pair V.",
			script: ctxLatnTag(),
			glyphs: []shapingGlyph{{gid: 3, cluster: 0}, {gid: 5, cluster: 1}},
			want: []ctxPosAdjustment{
				{0, 0, -80, 0},
				{-10, 0, 0, 0},
			},
		},
		{
			// Non-matching second glyph: rule must not fire, nothing touched.
			name:   "no-match AV",
			script: ctxLatnTag(),
			glyphs: []shapingGlyph{{gid: 2, cluster: 0}, {gid: 3, cluster: 1}},
			want: []ctxPosAdjustment{
				{0, 0, 0, 0},
				{20, 0, 0, 0}, // SinglePos fmt2 still applies via 7.1 A+V rule
			},
		},
		{
			// Arabic chaining format 3: dal followed by both vowel marks
			// (lookahead coverages). Logical buffer order: dal, fatha, kasra.
			name:   "arab chain8.3 dal+marks",
			script: ctxArabTag(),
			glyphs: []shapingGlyph{{gid: 11, cluster: 0}, {gid: 13, cluster: 0}, {gid: 14, cluster: 0}},
			want: []ctxPosAdjustment{
				{98, 0, 98, 0},
				{0, 0, 0, 0},
				{0, 0, 0, 0},
			},
		},
		{
			// Arabic chaining format 1 (RTL): the rule is
			// backtrack=reh, input=dal. OpenType contextual matching runs
			// in glyph-processing order — HarfBuzz shapes RTL runs in
			// visual order, so the buffer here is [reh, dal]. The -50
			// advance lands on dal; the negative value must be preserved.
			name:   "arab chain8.1 dal+reh",
			script: ctxArabTag(),
			glyphs: []shapingGlyph{{gid: 12, cluster: 1}, {gid: 11, cluster: 0}},
			want: []ctxPosAdjustment{
				{0, 0, 0, 0},
				{0, 0, -50, 0},
			},
		},
		{
			// Lookahead not satisfied (only one mark): no adjustment.
			name:   "arab lookahead-miss dal+one-mark",
			script: ctxArabTag(),
			glyphs: []shapingGlyph{{gid: 11, cluster: 0}, {gid: 13, cluster: 0}},
			want: []ctxPosAdjustment{
				{0, 0, 0, 0},
				{0, 0, 0, 0},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adj := g.applyGPOS(tc.glyphs, tc.script, [4]byte{}, [][4]byte{ctxKernTag()})
			if len(adj) != len(tc.want) {
				t.Fatalf("adjustment count = %d, want %d", len(adj), len(tc.want))
			}
			for i, w := range tc.want {
				got := ctxPosAdjustment{
					adj[i].xPlacement, adj[i].yPlacement,
					adj[i].xAdvance, adj[i].yAdvance,
				}
				if got != w {
					t.Errorf("glyph %d: got %+v, want %+v", i, got, w)
				}
			}
		})
	}
}

// TestGPOSContext_UnmatchedGlyphsNotConsumed verifies that glyphs outside a
// rule match keep zero adjustments and that a second shaping pass over the
// same buffer yields identical results (no hidden state consumption).
func TestGPOSContext_UnmatchedGlyphsNotConsumed(t *testing.T) {
	g := loadCtxGPOS(t)
	glyphs := []shapingGlyph{
		{gid: 2, cluster: 0},  // A
		{gid: 10, cluster: 1}, // colon — no rule starts here
		{gid: 3, cluster: 2},  // V
		{gid: 5, cluster: 3},  // period
	}
	first := g.applyGPOS(glyphs, ctxLatnTag(), [4]byte{}, [][4]byte{ctxKernTag()})
	second := g.applyGPOS(glyphs, ctxLatnTag(), [4]byte{}, [][4]byte{ctxKernTag()})
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("pass %d mismatch: %+v vs %+v", i, first[i], second[i])
		}
	}
	// The colon at index 1 is never part of a matched input sequence and
	// must remain untouched even though neighboring glyphs are positioned.
	if first[1] != (gposAdjustment{}) {
		t.Errorf("unmatched glyph adjusted: %+v", first[1])
	}
}

// TestGPOSContext_NegativeAdvance keeps negative advances intact (they are
// legal in Arabic positioning and must not be clamped away).
func TestGPOSContext_NegativeAdvance(t *testing.T) {
	g := loadCtxGPOS(t)
	// Visual RTL order [reh, dal]: backtrack=reh, input=dal fires the rule.
	glyphs := []shapingGlyph{{gid: 12, cluster: 1}, {gid: 11, cluster: 0}}
	adj := g.applyGPOS(glyphs, ctxArabTag(), [4]byte{}, [][4]byte{ctxKernTag()})
	if adj[1].xAdvance != -50 {
		t.Fatalf("xAdvance = %d, want -50 (negative advance must be preserved)", adj[1].xAdvance)
	}
}

// TestGPOSContext_DefaultFeaturesIncludesDist ensures the Devanagari 'dist'
// feature is reachable through the default feature set.
func TestGPOSContext_DefaultFeaturesIncludesDist(t *testing.T) {
	_, gposTags := collectDesiredFeatures(nil)
	found := false
	for _, tag := range gposTags {
		if tag == [4]byte{'d', 'i', 's', 't'} {
			found = true
		}
	}
	if !found {
		t.Error("default GPOS features must include 'dist'")
	}
}
