package text

import (
	"os"
	"testing"
)

// gsubFixtureSource loads one of the generated contextual GSUB fixture fonts.
// Tests skip when the fixture is absent so the package remains testable in
// environments without the generated fonts.
func gsubFixtureCache(t *testing.T, name string) *ownShaperCache {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Skipf("fixture %s not available: %v", name, err)
	}
	source, err := NewFontSource(data, WithParser("own"))
	if err != nil {
		t.Fatalf("NewFontSource(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = source.Close() })

	shaper := NewOwnShaper()
	sc := shaper.getOrCreateCache(source)
	if sc == nil || sc.gsub == nil {
		t.Fatalf("fixture %s has no parsed GSUB", name)
	}
	return sc
}

// runGSUBFeature shapes text with exactly one GSUB feature tag enabled,
// bypassing the default feature set so golden sequences isolate one lookup.
// Returns the resulting glyph IDs and the error recorded by the engine.
func runGSUBFeature(t *testing.T, sc *ownShaperCache, text, script string, tag [4]byte) ([]uint16, error) {
	t.Helper()
	runes := []rune(text)
	glyphs := runeToGlyphs(runes, sc)
	var scriptTag [4]byte
	switch script {
	case "arab":
		scriptTag = [4]byte{'a', 'r', 'a', 'b'}
	default:
		scriptTag = [4]byte{'l', 'a', 't', 'n'}
	}
	glyphs = sc.gsub.applyGSUB(glyphs, scriptTag, [4]byte{}, [][4]byte{tag})
	out := make([]uint16, len(glyphs))
	for i, g := range glyphs {
		out[i] = g.gid
	}
	return out, sc.gsub.applyGSUBError()
}

// gsubTag builds a 4-byte OpenType tag from a 4-character string.
func gsubTag(s string) [4]byte {
	var tag [4]byte
	copy(tag[:], s)
	return tag
}

// TestGSUBContext_LatinGolden compares the engine against HarfBuzz 15 golden
// sequences captured with every other feature disabled (golden_latin.py).
func TestGSUBContext_LatinGolden(t *testing.T) {
	sc := gsubFixtureCache(t, "gsub_context_latin.ttf")

	cases := []struct {
		name string
		text string
		tag  string
		want []uint16
	}{
		// Context formats 1/2/3.
		{"context_f1", "abc", "calt", []uint16{712, 713, 70}},
		{"context_f2_class", "ab", "clig", []uint16{712, 713}},
		{"context_f3_coverage", "ab", "salt", []uint16{712, 69}},
		// Plain ligature non-regression.
		{"ligature", "fi", "liga", []uint16{718}},
		{"ligature_word", "office", "liga", []uint16{82, 73, 718, 70, 72}},
		// Chain formats 1/2/3, positive and negative.
		{"chain_f1_match", "xaby", "dlig", []uint16{91, 712, 713, 92}},
		{"chain_f1_no_backtrack", "aby", "dlig", []uint16{68, 69, 92}},
		{"chain_f1_wrong_backtrack", "axby", "dlig", []uint16{68, 91, 69, 92}},
		{"chain_f2_class", "xaby", "cswh", []uint16{91, 712, 713, 92}},
		{"chain_f3_coverage", "xaby", "ccmp", []uint16{91, 712, 713, 92}},
		// Reverse chain single format 1, positive and negative.
		{"reverse_f1_match", "xe", "rvrn", []uint16{91, 716}},
		{"reverse_f1_negative", "e", "rvrn", []uint16{72}},
		// Reverse chain "format 3" subtable was compiled empty by fontTools;
		// the engine must stop safely and leave the buffer unchanged.
		{"reverse_unknown_safe_stop", "zey", "rclt", []uint16{93, 72, 92}},
		{"reverse_unknown_negative", "ey", "rclt", []uint16{72, 92}},
		// IgnoreMarks: marks between ligature components are skipped.
		{"ignore_marks_two_marks", "a\ue000c\ue000m", "med2", []uint16{68, 720, 717, 720}},
		{"ignore_marks_plain", "acm", "med2", []uint16{68, 717}},
		{"ignore_marks_no_anchor", "cm", "med2", []uint16{70, 80}},
		// Mark filtering sets: direct lookups enforce the set, nested
		// recursion reproduces HarfBuzz's coverage-only behavior.
		{"mark_filter_base_path", "\ue000de", "init", []uint16{720, 715, 72}},
		{"nested_mark_in_set", "a\ue000", "medi", []uint16{68, 713}},
		{"nested_mark_outside_set_fires", "a\ue009", "mset", []uint16{68, 714}},
		{"direct_mark_outside_set_blocked", "a\ue009", "mk3f", []uint16{68, 721}},
		{"direct_mark_in_set", "a\ue000", "mk2f", []uint16{68, 713}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runGSUBFeature(t, sc, tc.text, "latn", gsubTag(tc.tag))
			if err != nil {
				t.Fatalf("unexpected engine error: %v", err)
			}
			if !uint16Equal(got, tc.want) {
				t.Errorf("GSUB %s on %q:\n got %v\nwant %v", tc.tag, tc.text, got, tc.want)
			}
		})
	}
}

// TestGSUBContext_ArabicGolden exercises RightToLeft chain context lookups
// (formats 1/2/3) producing Arabic presentation forms. Golden sequences are
// in logical order and were captured from HarfBuzz 15.
func TestGSUBContext_ArabicGolden(t *testing.T) {
	sc := gsubFixtureCache(t, "gsub_context_arabic.ttf")

	cases := []struct {
		tag  string
		text string
		want []uint16
	}{
		{"cf01", "ا", []uint16{712}},
		{"cf01", "اب", []uint16{717, 713}},
		{"cf01", "ابم", []uint16{717, 717, 714}},
		{"cf01", "ابمل", []uint16{717, 717, 717, 715}},
		{"cf02", "اب", []uint16{717, 713}},
		{"cf02", "ابمل", []uint16{717, 717, 717, 715}},
		{"cf03", "اب", []uint16{717, 713}},
		{"cf03", "ابمل", []uint16{717, 717, 717, 715}},
	}
	for _, tc := range cases {
		t.Run(tc.tag+"_"+tc.text, func(t *testing.T) {
			got, err := runGSUBFeature(t, sc, tc.text, "arab", gsubTag(tc.tag))
			if err != nil {
				t.Fatalf("unexpected engine error: %v", err)
			}
			if !uint16Equal(got, tc.want) {
				t.Errorf("GSUB %s on %q:\n got %v\nwant %v", tc.tag, tc.text, got, tc.want)
			}
		})
	}
}

// TestGSUBContext_LookupCycle ensures a self-referencing contextual lookup
// terminates with the nesting error instead of looping forever.
func TestGSUBContext_LookupCycle(t *testing.T) {
	sc := gsubFixtureCache(t, "gsub_context_latin.ttf")
	got, err := runGSUBFeature(t, sc, "aa", "latn", gsubTag("rlig"))
	if err == nil {
		t.Fatalf("expected nesting/cycle error, got nil; glyphs=%v", got)
	}
	if len(got) > 4 {
		t.Errorf("cycle guard left %d glyphs, want a small bounded buffer", len(got))
	}
}

func uint16Equal(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
