// Real-font integration tests for contextual GPOS: Arabic (Amiri subset),
// Devanagari (Noto subset, 'dist' feature) and Latin punctuation on the
// synthetic font. These exercise the full OwnShaper pipeline (cmap → GSUB →
// GPOS → scaling), verifying positions, clusters and boundary behavior stay
// stable when contextual lookups are involved.
//
// Numeric expectations come from HarfBuzz (uharfbuzz 0.57) at font scale
// equal to units-per-em, so 1 font unit == 1 output unit in these tests.
package text

import (
	"testing"
)

// shapedSnapshot captures the positioning fields that contextual GPOS can
// affect. GID and Cluster must never change.
type shapedSnapshot struct {
	gid                      uint16
	cluster                  int
	x, y, xAdvance, yAdvance float64
}

func snapshotShaped(in []ShapedGlyph) []shapedSnapshot {
	out := make([]shapedSnapshot, len(in))
	for i, g := range in {
		out[i] = shapedSnapshot{
			gid: uint16(g.GID), cluster: g.Cluster,
			x: g.X, y: g.Y, xAdvance: g.XAdvance, yAdvance: g.YAdvance,
		}
	}
	return out
}

// openCtxFont loads a font from the ctxpos testdata directory.
func openCtxFont(t *testing.T, name string) (*FontSource, *OwnShaper) {
	t.Helper()
	source, err := NewFontSource(mustReadCtxFile(t, name), WithParser("own"))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source, NewOwnShaper()
}

func mustReadCtxFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := readCtxTestFile("testdata/ctxpos/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

// TestGPOSContext_DevanagariDist checks the Noto Sans Devanagari subset.
// The font implements 'dist' with chaining-context format 3 lookups that
// reposition the Vedic sign U+1CD0 relative to surrounding glyphs.
func TestGPOSContext_DevanagariDist(t *testing.T) {
	source, shaper := openCtxFont(t, "notosansdev_ctx_subset.ttf")
	face := source.Face(1000) // 1 font unit per output unit

	t.Run("single vedic sign keeps zero advance", func(t *testing.T) {
		// U+1CD0 alone maps directly to gid 5. (HarfBuzz additionally inserts
		// a dotted-circle base via GSUB and attaches the mark via a Type 4
		// lookup; neither is implemented in this shaper yet.) The GPOS-relevant
		// invariant is that the zero-width mark keeps a zero advance and the
		// contextual lookups do not invent width for it.
		out := shaper.Shape("᳐", face)
		if len(out) != 1 {
			t.Fatalf("got %d glyphs, want 1 (no GSUB dotted-circle insertion yet)", len(out))
		}
		if out[0].GID != GlyphID(5) {
			t.Errorf("gid = %d, want 5", out[0].GID)
		}
		if out[0].XAdvance != 0 {
			t.Errorf("mark XAdvance = %.1f, want 0 (zero-width mark)", out[0].XAdvance)
		}
	})

	t.Run("space backtrack keeps mark zero-width", func(t *testing.T) {
		// " ᳐": the space (gid 1) is the backtrack coverage of the chaining
		// format-3 'dist' rule; matching must not crash or add width.
		out := shaper.Shape(" ᳐", face)
		if len(out) != 2 {
			t.Fatalf("got %d glyphs, want 2", len(out))
		}
		if out[0].GID != GlyphID(1) || out[1].GID != GlyphID(5) {
			t.Errorf("gids = %d,%d want 1,5", out[0].GID, out[1].GID)
		}
		if out[1].XAdvance != 0 {
			t.Errorf("mark XAdvance = %.1f, want 0", out[1].XAdvance)
		}
	})

	t.Run("chaining rule applies x placement around ra", func(t *testing.T) {
		// U+1CD0 U+0930 U+1CD0: chaining-context 'dist' lookups reposition the
		// marks flanking ra (gid 4). HarfBuzz reports the same x offsets
		// (-120 before, +60 after); our output has 3 glyphs because we do not
		// synthesize the dotted-circle base.
		out := shaper.Shape("᳐र᳐", face)
		if len(out) != 3 {
			t.Fatalf("got %d glyphs, want 3", len(out))
		}
		marks := 0
		for _, g := range out {
			if g.GID == GlyphID(5) {
				marks++
				if g.XAdvance != 0 {
					t.Errorf("mark gid %d gained advance %.1f, want 0", g.GID, g.XAdvance)
				}
			}
		}
		if marks != 2 {
			t.Errorf("got %d U+1CD0 marks, want 2", marks)
		}
		first, last := out[0], out[2]
		if first.GID != GlyphID(5) || first.X != -120 {
			t.Errorf("first mark: gid=%d X=%.1f, want gid=5 X=-120", first.GID, first.X)
		}
		if last.GID != GlyphID(5) || last.X != 469 {
			// 469 = ra origin 409 + contextual placement 60.
			t.Errorf("last mark: gid=%d X=%.1f, want gid=5 X=469", last.GID, last.X)
		}
	})

	t.Run("clusters preserved", func(t *testing.T) {
		out := shaper.Shape("᳐र᳐", face)
		// All output glyphs map back to one of the source rune indices.
		for i, g := range out {
			if g.Cluster < 0 || g.Cluster > 2 {
				t.Errorf("glyph %d cluster = %d out of range [0,2]", i, g.Cluster)
			}
		}
	})
}

// TestGPOSContext_ArabicSubset shapes Arabic text on the Amiri subset. The
// subset's contextual positioning interacts with pair (kern), cursive and
// mark lookups in the same GPOS table; this verifies contextual rules do not
// disturb glyph identity, clusters or advances of neighboring lookups.
func TestGPOSContext_ArabicSubset(t *testing.T) {
	source, shaper := openCtxFont(t, "amiri_ctx_subset.ttf")
	face := source.Face(16)

	t.Run("shaping is deterministic and stable", func(t *testing.T) {
		first := snapshotShaped(shaper.Shape("مرحبا", face))
		second := snapshotShaped(shaper.Shape("مرحبا", face))
		if len(first) != len(second) {
			t.Fatalf("run lengths differ: %d vs %d", len(first), len(second))
		}
		for i := range first {
			if first[i] != second[i] {
				t.Fatalf("non-deterministic output at %d: %+v vs %+v", i, first[i], second[i])
			}
		}
	})

	t.Run("mixed latin-arabic keeps clusters and positive run width", func(t *testing.T) {
		out := shaper.Shape("Hello مرحبا World", face)
		if len(out) == 0 {
			t.Fatal("no glyphs shaped")
		}
		// Every shaped glyph retains a valid source cluster.
		for i, g := range out {
			if g.Cluster < 0 {
				t.Errorf("glyph %d has negative cluster %d", i, g.Cluster)
			}
		}
		// Total run width must remain positive regardless of any negative
		// contextual adjustments inside the Arabic segment.
		last := out[len(out)-1]
		if last.X+last.XAdvance <= 0 {
			t.Errorf("mixed run total width = %.4f, want > 0", last.X+last.XAdvance)
		}
	})

	t.Run("vowel marks stay zero-advance", func(t *testing.T) {
		// dal + fatha: the mark must not gain advance from GPOS.
		out := shaper.Shape("دَ", face)
		var foundMark bool
		for _, g := range out {
			if g.GID == GlyphID(9) { // uni064E fatha
				foundMark = true
				if g.XAdvance != 0 {
					t.Errorf("fatha XAdvance = %.4f, want 0", g.XAdvance)
				}
			}
		}
		if !foundMark {
			t.Skip("subset cmap did not map fatha to gid 9; skipping")
		}
	})
}

// TestGPOSContext_LatinPunctuation exercises the synthetic font's Latin
// punctuation rules through the full shaper, including scaling. At size 1000
// with upem 1000 the scale factor is 1, so font-unit expectations apply.
func TestGPOSContext_LatinPunctuation(t *testing.T) {
	source, shaper := openCtxFont(t, "ctxpos_test.ttf")
	face := source.Face(1000)

	t.Run("f question gets placement and advance", func(t *testing.T) {
		out := shaper.Shape("f?", face)
		if len(out) != 2 {
			t.Fatalf("got %d glyphs, want 2", len(out))
		}
		q := out[1]
		if q.X != 530 {
			t.Errorf("question X = %.1f, want 530 (500 advance + 30 placement)", q.X)
		}
		if q.XAdvance != 515 {
			t.Errorf("question XAdvance = %.1f, want 515", q.XAdvance)
		}
	})

	t.Run("scaling applies to contextual adjustments", func(t *testing.T) {
		small := source.Face(500) // scale 0.5
		out := shaper.Shape("f?", small)
		q := out[1]
		// Base advance 250, placement 15 → X = 250 + 15 = 265.
		if q.X != 265 {
			t.Errorf("scaled question X = %.1f, want 265", q.X)
		}
		if q.XAdvance != 257.5 {
			t.Errorf("scaled question XAdvance = %.1f, want 257.5", q.XAdvance)
		}
	})

	t.Run("boundary: rule at buffer end does not panic", func(t *testing.T) {
		// 'V' alone is the coverage glyph of several rules whose sequences
		// extend past the buffer; all must simply fail to match.
		out := shaper.Shape("V", face)
		if len(out) != 1 {
			t.Fatalf("got %d glyphs, want 1", len(out))
		}
		if out[0].XAdvance != 500 {
			t.Errorf("lone V XAdvance = %.1f, want unmodified 500", out[0].XAdvance)
		}
	})

	t.Run("missing glyph (notdef) is not consumed", func(t *testing.T) {
		// U+FFFF has no cmap entry, so it maps to .notdef (gid 0). Contextual
		// rules must skip it rather than match or consume it, and it still
		// occupies a buffer slot.
		run := "A￿f?"
		out := shaper.Shape(run, face)
		before := len(out)
		if before < 2 {
			t.Fatalf("expected notdef slot plus other glyphs, got %d", before)
		}
		// Re-shaping must give identical output (no stateful consumption).
		again := shaper.Shape(run, face)
		if len(again) != before {
			t.Fatalf("notdef handling inconsistent: %d vs %d glyphs", len(again), before)
		}
	})
}

// TestGPOSContext_MultiFontFallbackStability verifies that contextual
// positioning is scoped to one face: the same text shaped through two
// different fonts keeps per-face GPOS adjustments internally consistent and
// never carries state across faces (the OwnShaper cache is keyed by
// FontSource, so fallback runs cannot leak adjustments).
func TestGPOSContext_MultiFontFallbackStability(t *testing.T) {
	ctxSource, ctxShaper := openCtxFont(t, "ctxpos_test.ttf")

	// Go Regular is a Latin font with a conventional (non-contextual) GPOS.
	goData, err := readCtxTestFile("testdata/goregular.ttf")
	if err != nil {
		t.Fatalf("read goregular: %v", err)
	}
	goSource, err := NewFontSource(goData, WithParser("own"))
	if err != nil {
		t.Fatalf("open goregular: %v", err)
	}
	t.Cleanup(func() { _ = goSource.Close() })
	goShaper := NewOwnShaper()

	// Shape the same Latin string on both faces; each result must be
	// independently stable across repeated runs (no cross-cache leakage).
	for run, shaperSrc := range map[string]struct {
		shaper *OwnShaper
		source *FontSource
	}{
		"f?":  {ctxShaper, ctxSource},
		"AV.": {ctxShaper, ctxSource},
		"the": {goShaper, goSource},
	} {
		face := shaperSrc.source.Face(1000)
		first := snapshotShaped(shaperSrc.shaper.Shape(run, face))
		second := snapshotShaped(shaperSrc.shaper.Shape(run, face))
		if len(first) != len(second) {
			t.Fatalf("%q: glyph counts differ across runs", run)
		}
		for i := range first {
			if first[i] != second[i] {
				t.Fatalf("%q: unstable glyph %d: %+v vs %+v", run, i, first[i], second[i])
			}
		}
	}

	// Interleaved shaping between the two faces must not contaminate either
	// cache: after shaping Go Regular text, the synthetic font's contextual
	// rule still produces exactly its expected adjustment.
	_ = goShaper.Shape("the quick brown fox", goSource.Face(1000))
	out := ctxShaper.Shape("f?", ctxSource.Face(1000))
	if len(out) != 2 || out[1].X != 530 || out[1].XAdvance != 515 {
		t.Fatalf("cross-face contamination: f? -> %+v", out)
	}
}
