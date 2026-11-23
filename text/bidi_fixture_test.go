package text

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertVisualOrderMatchesReference drives the full layout pipeline for one
// paragraph and verifies that the visual glyph sequence on every line is a
// faithful rendering of the UAX #9 rule-L2 reference order:
//
//   - glyphs appear in the same left-to-right sequence as the reference
//     reorder array restricted to source runes that produced glyphs;
//   - glyphs belonging to one cluster are never split across another cluster;
//   - X coordinates are non-negative and (up to kerning epsilon) non-decreasing.
//
// The reference array itself is validated against the official
// BidiCharacterTest.txt conformance suite in internal/bidi.
func assertVisualOrderMatchesReference(t *testing.T, para string, dir Direction) {
	t.Helper()
	face := layoutTestFace(t)
	opts := LayoutOptions{Direction: dir, LineSpacing: 1.0}
	layout := LayoutText(para, face, opts)
	if layout == nil || len(layout.Lines) == 0 {
		t.Fatalf("no layout for %q", para)
	}

	pb := resolveParagraphLevels(para, dir)

	for lineIdx := range layout.Lines {
		line := &layout.Lines[lineIdx]
		monotonicX(t, line)
		if len(line.Glyphs) == 0 {
			continue
		}

		// Reference visual order of rune indices for this line's span.
		ls, le := lineRuneSpan(line.Runs)
		ref := pb.resolver.LineReorder(ls, le)
		if len(ref) == 0 {
			continue
		}

		// Which runes actually produced a glyph?
		glyphRunes := make(map[int]bool)
		for _, g := range line.Glyphs {
			glyphRunes[pb.runeIndexForByteOffset(g.Cluster)] = true
		}

		// Expected visual rune order = reference order filtered to glyph runes.
		var expected []int
		for _, runeIdx := range ref {
			if glyphRunes[runeIdx] {
				expected = append(expected, runeIdx)
			}
		}

		// Actual visual rune order from the laid-out glyph clusters. Consecutive
		// glyphs sharing a rune collapse to one entry.
		var actual []int
		prevRune := -1
		for _, g := range line.Glyphs {
			runeIdx := pb.runeIndexForByteOffset(g.Cluster)
			if runeIdx != prevRune {
				actual = append(actual, runeIdx)
				prevRune = runeIdx
			}
		}

		if !intSlicesEqual(actual, expected) {
			t.Errorf("line %d of %q (dir=%v): visual rune order = %v, want %v",
				lineIdx, para, dir, actual, expected)
		}
	}
}

// TestBidiFixture_ArabicWithLatinDigits covers Arabic text embedding Latin
// digits and currency-like neutral symbols in both base directions.
func TestBidiFixture_ArabicWithLatinDigits(t *testing.T) {
	fixtures := []string{
		"السعر 123 دولار",         // price 123 dollars
		"مكتب 42 شارع",            // office 42 street
		"العدد 3.14 تقريبا",       // number 3.14 approximately
		"اشتريت 5 كتب بـ 90 ريال", // bought 5 books for 90 riyal
	}
	for _, dir := range []Direction{DirectionLTR, DirectionRTL} {
		for _, para := range fixtures {
			assertVisualOrderMatchesReference(t, para, dir)
		}
	}
}

// TestBidiFixture_HebrewPunctuation covers Hebrew with question marks,
// exclamation, colons and paired brackets.
func TestBidiFixture_HebrewPunctuation(t *testing.T) {
	fixtures := []string{
		"שלום! מה קורה?",
		"מחיר: 100",
		"אב(גד)קו",
		"שלום (עולם) הערב",
		"האם זה נכון: כן או לא?",
	}
	for _, dir := range []Direction{DirectionLTR, DirectionRTL} {
		for _, para := range fixtures {
			assertVisualOrderMatchesReference(t, para, dir)
		}
	}
}

// TestBidiFixture_Multiline verifies independent per-line reordering across
// hard line breaks mixing LTR and RTL lines.
func TestBidiFixture_Multiline(t *testing.T) {
	para := strings.Join([]string{
		"مرحبا Hello",
		"Hello مرحبا",
		"שלום 123",
		"plain LTR line",
		"عربي (abc) نص",
	}, "\n")

	face := layoutTestFace(t)
	layout := LayoutText(para, face, LayoutOptions{Direction: DirectionLTR, LineSpacing: 1.0})
	if len(layout.Lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(layout.Lines))
	}

	paragraphs := strings.Split(para, "\n")
	for i, line := range layout.Lines {
		// Each line must be independently valid against its own reference.
		pb := resolveParagraphLevels(paragraphs[i], DirectionLTR)
		monotonicX(t, &layout.Lines[i])
		if len(line.Glyphs) == 0 {
			continue
		}
		ls, le := lineRuneSpan(line.Runs)
		ref := pb.resolver.LineReorder(ls, le)
		glyphRunes := map[int]bool{}
		for _, g := range line.Glyphs {
			glyphRunes[pb.runeIndexForByteOffset(g.Cluster)] = true
		}
		var expected []int
		for _, runeIdx := range ref {
			if glyphRunes[runeIdx] {
				expected = append(expected, runeIdx)
			}
		}
		var actual []int
		prev := -1
		for _, g := range line.Glyphs {
			ri := pb.runeIndexForByteOffset(g.Cluster)
			if ri != prev {
				actual = append(actual, ri)
				prev = ri
			}
		}
		if !intSlicesEqual(actual, expected) {
			t.Errorf("line %d %q: order %v want %v", i, paragraphs[i], actual, expected)
		}
	}
}

// TestBidiFixture_FromFile runs additional hand-authored fixtures stored in
// testdata, one paragraph per line (lines beginning with # are comments).
// This makes it easy to extend reference-order coverage without code changes.
func TestBidiFixture_FromFile(t *testing.T) {
	path := filepath.Join("testdata", "bidi_fixtures.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fixture file not present: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Each fixture line: direction|paragraph
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			t.Fatalf("malformed fixture %q (want dir|text)", line)
		}
		dir := DirectionLTR
		if strings.TrimSpace(parts[0]) == "RTL" {
			dir = DirectionRTL
		}
		assertVisualOrderMatchesReference(t, parts[1], dir)
	}
}
