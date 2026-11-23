package bidix

import (
	"testing"

	"golang.org/x/text/unicode/bidi"
)

// TestParityWithUpstream verifies the vendored engine against
// golang.org/x/text/unicode/bidi: level parity must match per rune. Full L2
// visual order is covered exhaustively by TestUAX9Conformance against the
// official BidiCharacterTest.txt suite; the upstream public API only exposes
// run parity, not the multi-pass L2 reordering.
func TestParityWithUpstream(t *testing.T) {
	cases := []struct {
		name string
		text string
		base bidi.Direction
	}{
		{"latin", "Hello World", bidi.Neutral},
		{"arabic sandwich", "Hello مرحبا World", bidi.Neutral},
		{"hebrew numbers", "מחיר: 100", bidi.Neutral},
		{"rtl base latin", "مرحبا Hello", bidi.RightToLeft},
		{"arabic latin digits", "السعر 123 دولار", bidi.Neutral},
		{"brackets ltr", "a(b)c", bidi.Neutral},
		{"brackets rtl", "אב(גד)קו", bidi.Neutral},
		{"nested embeds", "a‪b⁩c‫d⁩e", bidi.Neutral},
		{"isolates", "⁦LRI⁩ middle ⁧RLI⁩", bidi.Neutral},
		{"fsi", "⁨مرحبا⁩ end", bidi.Neutral},
		{"override", "a‮reversed‬z", bidi.Neutral},
		{"digits in arabic", "مكتب 42 شارع", bidi.Neutral},
		{"punctuation hebrew", "שלום! מה קורה?", bidi.Neutral},
		{"mixed punctuation", "Hello (world) مرحبا", bidi.Neutral},
		{"european number", "price 3.14 is good", bidi.Neutral},
		{"arabic number", "١٢٣٤", bidi.Neutral},
		{"trailing ws", "  tabs\tand spaces  ", bidi.Neutral},
		{"forced ltr", "مرحبا", bidi.LeftToRight},
		{"forced rtl", "Hello", bidi.RightToLeft},
		{"invalid utf8", "a\xffb\xc0\x80c", bidi.Neutral},
		{"combining marks", "áb̂ אְב", bidi.Neutral},
		{"canonical brackets", "a﹙b﹚c ［d］", bidi.Neutral},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := DirectionDefault
			switch tc.base {
			case bidi.LeftToRight:
				base = DirectionLeftToRight
			case bidi.RightToLeft:
				base = DirectionRightToLeft
			}

			r := NewResolver(tc.text, base)

			var p bidi.Paragraph
			_, err := p.SetString(tc.text, bidi.DefaultDirection(tc.base))
			if err != nil {
				t.Fatalf("upstream SetString: %v", err)
			}
			ordering, err := p.Order()
			if err != nil {
				t.Fatalf("upstream Order: %v", err)
			}

			// The reorder array must be a complete permutation.
			n := r.RuneCount()
			reorder := r.LineReorder(0, n)
			if len(reorder) != n {
				t.Fatalf("reorder len %d, want %d", len(reorder), n)
			}
			seen := make(map[int]bool, n)
			for _, idx := range reorder {
				if idx < 0 || idx >= n || seen[idx] {
					t.Fatalf("not a permutation: %v", reorder)
				}
				seen[idx] = true
			}

			// Level parity must match the upstream run direction.
			for i := 0; i < ordering.NumRuns(); i++ {
				run := ordering.Run(i)
				start, end := run.Pos() // inclusive rune indices
				wantOdd := run.Direction() == bidi.RightToLeft
				for j := start; j <= end; j++ {
					if got := r.IsRTLAt(j); got != wantOdd {
						t.Fatalf("rune %d: RTL=%v want %v (text=%q)",
							j, got, wantOdd, tc.text)
					}
				}
			}
		})
	}
}

// TestLineReorderSubset exercises L1 on line subsets with trailing spaces.
func TestLineReorderSubset(t *testing.T) {
	r := NewResolver("ab אבב ", DirectionDefault)
	n := r.RuneCount()
	full := r.LineReorder(0, n)
	if len(full) != n {
		t.Fatalf("got %d want %d", len(full), n)
	}
	seen := make(map[int]bool, n)
	for _, idx := range full {
		if seen[idx] {
			t.Fatalf("duplicate rune %d", idx)
		}
		seen[idx] = true
	}

	// Subranges return global logical indices (as documented), forming a
	// permutation of exactly the requested rune range.
	sub := r.LineReorder(1, 4)
	if len(sub) != 3 {
		t.Fatalf("subrange len %d want 3", len(sub))
	}
	got := map[int]bool{}
	for _, idx := range sub {
		if idx < 1 || idx >= 4 {
			t.Fatalf("index %d outside [1,4)", idx)
		}
		got[idx] = true
	}
	if len(got) != 3 {
		t.Fatalf("subrange not a permutation of [1,4): %v", sub)
	}

	if got := r.LineReorder(0, 0); got != nil {
		t.Fatalf("empty range should be nil, got %v", got)
	}
	if got := r.LineReorder(2, 1); got != nil {
		t.Fatalf("inverted range should be nil, got %v", got)
	}
}

// TestDeepEmbeddingBounded ensures very deep explicit nesting terminates
// deterministically without unbounded level growth and without panicking.
// Note: the reference implementation (upstream x/text, copied verbatim here)
// lets same-direction embeddings reach level 126 before its overflow
// handling engages; levels beyond 125 never appear and all further embeddings
// are ignored, so the output stays bounded and stable.
func TestDeepEmbeddingBounded(t *testing.T) {
	text := "x"
	for i := 0; i < 200; i++ {
		text += "‫" // RLE
	}
	text += "y"
	for i := 0; i < 200; i++ {
		text += "‬" // PDF
	}
	r := NewResolver(text, DirectionDefault)
	maxLevel := 0
	for i := 0; i < r.RuneCount(); i++ {
		if l := r.LevelAt(i); l > maxLevel {
			maxLevel = l
		}
	}
	if maxLevel > 126 {
		t.Fatalf("unbounded level growth: %d", maxLevel)
	}
	// The resolver must remain usable and produce a full permutation.
	reorder := r.LineReorder(0, r.RuneCount())
	if len(reorder) != r.RuneCount() {
		t.Fatalf("reorder len %d, want %d", len(reorder), r.RuneCount())
	}
}

// TestEmptyAndInvalidInputs covers empty strings and invalid UTF-8.
func TestEmptyAndInvalidInputs(t *testing.T) {
	r := NewResolver("", DirectionDefault)
	if r.RuneCount() != 0 {
		t.Fatalf("empty resolver rune count %d", r.RuneCount())
	}
	if r.ParagraphLevel() != 0 {
		t.Fatalf("empty paragraph level %d", r.ParagraphLevel())
	}
	if got := r.LineReorder(0, 0); got != nil {
		t.Fatalf("empty reorder = %v", got)
	}

	// Invalid UTF-8: RuneError gets a deterministic level and never panics.
	bad := NewResolver("a\xffb", DirectionDefault)
	if bad.RuneCount() != 3 {
		t.Fatalf("rune count = %d, want 3", bad.RuneCount())
	}
	for i := 0; i < bad.RuneCount(); i++ {
		l := bad.LevelAt(i)
		if l < 0 {
			t.Fatalf("rune %d has negative level %d", i, l)
		}
	}
	if len(bad.LineReorder(0, bad.RuneCount())) != 3 {
		t.Fatal("invalid UTF-8 reorder incomplete")
	}
}
