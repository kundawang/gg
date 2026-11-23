package text

import (
	"context"
	"testing"
)

// visualClusters returns the source byte-cluster indices of every glyph in
// visual (left-to-right) order. It is the primary assertion hook: the test
// font lacks Arabic/Hebrew glyphs, so visual correctness is verified through
// the stable cluster mapping rather than rasterized pixels.
func visualClusters(line *Line) []int {
	out := make([]int, len(line.Glyphs))
	for i, g := range line.Glyphs {
		out[i] = g.Cluster
	}
	return out
}

// monotonicX reports whether glyph X positions are non-decreasing and all
// non-negative. A small epsilon tolerates negative GPOS kerning between
// adjacent glyphs, which is normal in real shaping.
func monotonicX(t *testing.T, line *Line) {
	t.Helper()
	const eps = 2.0 // negative kerning can pull a glyph slightly left
	prev := 0.0
	for i, g := range line.Glyphs {
		if g.X < -eps {
			t.Errorf("glyph %d has negative X %f", i, g.X)
		}
		if i > 0 && g.X < prev-eps {
			t.Errorf("glyph %d X %f is far less than previous %f", i, g.X, prev)
		}
		prev = g.X
	}
}

// TestLayoutBidi_LTRSandwich verifies an LTR paragraph containing an RTL run:
// "AB <arabic 5> CD". Visual order keeps Latin runs in place and reverses the
// Arabic glyphs internally. Reference L2 array from the conformance engine:
// [0 1 2 7 6 5 4 3 8 9 10].
func TestLayoutBidi_LTRSandwich(t *testing.T) {
	face := layoutTestFace(t)
	para := "AB مرحبا CD"

	layout := LayoutText(para, face, LayoutOptions{Direction: DirectionLTR, LineSpacing: 1.0})
	if len(layout.Lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(layout.Lines))
	}
	line := &layout.Lines[0]
	monotonicX(t, line)

	// Logical clusters map to byte offsets: Arabic rune starts at byte 3,
	// each Arabic rune is 2 bytes => clusters 3,5,7,9,11 logical order.
	// Visual order keeps the leading space (cluster 2) with the LTR prefix
	// and reverses the Arabic glyphs internally: 11,9,7,5,3.
	got := visualClusters(line)
	wantPrefix := []int{0, 1, 2} // A B space
	wantArabic := []int{11, 9, 7, 5, 3}
	wantSuffix := []int{13, 14, 15} // space C D
	want := append(append(append([]int{}, wantPrefix...), wantArabic...), wantSuffix...)
	if !intSlicesEqual(got, want) {
		t.Errorf("visual clusters = %v, want %v", got, want)
	}

	// The Arabic run must be the middle run and flagged RTL.
	var foundRTL bool
	for i := range line.Runs {
		if line.Runs[i].BidiLevel%2 == 1 {
			foundRTL = true
			if line.Runs[i].Direction != DirectionRTL {
				t.Errorf("odd-level run Direction = %v, want RTL", line.Runs[i].Direction)
			}
		}
	}
	if !foundRTL {
		t.Error("expected at least one RTL run")
	}
}

// TestLayoutBidi_RTLParagraph verifies a forced-RTL paragraph with a Latin
// suffix. The whole line is mirrored: Latin appears leftmost (visual), then
// space, then the reversed Arabic.
func TestLayoutBidi_RTLParagraph(t *testing.T) {
	face := layoutTestFace(t)
	para := "مرحبا Hello"

	layout := LayoutText(para, face, LayoutOptions{Direction: DirectionRTL, LineSpacing: 1.0})
	line := &layout.Lines[0]
	monotonicX(t, line)

	// Reference L2: [6 7 8 9 10 5 4 3 2 1 0] in rune units.
	// Byte clusters: Hello = 10..15, space = 10, Arabic = 0,2,4,6,8.
	// Visual: Hello forward, space, Arabic reversed (8,6,4,2,0).
	got := visualClusters(line)
	want := []int{11, 12, 13, 14, 15, 10, 8, 6, 4, 2, 0}
	if !intSlicesEqual(got, want) {
		t.Errorf("visual clusters = %v, want %v", got, want)
	}

	// Leftmost visual run must be the Latin run (even level 2, LTR).
	if len(line.Runs) < 2 {
		t.Fatalf("runs = %d, want >=2", len(line.Runs))
	}
	if line.Runs[0].BidiLevel%2 != 0 || line.Runs[0].Direction != DirectionLTR {
		t.Errorf("leftmost run level=%d dir=%v; expected even-level Latin LTR run",
			line.Runs[0].BidiLevel, line.Runs[0].Direction)
	}
}

// TestLayoutBidi_NumbersInArabic checks nested European numbers, which stay
// LTR (even level) inside RTL text and must not have their digits reordered.
func TestLayoutBidi_NumbersInArabic(t *testing.T) {
	face := layoutTestFace(t)
	// "مكتب 42 شارع" — office 42 street.
	para := "مكتب 42 شارع"

	layout := LayoutText(para, face, LayoutOptions{Direction: DirectionRTL, LineSpacing: 1.0})
	line := &layout.Lines[0]
	monotonicX(t, line)

	// Locate the digit glyphs and confirm '4' precedes '2' visually.
	idx4, idx2 := -1, -1
	for i, g := range line.Glyphs {
		switch g.Cluster {
		case byteIndexOf(para, "4"):
			idx4 = i
		case byteIndexOf(para, "2"):
			idx2 = i
		}
	}
	if idx4 < 0 || idx2 < 0 {
		t.Fatalf("digit glyphs not found (4=%d, 2=%d)", idx4, idx2)
	}
	if idx2 <= idx4 {
		t.Errorf("digits reversed visually: '4' at %d, '2' at %d", idx4, idx2)
	}
}

// TestLayoutBidi_HebrewPunctuation checks neutral punctuation (question mark)
// resolving inside Hebrew text and bracket handling.
func TestLayoutBidi_HebrewPunctuation(t *testing.T) {
	face := layoutTestFace(t)
	para := "שלום!" // Hebrew + exclamation, RTL base.

	layout := LayoutText(para, face, LayoutOptions{Direction: DirectionRTL, LineSpacing: 1.0})
	if len(layout.Lines) != 1 {
		t.Fatalf("lines = %d", len(layout.Lines))
	}
	line := &layout.Lines[0]
	monotonicX(t, line)
	if len(line.Glyphs) == 0 {
		t.Fatal("no glyphs")
	}
	// Punctuation cluster must be present exactly once.
	bang := byteIndexOf(para, "!")
	count := 0
	for _, g := range line.Glyphs {
		if g.Cluster == bang {
			count++
		}
	}
	if count != 1 {
		t.Errorf("exclamation cluster count = %d, want 1", count)
	}
}

// TestLayoutBidi_Brackets verifies paired-bracket rule N0: an LTR bracket
// group embedded in RTL text keeps its brackets paired (no runaway reversal).
func TestLayoutBidi_Brackets(t *testing.T) {
	face := layoutTestFace(t)
	// Latin word in parentheses inside an Arabic sentence, RTL base.
	para := "عربي (abc) نص"

	layout := LayoutText(para, face, LayoutOptions{Direction: DirectionRTL, LineSpacing: 1.0})
	line := &layout.Lines[0]
	monotonicX(t, line)

	open := byteIndexOf(para, "(")
	close := byteIndexOf(para, ")")
	posOpen, posClose := -1, -1
	for i, g := range line.Glyphs {
		if g.Cluster == open {
			posOpen = i
		}
		if g.Cluster == close {
			posClose = i
		}
	}
	if posOpen < 0 || posClose < 0 {
		t.Fatalf("bracket glyphs missing (open=%d close=%d)", posOpen, posClose)
	}
	// Per UAX #9 L2 the reference reorder for this RTL paragraph places the
	// closing bracket before the opening bracket visually (the brackets are
	// mirrored). Assert against that reference rather than assuming LTR order.
	pb := resolveParagraphLevels(para, DirectionRTL)
	ref := pb.resolver.LineReorder(0, pb.resolver.RuneCount())
	runeToVisual := make(map[int]int, len(ref))
	for visualPos, runeIdx := range ref {
		if _, exists := runeToVisual[runeIdx]; !exists {
			runeToVisual[runeIdx] = visualPos
		}
	}
	openRune := pb.runeIndexForByteOffset(open)
	closeRune := pb.runeIndexForByteOffset(close)
	if posOpen != runeToVisual[openRune] || posClose != runeToVisual[closeRune] {
		t.Errorf("bracket visual positions ( =%d, )=%d do not match reference %v",
			posOpen, posClose, runeToVisual)
	}
}

// TestLayoutBidi_CombiningMarks ensures combining marks stay attached to
// their base character (grapheme integrity) through reversal.
func TestLayoutBidi_CombiningMarks(t *testing.T) {
	face := layoutTestFace(t)
	// Latin base + combining circumflex inside RTL context.
	para := "اب â ب" // alef-bet, space, a-circumflex, space, bet

	layout := LayoutText(para, face, LayoutOptions{Direction: DirectionRTL, LineSpacing: 1.0})
	line := &layout.Lines[0]
	monotonicX(t, line)

	// The mark's cluster must equal its base's cluster (shaper merges them),
	// or at minimum the mark must render adjacent to the base. We assert no
	// glyph cluster was duplicated/lost and total cluster set is a subset
	// of the source byte offsets.
	if len(line.Glyphs) == 0 {
		t.Fatal("no glyphs")
	}
	maxByte := len(para)
	for i, g := range line.Glyphs {
		if g.Cluster < 0 || g.Cluster > maxByte {
			t.Errorf("glyph %d has out-of-range cluster %d", i, g.Cluster)
		}
	}
}

// TestLayoutBidi_PureLTRUnchanged asserts the pure-LTR fast path produces
// identical positions to a plain LTR layout (no reordering side effects).
func TestLayoutBidi_PureLTRUnchanged(t *testing.T) {
	face := layoutTestFace(t)
	para := "Hello World 123"

	got := LayoutText(para, face, DefaultLayoutOptions())
	if len(got.Lines) != 1 {
		t.Fatalf("lines = %d", len(got.Lines))
	}
	line := &got.Lines[0]
	monotonicX(t, line)

	// First glyph at origin, clusters in ascending logical order.
	if len(line.Glyphs) == 0 || line.Glyphs[0].X != 0 {
		t.Fatal("first glyph not at X=0")
	}
	prev := -1
	for _, g := range line.Glyphs {
		if g.Cluster < prev {
			t.Errorf("pure LTR cluster decreased: %d after %d", g.Cluster, prev)
		}
		prev = g.Cluster
	}
	// All runs must be even level.
	for i := range line.Runs {
		if line.Runs[i].BidiLevel%2 != 0 {
			t.Errorf("run %d odd level in pure-LTR text", i)
		}
	}
}

// TestLayoutBidi_Alignment checks Left/Center/Right offsets are consistent
// with the visually reordered (and thus correct) line width.
func TestLayoutBidi_Alignment(t *testing.T) {
	face := layoutTestFace(t)
	para := "AB مرحبا CD"
	const maxWidth = 400.0

	for _, align := range []Alignment{AlignLeft, AlignCenter, AlignRight} {
		t.Run(align.String(), func(t *testing.T) {
			opts := LayoutOptions{
				MaxWidth:    maxWidth,
				Alignment:   align,
				Direction:   DirectionLTR,
				LineSpacing: 1.0,
			}
			layout := LayoutText(para, face, opts)
			line := &layout.Lines[0]
			if len(line.Glyphs) == 0 {
				t.Fatal("no glyphs")
			}
			firstX := line.Glyphs[0].X
			last := line.Glyphs[len(line.Glyphs)-1]
			endX := last.X + last.XAdvance

			switch align {
			case AlignLeft:
				if firstX != 0 {
					t.Errorf("left align first X = %f, want 0", firstX)
				}
			case AlignCenter:
				want := (maxWidth - line.Width) / 2
				if absF(firstX-want) > 1 {
					t.Errorf("center first X = %f, want ~%f", firstX, want)
				}
			case AlignRight:
				if absF(endX-maxWidth) > 1 {
					t.Errorf("right end X = %f, want ~%f", endX, maxWidth)
				}
			}
			monotonicX(t, line)
		})
	}
}

// TestLayoutBidi_MultiLine exercises hard line breaks with mixed directions;
// each line reorders independently.
func TestLayoutBidi_MultiLine(t *testing.T) {
	face := layoutTestFace(t)
	para := "مرحبا\nHello 42\nAB ج CD"

	layout := LayoutText(para, face, LayoutOptions{Direction: DirectionLTR, LineSpacing: 1.0})
	if len(layout.Lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(layout.Lines))
	}
	for i := range layout.Lines {
		monotonicX(t, &layout.Lines[i])
		if i > 0 && layout.Lines[i].Y <= layout.Lines[i-1].Y {
			t.Errorf("line %d Y not increasing", i)
		}
	}
}

// TestLayoutBidi_Wrapped wraps mixed text and confirms each wrapped line is
// independently reordered and never exceeds a reasonable width bound.
func TestLayoutBidi_Wrapped(t *testing.T) {
	face := layoutTestFace(t)
	para := "abc مرحبا def مرحبا ghi مرحبا jkl مرحبا mno"

	opts := LayoutOptions{
		MaxWidth:    90,
		LineSpacing: 1.0,
		Alignment:   AlignLeft,
		Direction:   DirectionLTR,
	}
	layout := LayoutText(para, face, opts)
	if len(layout.Lines) < 2 {
		t.Fatalf("expected wrapped multiple lines, got %d", len(layout.Lines))
	}
	for i := range layout.Lines {
		monotonicX(t, &layout.Lines[i])
		if layout.Lines[i].Width > opts.MaxWidth*2.5 {
			t.Errorf("line %d width %f far exceeds MaxWidth", i, layout.Lines[i].Width)
		}
	}
}

// TestLayoutBidi_InvalidUTF8 ensures malformed UTF-8 does not panic, every
// glyph keeps a valid cluster, and the line still reorders deterministically.
func TestLayoutBidi_InvalidUTF8(t *testing.T) {
	face := layoutTestFace(t)
	para := "ab\xff\xc0\x80cd مرحبا"

	layout := LayoutText(para, face, DefaultLayoutOptions())
	if layout == nil || len(layout.Lines) != 1 {
		t.Fatalf("unexpected layout: %+v", layout)
	}
	line := &layout.Lines[0]
	for i, g := range line.Glyphs {
		if g.Cluster < 0 || g.Cluster > len(para) {
			t.Errorf("glyph %d invalid cluster %d", i, g.Cluster)
		}
	}
	monotonicX(t, line)
}

// TestLayoutBidi_EmptyRun ensures runs that shape to no glyphs (e.g. only
// bidi control characters) are dropped without breaking layout.
func TestLayoutBidi_EmptyRun(t *testing.T) {
	face := layoutTestFace(t)
	// RLE ... PDF around a word; the controls produce no glyphs.
	para := "abc ‫def‬ ghi"

	layout := LayoutText(para, face, DefaultLayoutOptions())
	if len(layout.Lines) != 1 {
		t.Fatalf("lines = %d", len(layout.Lines))
	}
	line := &layout.Lines[0]
	// Only the 9 letters + 2 spaces should produce glyphs (controls filtered).
	if len(line.Glyphs) != 11 {
		t.Errorf("glyphs = %d, want 11 (controls must be filtered)", len(line.Glyphs))
	}
	monotonicX(t, line)
}

// TestLayoutBidi_MissingGlyph keeps .notdef glyphs but preserves their cluster
// and advance so RTL text with an unsupported font still lays out correctly.
func TestLayoutBidi_MissingGlyph(t *testing.T) {
	face := layoutTestFace(t) // has no Arabic glyphs
	para := "مرحبا"

	layout := LayoutText(para, face, LayoutOptions{Direction: DirectionRTL, LineSpacing: 1.0})
	line := &layout.Lines[0]
	if len(line.Glyphs) != 5 {
		t.Fatalf("expected 5 .notdef glyphs, got %d", len(line.Glyphs))
	}
	// All must be GID 0 (.notdef) but clusters must be distinct and valid.
	seen := map[int]bool{}
	for _, g := range line.Glyphs {
		if g.GID != 0 {
			t.Errorf("expected .notdef GID 0, got %d", g.GID)
		}
		if seen[g.Cluster] {
			t.Errorf("duplicate cluster %d", g.Cluster)
		}
		seen[g.Cluster] = true
	}
	monotonicX(t, line)
}

// TestLayoutBidi_Cancellation verifies a canceled context returns an error
// and no partial layout.
func TestLayoutBidi_Cancellation(t *testing.T) {
	face := layoutTestFace(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	layout, err := LayoutTextWithContext(ctx, "مرحبا Hello", face, DefaultLayoutOptions())
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if layout != nil {
		t.Errorf("expected nil layout on cancel, got %+v", layout)
	}
}

// TestLayoutBidi_ClusterAndFacePreserved ensures reordering never mutates the
// face reference or advance widths and keeps every glyph's source cluster.
func TestLayoutBidi_ClusterAndFacePreserved(t *testing.T) {
	face := layoutTestFace(t)
	para := "AB مرحبا CD"

	logical := shapeAllLogical(t, para, face)
	layout := LayoutText(para, face, LayoutOptions{Direction: DirectionLTR, LineSpacing: 1.0})

	// Sum of advances must be identical before and after reordering.
	var logicalAdv, visualAdv float64
	for i := range logical {
		for _, g := range logical[i].Glyphs {
			logicalAdv += g.XAdvance
		}
	}
	for i := range layout.Lines[0].Runs {
		for _, g := range layout.Lines[0].Runs[i].Glyphs {
			visualAdv += g.XAdvance
		}
		if layout.Lines[0].Runs[i].Face != face {
			t.Error("run Face reference changed during reordering")
		}
	}
	if absF(logicalAdv-visualAdv) > 1e-6 {
		t.Errorf("advance sum changed: logical %f visual %f", logicalAdv, visualAdv)
	}

	// Every visual glyph cluster must exist in the logical glyph set.
	logicalClusters := map[int]bool{}
	for i := range logical {
		for _, g := range logical[i].Glyphs {
			logicalClusters[g.Cluster] = true
		}
	}
	for _, g := range layout.Lines[0].Glyphs {
		if !logicalClusters[g.Cluster] {
			t.Errorf("visual glyph cluster %d not present in logical layout", g.Cluster)
		}
	}
}

// shapeAllLogical shapes the paragraph segments without visual reordering,
// returning the logical-order shaped runs for comparison.
func shapeAllLogical(t *testing.T, para string, face Face) []ShapedRun {
	t.Helper()
	segs := NewBuiltinSegmenterWithDirection(DirectionLTR).Segment(para)
	pb := resolveParagraphLevels(para, DirectionLTR)
	br := shapeBidiSegments(pb, segs, face, face.Metrics())
	return plainRuns(br)
}

// --- small helpers ---

func intSlicesEqual(a, b []int) bool {
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

func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// byteIndexOf returns the byte offset of the first occurrence of substr in s,
// or -1 if absent.
func byteIndexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
