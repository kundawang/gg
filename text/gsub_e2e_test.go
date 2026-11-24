package text

import (
	"os"
	"testing"
)

// TestGSUBContext_EndToEnd_Latin drives the full OwnShaper.Shape pipeline
// (cmap -> GSUB engine -> GPOS -> positioning) on the Latin fixture and
// checks both glyph IDs and cluster metadata propagation.
func TestGSUBContext_EndToEnd_Latin(t *testing.T) {
	data, err := os.ReadFile("testdata/gsub_context_latin.ttf")
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	source, err := NewFontSource(data, WithParser("own"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	shaper := NewOwnShaper()

	t.Run("ligature_cluster_merge", func(t *testing.T) {
		// Default features include liga: "fi" -> g_fi (718), one glyph whose
		// cluster must be the earliest source cluster (0).
		face := source.Face(16)
		out := shaper.Shape("fi", face)
		if len(out) != 1 {
			t.Fatalf("Shape(fi): got %d glyphs, want 1", len(out))
		}
		if uint16(out[0].GID) != 718 {
			t.Errorf("GID=%d, want 718 (g_fi)", out[0].GID)
		}
		if out[0].Cluster != 0 {
			t.Errorf("ligature cluster=%d, want 0 (merged minimum)", out[0].Cluster)
		}
	})

	t.Run("context_ligature_keeps_mark_clusters", func(t *testing.T) {
		// med2: a + mark + c + mark + m -> a, mark, ligature, mark.
		face := source.Face(16, WithFeatures(NewFontFeature("med2", 1)))
		out := shaper.Shape("a\ue000c\ue000m", face)
		wantGIDs := []uint16{68, 720, 717, 720}
		if len(out) != len(wantGIDs) {
			t.Fatalf("got %d glyphs, want %d: %+v", len(out), len(wantGIDs), out)
		}
		for i, want := range wantGIDs {
			if uint16(out[i].GID) != want {
				t.Errorf("glyph %d GID=%d, want %d", i, out[i].GID, want)
			}
		}
		// The ligature spans c (cluster 2) through m (cluster 4); it
		// inherits the minimum cluster in that span (2). The anchor 'a'
		// is outside the ligature and keeps cluster 0.
		if out[2].Cluster != 2 {
			t.Errorf("ligature cluster=%d, want 2", out[2].Cluster)
		}
	})

	t.Run("feature_disabled_leaves_input", func(t *testing.T) {
		// With every default GSUB feature off, "abc" must stay at its raw
		// cmap glyphs and keep sequential clusters.
		face := source.Face(16, WithFeatures(
			NewFontFeature("ccmp", 0),
			NewFontFeature("liga", 0),
			NewFontFeature("clig", 0),
			NewFontFeature("rlig", 0),
			NewFontFeature("dlig", 0),
			NewFontFeature("calt", 0),
		))
		out := shaper.Shape("abc", face)
		wantGIDs := []uint16{68, 69, 70}
		if len(out) != len(wantGIDs) {
			t.Fatalf("got %d glyphs, want %d", len(out), len(wantGIDs))
		}
		for i, want := range wantGIDs {
			if uint16(out[i].GID) != want {
				t.Errorf("glyph %d GID=%d, want %d", i, out[i].GID, want)
			}
			if out[i].Cluster != i {
				t.Errorf("glyph %d cluster=%d, want %d", i, out[i].Cluster, i)
			}
		}
	})

	t.Run("reverse_chain_negative", func(t *testing.T) {
		// rvrn only substitutes "e" when "x" backtracks; standalone "e"
		// must survive unchanged through the full pipeline.
		face := source.Face(16, WithFeatures(NewFontFeature("rvrn", 1)))
		out := shaper.Shape("e", face)
		if len(out) != 1 || uint16(out[0].GID) != 72 {
			t.Fatalf("Shape(e) = %+v, want unchanged gid 72", out)
		}
	})
}

// TestGSUBContext_EndToEnd_Arabic exercises the RightToLeft chain lookups via
// the public shaping API. Glyph clusters stay in logical order even though
// the font marks its lookups RightToLeft.
func TestGSUBContext_EndToEnd_Arabic(t *testing.T) {
	data, err := os.ReadFile("testdata/gsub_context_arabic.ttf")
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	source, err := NewFontSource(data, WithParser("own"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	shaper := NewOwnShaper()
	// Disable the default isolated-form feature (ccmp) so only the chain
	// lookup under cf01 contributes.
	face := source.Face(16, WithFeatures(
		NewFontFeature("ccmp", 0),
		NewFontFeature("cf01", 1),
	))

	out := shaper.Shape("اب", face) // alef (cluster 0) + beh (cluster 1)
	if len(out) != 2 {
		t.Fatalf("got %d glyphs, want 2: %+v", len(out), out)
	}
	// Logical order: init-form alef (717) then base beh (713).
	wantGIDs := []uint16{717, 713}
	for i, want := range wantGIDs {
		if uint16(out[i].GID) != want {
			t.Errorf("glyph %d GID=%d, want %d", i, out[i].GID, want)
		}
	}
	if out[0].Cluster != 0 || out[1].Cluster != 1 {
		t.Errorf("clusters = [%d %d], want [0 1]", out[0].Cluster, out[1].Cluster)
	}
}
