package text

import (
	"math"
	"os"
	"testing"
)

// justifyTestFace loads a test font from testdata at the given size.
func justifyTestFace(t *testing.T, fontFile string, size float64) Face {
	t.Helper()

	data, err := os.ReadFile("testdata/" + fontFile)
	if err != nil {
		t.Skipf("test font %s not available: %v", fontFile, err)
	}
	source, err := NewFontSource(data)
	if err != nil {
		t.Fatalf("failed to create font source from %s: %v", fontFile, err)
	}
	t.Cleanup(func() {
		_ = source.Close()
	})

	return source.Face(size)
}

// justifyReference returns per-line natural widths and cluster sequences
// from an AlignLeft layout, used as an independent reference: justification
// must not change wrapping, clusters, or glyph order — only X positions.
func justifyReference(t *testing.T, text string, face Face, opts LayoutOptions) (widths []float64, clusters [][]int, xs [][]float64) {
	t.Helper()

	ref := opts
	ref.Alignment = AlignLeft
	ref.Justify = JustifyOptions{}

	layout := LayoutText(text, face, ref)
	for i := range layout.Lines {
		line := &layout.Lines[i]
		widths = append(widths, line.Width)
		var cl []int
		var x []float64
		for _, g := range line.Glyphs {
			cl = append(cl, g.Cluster)
			x = append(x, g.X)
		}
		clusters = append(clusters, cl)
		xs = append(xs, x)
	}
	return widths, clusters, xs
}

const justifyEps = 1e-6

// checkJustifiedLine verifies the core invariants of a justified line
// against its left-aligned reference: exact target width, zero initial
// shift, monotonically non-decreasing shifts, and identical clusters.
func checkJustifiedLine(t *testing.T, line *Line, refWidth float64, refClusters []int, refX []float64, target float64) {
	t.Helper()

	if math.Abs(line.Width-target) > justifyEps {
		t.Errorf("justified line width = %f, want %f", line.Width, target)
	}
	if len(line.Glyphs) != len(refClusters) {
		t.Fatalf("glyph count %d != reference %d", len(line.Glyphs), len(refClusters))
	}
	if len(line.Glyphs) == 0 {
		return
	}
	if line.Glyphs[0].X != 0 {
		t.Errorf("first glyph X = %f, want 0 (no shift carried into line)", line.Glyphs[0].X)
	}

	prevShift := 0.0
	for i, g := range line.Glyphs {
		if g.Cluster != refClusters[i] {
			t.Errorf("glyph %d cluster = %d, reference %d", i, g.Cluster, refClusters[i])
		}
		shift := g.X - refX[i]
		if shift < prevShift-1e-9 {
			t.Errorf("glyph %d shift %f < previous shift %f (shifts must be non-decreasing)",
				i, shift, prevShift)
		}
		prevShift = shift
	}

	// Total shift of the visual end must equal the distributed extra.
	last := line.Glyphs[len(line.Glyphs)-1]
	endShift := last.X - refX[len(refX)-1]
	extra := target - refWidth
	if math.Abs(endShift-extra) > justifyEps {
		t.Errorf("total shift %f, want distributed extra %f", endShift, extra)
	}
}

// checkNonFinalLine verifies a non-final line against its left-aligned
// reference. Lines whose natural width already exceeds the target (the
// greedy wrapper can overshoot by one break unit) must stay byte-for-byte
// unchanged — justification never compresses. Lines at or under the
// target must be stretched to exactly the target width.
func checkNonFinalLine(t *testing.T, line *Line, refWidth float64, refClusters []int, refX []float64, target float64) {
	t.Helper()

	if refWidth > target {
		// Over-wide line: deterministic no-op.
		if line.Width != refWidth {
			t.Errorf("over-wide line width = %f, want unchanged %f", line.Width, refWidth)
		}
		for i, g := range line.Glyphs {
			if g.X != refX[i] {
				t.Errorf("over-wide line glyph %d X = %f, want unchanged %f", i, g.X, refX[i])
			}
		}
		return
	}
	checkJustifiedLine(t, line, refWidth, refClusters, refX, target)
}

// TestLayoutText_JustifyLatin tests justified alignment of a wrapped
// Latin paragraph against an independent left-aligned reference.
func TestLayoutText_JustifyLatin(t *testing.T) {
	face := layoutTestFace(t)
	text := "The quick brown fox jumps over the lazy dog while " +
		"a second fox watches quietly from the tall grass nearby"

	opts := LayoutOptions{
		MaxWidth:    200,
		LineSpacing: 1.0,
		Alignment:   AlignJustify,
		Direction:   DirectionLTR,
	}

	refWidths, refClusters, refX := justifyReference(t, text, face, opts)
	layout := LayoutText(text, face, opts)

	if len(layout.Lines) < 3 {
		t.Fatalf("expected at least 3 wrapped lines, got %d", len(layout.Lines))
	}

	last := len(layout.Lines) - 1
	for i := range layout.Lines {
		line := &layout.Lines[i]
		if i == last {
			// Final line of the paragraph: last-line policy (default left).
			if line.Width != refWidths[i] {
				t.Errorf("last line width = %f, want natural %f", line.Width, refWidths[i])
			}
			if len(line.Glyphs) > 0 && line.Glyphs[0].X != 0 {
				t.Errorf("last line first glyph X = %f, want 0", line.Glyphs[0].X)
			}
			continue
		}
		checkNonFinalLine(t, line, refWidths[i], refClusters[i], refX[i], opts.MaxWidth)
	}
}

// TestLayoutText_JustifyCJK tests CJK justification. Pure CJK lines wrap
// one glyph past MaxWidth (the greedy wrapper breaks after the overflow
// glyph), so non-final lines exercise the deterministic over-wide no-op;
// CJK phrases separated by spaces wrap under MaxWidth and their inter-CJK
// gaps must expand to fill the line.
func TestLayoutText_JustifyCJK(t *testing.T) {
	face := justifyTestFace(t, "notoseriftc_autohint_metrics.ttf", 16)

	t.Run("PureCJKOverWideLinesUnchanged", func(t *testing.T) {
		// Only code points covered by the trimmed test font.
		text := "你我都說是時期新事情人們想過你我都說是時期新事情人們想過你我都說是時期"
		opts := LayoutOptions{
			MaxWidth:    100,
			LineSpacing: 1.0,
			Alignment:   AlignJustify,
			Direction:   DirectionLTR,
		}

		refWidths, refClusters, refX := justifyReference(t, text, face, opts)
		layout := LayoutText(text, face, opts)

		if len(layout.Lines) < 3 {
			t.Fatalf("expected at least 3 wrapped lines, got %d", len(layout.Lines))
		}
		for i := 0; i < len(layout.Lines)-1; i++ {
			checkNonFinalLine(t, &layout.Lines[i], refWidths[i], refClusters[i], refX[i], opts.MaxWidth)
		}
	})

	t.Run("MixedCJKLatinGapsExpand", func(t *testing.T) {
		// Latin words create break opportunities before the overflow
		// glyph, so wrapped lines end under MaxWidth and contain adjacent
		// CJK glyphs whose gaps must absorb extra width.
		text := "你我都abc 說是時def 期新事ghi 情人們jkl 想過你mno 我都說pqr 是時期stu 新事情vwx 人們想yza 過你我bcd"
		opts := LayoutOptions{
			MaxWidth:    120,
			LineSpacing: 1.0,
			Alignment:   AlignJustify,
			Direction:   DirectionLTR,
		}

		refWidths, refClusters, refX := justifyReference(t, text, face, opts)
		layout := LayoutText(text, face, opts)

		if len(layout.Lines) < 3 {
			t.Fatalf("expected at least 3 wrapped lines, got %d", len(layout.Lines))
		}

		justifiedLines := 0
		expanded := 0
		for i := 0; i < len(layout.Lines)-1; i++ {
			line := &layout.Lines[i]
			checkNonFinalLine(t, line, refWidths[i], refClusters[i], refX[i], opts.MaxWidth)
			if refWidths[i] > opts.MaxWidth {
				continue
			}
			justifiedLines++
			// Find an adjacent CJK-CJK pair and verify its gap grew.
			for j := 0; j+1 < len(line.Glyphs); j++ {
				if line.Glyphs[j].IsCJK && line.Glyphs[j+1].IsCJK {
					gotGap := line.Glyphs[j+1].X - line.Glyphs[j].X
					refGap := refX[i][j+1] - refX[i][j]
					if gotGap > refGap+justifyEps {
						expanded++
					}
					break
				}
			}
		}
		if justifiedLines == 0 {
			t.Error("no under-width non-final lines to justify")
		}
		if expanded == 0 {
			t.Error("no inter-CJK gap expanded in any justified line")
		}
	})
}

// TestLayoutText_JustifyCJKGapWeightDisabled tests that a negative
// CJKGapWeight disables CJK gap expansion: with no word spaces there are
// no expansion points, so lines keep their natural width.
func TestLayoutText_JustifyCJKGapWeightDisabled(t *testing.T) {
	face := justifyTestFace(t, "notoseriftc_autohint_metrics.ttf", 16)
	text := "你我都說是時期新事情人們想過你我都說是時期新事情人們想過"

	opts := LayoutOptions{
		MaxWidth:    100,
		LineSpacing: 1.0,
		Alignment:   AlignJustify,
		Direction:   DirectionLTR,
		Justify:     JustifyOptions{CJKGapWeight: -1},
	}

	refWidths, _, refX := justifyReference(t, text, face, opts)
	layout := LayoutText(text, face, opts)

	for i := range layout.Lines {
		line := &layout.Lines[i]
		if line.Width != refWidths[i] {
			t.Errorf("line %d width = %f, want unchanged natural width %f",
				i, line.Width, refWidths[i])
		}
		for j, g := range line.Glyphs {
			if g.X != refX[i][j] {
				t.Errorf("line %d glyph %d X = %f, want unchanged %f",
					i, j, g.X, refX[i][j])
			}
		}
	}
}

// TestLayoutText_JustifyArabicRTL tests justification of Arabic text with
// RTL paragraph direction, including cluster preservation and visual order.
func TestLayoutText_JustifyArabicRTL(t *testing.T) {
	face := layoutTestFace(t)
	text := "مرحبا بالعالم هذا نص عربي بسيط للاختبار مع كلمات كثيرة " +
		"حتى يلتف السطر على أكثر من سطر واحد"

	opts := LayoutOptions{
		MaxWidth:    250,
		LineSpacing: 1.0,
		Alignment:   AlignJustify,
		Direction:   DirectionRTL,
	}

	refWidths, refClusters, refX := justifyReference(t, text, face, opts)
	layout := LayoutText(text, face, opts)

	if len(layout.Lines) < 2 {
		t.Fatalf("expected at least 2 wrapped lines, got %d", len(layout.Lines))
	}

	for i := 0; i < len(layout.Lines)-1; i++ {
		line := &layout.Lines[i]
		checkNonFinalLine(t, line, refWidths[i], refClusters[i], refX[i], opts.MaxWidth)

		// Visual order: glyph origins must be non-decreasing along the line.
		for j := 1; j < len(line.Glyphs); j++ {
			if line.Glyphs[j].X < line.Glyphs[j-1].X {
				t.Errorf("line %d glyph %d X=%f goes backwards (prev %f)",
					i, j, line.Glyphs[j].X, line.Glyphs[j-1].X)
			}
		}
	}
}

// TestLayoutText_JustifyMixedScript tests a bidi paragraph mixing Latin and
// Arabic runs: justification must stretch gaps across run boundaries.
func TestLayoutText_JustifyMixedScript(t *testing.T) {
	face := layoutTestFace(t)
	text := "Hello مرحبا World هذا نص mixed مع كلمات عربية and Latin words " +
		"together in one paragraph that wraps around"

	opts := LayoutOptions{
		MaxWidth:    220,
		LineSpacing: 1.0,
		Alignment:   AlignJustify,
		Direction:   DirectionLTR,
	}

	refWidths, refClusters, refX := justifyReference(t, text, face, opts)
	layout := LayoutText(text, face, opts)

	if len(layout.Lines) < 2 {
		t.Fatalf("expected at least 2 wrapped lines, got %d", len(layout.Lines))
	}

	for i := 0; i < len(layout.Lines)-1; i++ {
		checkNonFinalLine(t, &layout.Lines[i], refWidths[i], refClusters[i], refX[i], opts.MaxWidth)
	}
}

// TestLayoutText_JustifyLastLinePolicy tests configurable last-line behavior.
func TestLayoutText_JustifyLastLinePolicy(t *testing.T) {
	face := layoutTestFace(t)
	text := "The quick brown fox jumps over the lazy dog while " +
		"a second fox watches quietly from the tall grass nearby"

	base := LayoutOptions{
		MaxWidth:    200,
		LineSpacing: 1.0,
		Alignment:   AlignJustify,
		Direction:   DirectionLTR,
	}

	t.Run("AlignRight", func(t *testing.T) {
		opts := base
		opts.Justify.LastLineAlignment = AlignRight
		layout := LayoutText(text, face, opts)
		last := &layout.Lines[len(layout.Lines)-1]
		if len(last.Glyphs) == 0 {
			t.Fatal("last line has no glyphs")
		}
		lastG := last.Glyphs[len(last.Glyphs)-1]
		if end := lastG.X + lastG.XAdvance; math.Abs(end-opts.MaxWidth) > justifyEps {
			t.Errorf("last line end = %f, want %f (AlignRight policy)", end, opts.MaxWidth)
		}
	})

	t.Run("AlignCenter", func(t *testing.T) {
		opts := base
		opts.Justify.LastLineAlignment = AlignCenter
		layout := LayoutText(text, face, opts)
		last := &layout.Lines[len(layout.Lines)-1]
		if len(last.Glyphs) == 0 {
			t.Fatal("last line has no glyphs")
		}
		wantX := (opts.MaxWidth - last.Width) / 2
		if math.Abs(last.Glyphs[0].X-wantX) > justifyEps {
			t.Errorf("last line first X = %f, want %f (AlignCenter policy)",
				last.Glyphs[0].X, wantX)
		}
	})

	t.Run("JustifyTreatedAsLeft", func(t *testing.T) {
		opts := base
		opts.Justify.LastLineAlignment = AlignJustify
		refWidths, _, refX := justifyReference(t, text, face, opts)
		layout := LayoutText(text, face, opts)
		last := len(layout.Lines) - 1
		line := &layout.Lines[last]
		if line.Width != refWidths[last] {
			t.Errorf("last line width = %f, want natural %f (justify policy falls back to left)",
				line.Width, refWidths[last])
		}
		for j, g := range line.Glyphs {
			if g.X != refX[last][j] {
				t.Errorf("last line glyph %d X = %f, want %f", j, g.X, refX[last][j])
			}
		}
	})
}

// TestLayoutText_JustifyHardBreaks tests that lines ended by hard breaks
// are treated as paragraph-final (not stretched), while wrapped lines
// before them are justified.
func TestLayoutText_JustifyHardBreaks(t *testing.T) {
	face := layoutTestFace(t)
	text := "The quick brown fox jumps over the lazy dog while a second fox watches\n" +
		"Short line\n" +
		"The lazy dog sleeps under the old oak tree while the wind blows softly"

	opts := LayoutOptions{
		MaxWidth:    200,
		LineSpacing: 1.0,
		Alignment:   AlignJustify,
		Direction:   DirectionLTR,
	}

	refWidths, refClusters, refX := justifyReference(t, text, face, opts)
	layout := LayoutText(text, face, opts)

	// Paragraph 1 wraps to >=2 lines, paragraph 2 is a single short line,
	// paragraph 3 wraps to >=2 lines.
	if len(layout.Lines) < 5 {
		t.Fatalf("expected at least 5 lines, got %d", len(layout.Lines))
	}

	// Count lines per paragraph to locate paragraph-final lines.
	paraLines := []int{}
	for _, para := range splitParagraphs(text) {
		metrics := face.Metrics()
		paraLines = append(paraLines, len(layoutParagraph(para, face, opts, metrics)))
	}

	idx := 0
	for p, n := range paraLines {
		for j := 0; j < n; j++ {
			line := &layout.Lines[idx]
			if j == n-1 {
				// Paragraph-final line: natural width (hard break policy).
				if line.Width != refWidths[idx] {
					t.Errorf("paragraph %d final line width = %f, want natural %f",
						p, line.Width, refWidths[idx])
				}
			} else {
				checkNonFinalLine(t, line, refWidths[idx], refClusters[idx], refX[idx], opts.MaxWidth)
			}
			idx++
		}
	}
}

// TestLayoutText_JustifyMaxWordSpacing tests the configurable word-spacing
// cap: expanded word spaces never exceed MaxWordSpacing times their
// original advance.
func TestLayoutText_JustifyMaxWordSpacing(t *testing.T) {
	face := layoutTestFace(t)
	text := "The quick brown fox jumps over the lazy dog while " +
		"a second fox watches quietly from the tall grass nearby"

	const capFactor = 1.5
	opts := LayoutOptions{
		MaxWidth:    300,
		LineSpacing: 1.0,
		Alignment:   AlignJustify,
		Direction:   DirectionLTR,
		Justify:     JustifyOptions{MaxWordSpacing: capFactor},
	}

	_, _, refX := justifyReference(t, text, face, opts)
	refWidths, _, _ := justifyReference(t, text, face, opts)
	layout := LayoutText(text, face, opts)

	for i := 0; i < len(layout.Lines)-1; i++ {
		line := &layout.Lines[i]
		for j, g := range line.Glyphs {
			if !g.IsSpace || j+1 >= len(line.Glyphs) {
				continue
			}
			// Expanded space width = distance to the next glyph origin.
			expanded := line.Glyphs[j+1].X - g.X
			natural := refX[i][j+1] - refX[i][j]
			if expanded > capFactor*natural+1e-6 {
				t.Errorf("line %d space %d expanded to %f, cap is %f (%.1fx natural %f)",
					i, j, expanded, capFactor*natural, capFactor, natural)
			}
		}
		// Justified (stretched) lines must never exceed the target.
		// Lines the wrapper already pushed past MaxWidth stay unchanged.
		if refWidths[i] <= opts.MaxWidth && line.Width > opts.MaxWidth+justifyEps {
			t.Errorf("line %d width %f exceeds MaxWidth %f", i, line.Width, opts.MaxWidth)
		}
	}
}

// TestLayoutText_JustifyNoCarryOver tests that extra spacing never leaks
// into the next line: every line starts at X=0 with shifts computed from
// its own width only.
func TestLayoutText_JustifyNoCarryOver(t *testing.T) {
	face := layoutTestFace(t)
	text := "The quick brown fox jumps\n\nover the lazy dog and runs and runs and runs"

	opts := LayoutOptions{
		MaxWidth:    150,
		LineSpacing: 1.0,
		Alignment:   AlignJustify,
		Direction:   DirectionLTR,
	}

	layout := LayoutText(text, face, opts)

	// The empty middle paragraph must survive justification untouched.
	emptyLines := 0
	for i := range layout.Lines {
		if len(layout.Lines[i].Glyphs) == 0 {
			emptyLines++
			if layout.Lines[i].Width != 0 {
				t.Errorf("empty line %d has width %f, want 0", i, layout.Lines[i].Width)
			}
		}
	}
	if emptyLines != 1 {
		t.Errorf("expected exactly 1 empty line, got %d", emptyLines)
	}

	for i, line := range layout.Lines {
		if len(line.Glyphs) == 0 {
			continue
		}
		if line.Glyphs[0].X != 0 {
			t.Errorf("line %d first glyph X = %f, want 0 (no carry-over)", i, line.Glyphs[0].X)
		}
		if line.Width > opts.MaxWidth+justifyEps {
			// Only wrapper-overshoot lines may exceed MaxWidth; justify
			// must not have stretched them further. Verify against the
			// independent left-aligned reference.
			ref := LayoutText(text, face, LayoutOptions{
				MaxWidth: opts.MaxWidth, LineSpacing: 1.0, Alignment: AlignLeft,
			})
			if line.Width != ref.Lines[i].Width {
				t.Errorf("line %d width %f exceeds MaxWidth and differs from reference %f",
					i, line.Width, ref.Lines[i].Width)
			}
		}
	}
}

// TestLayoutText_JustifyDegenerateWidths tests zero and non-finite MaxWidth.
func TestLayoutText_JustifyDegenerateWidths(t *testing.T) {
	face := layoutTestFace(t)
	text := "The quick brown fox jumps over the lazy dog"

	for _, mw := range []float64{0, -100, math.NaN(), math.Inf(1), math.Inf(-1)} {
		opts := LayoutOptions{
			MaxWidth:    mw,
			LineSpacing: 1.0,
			Alignment:   AlignJustify,
			Direction:   DirectionLTR,
		}
		layout := LayoutText(text, face, opts) // must not panic
		if layout == nil {
			t.Fatalf("MaxWidth=%f: nil layout", mw)
		}
		for i, line := range layout.Lines {
			for j, g := range line.Glyphs {
				if math.IsNaN(g.X) || math.IsInf(g.X, 0) {
					t.Errorf("MaxWidth=%f line %d glyph %d: non-finite X %f", mw, i, j, g.X)
				}
			}
			if math.IsNaN(line.Width) {
				t.Errorf("MaxWidth=%f line %d: NaN width", mw, i)
			}
		}
	}
}

// TestJustifyLine_EdgeCases unit-tests justifyLine directly with
// hand-built lines for the deterministic edge cases.
func TestJustifyLine_EdgeCases(t *testing.T) {
	mkGlyph := func(cluster int, x, adv float64, space, cjk bool) ShapedGlyph {
		return ShapedGlyph{Cluster: cluster, X: x, XAdvance: adv, IsSpace: space, IsCJK: cjk}
	}

	t.Run("EmptyLine", func(t *testing.T) {
		line := &Line{}
		justifyLine(line, 100, JustifyOptions{})
		if line.Width != 0 || len(line.Glyphs) != 0 {
			t.Errorf("empty line changed: %+v", line)
		}
	})

	t.Run("SingleGlyph", func(t *testing.T) {
		line := &Line{
			Glyphs: []ShapedGlyph{mkGlyph(0, 0, 10, false, false)},
			Width:  10,
		}
		justifyLine(line, 100, JustifyOptions{})
		if line.Width != 10 || line.Glyphs[0].X != 0 {
			t.Errorf("single-glyph line changed: width=%f x=%f", line.Width, line.Glyphs[0].X)
		}
	})

	t.Run("NoExpansionPoints", func(t *testing.T) {
		line := &Line{
			Glyphs: []ShapedGlyph{
				mkGlyph(0, 0, 10, false, false),
				mkGlyph(1, 10, 10, false, false),
			},
			Width: 20,
		}
		justifyLine(line, 100, JustifyOptions{})
		if line.Width != 20 || line.Glyphs[1].X != 10 {
			t.Errorf("line without expansion points changed: width=%f x1=%f",
				line.Width, line.Glyphs[1].X)
		}
	})

	t.Run("DegenerateMaxWidth", func(t *testing.T) {
		for _, mw := range []float64{0, -5, math.NaN(), math.Inf(1), math.Inf(-1)} {
			line := &Line{
				Glyphs: []ShapedGlyph{
					mkGlyph(0, 0, 10, false, false),
					mkGlyph(1, 10, 4, true, false),
					mkGlyph(2, 14, 10, false, false),
				},
				Width: 24,
			}
			justifyLine(line, mw, JustifyOptions{})
			if line.Width != 24 || line.Glyphs[2].X != 14 {
				t.Errorf("maxWidth=%f: line changed: width=%f x2=%f",
					mw, line.Width, line.Glyphs[2].X)
			}
		}
	})

	t.Run("LineWiderThanMaxWidth", func(t *testing.T) {
		line := &Line{
			Glyphs: []ShapedGlyph{
				mkGlyph(0, 0, 10, false, false),
				mkGlyph(1, 10, 4, true, false),
				mkGlyph(2, 14, 10, false, false),
			},
			Width: 24,
		}
		justifyLine(line, 20, JustifyOptions{})
		if line.Width != 24 || line.Glyphs[2].X != 14 {
			t.Errorf("over-wide line compressed: width=%f x2=%f", line.Width, line.Glyphs[2].X)
		}
	})

	t.Run("NegativeAdvanceSpaceWithCap", func(t *testing.T) {
		// A space glyph with negative advance contributes zero cap room;
		// with no other expansion points the line stays unchanged.
		line := &Line{
			Glyphs: []ShapedGlyph{
				mkGlyph(0, 0, 10, false, false),
				mkGlyph(1, 10, -4, true, false),
				mkGlyph(2, 6, 10, false, false),
			},
			Width: 16,
		}
		justifyLine(line, 100, JustifyOptions{MaxWordSpacing: 2})
		if line.Width != 16 || line.Glyphs[2].X != 6 {
			t.Errorf("negative-advance space expanded under cap: width=%f x2=%f",
				line.Width, line.Glyphs[2].X)
		}
	})

	t.Run("ClusterNotSplit", func(t *testing.T) {
		// Glyphs 0 and 1 share a cluster (e.g. a ligature): the gap
		// between them must not expand even though glyph 0 is a space.
		line := &Line{
			Glyphs: []ShapedGlyph{
				mkGlyph(0, 0, 4, true, false),
				mkGlyph(0, 4, 10, false, false),
				mkGlyph(1, 14, 4, true, false),
				mkGlyph(2, 18, 10, false, false),
			},
			Width: 28,
		}
		justifyLine(line, 38, JustifyOptions{})
		if line.Glyphs[1].X != 4 {
			t.Errorf("cluster split: glyph 1 moved to %f", line.Glyphs[1].X)
		}
		if line.Glyphs[3].X != 28 {
			t.Errorf("glyph 3 X = %f, want 28 (full extra at the only legal gap)", line.Glyphs[3].X)
		}
		if line.Width != 38 {
			t.Errorf("width = %f, want 38", line.Width)
		}
	})

	t.Run("WordSpacingCapOverflowToCJK", func(t *testing.T) {
		// Capped word space saturates; the remainder flows to CJK gaps.
		line := &Line{
			Glyphs: []ShapedGlyph{
				mkGlyph(0, 0, 10, false, false),
				mkGlyph(1, 10, 4, true, false), // space, cap 1.5 -> max extra 2
				mkGlyph(2, 14, 16, false, true),
				mkGlyph(3, 30, 16, false, true),
			},
			Width: 46,
		}
		justifyLine(line, 56, JustifyOptions{MaxWordSpacing: 1.5})
		if line.Width != 56 {
			t.Errorf("width = %f, want 56 (cap overflow reaches CJK gap)", line.Width)
		}
		if got := line.Glyphs[2].X - 14; got != 2 {
			t.Errorf("space expansion = %f, want 2 (capped)", got)
		}
		if got := line.Glyphs[3].X - 30; got != 10 {
			t.Errorf("glyph 3 shift = %f, want 10 (2 space + 8 CJK)", got)
		}
	})

	t.Run("WordSpacingCapNoOverflowTarget", func(t *testing.T) {
		// All points capped below the extra: line keeps a smaller final
		// width deterministically instead of exceeding the cap.
		line := &Line{
			Glyphs: []ShapedGlyph{
				mkGlyph(0, 0, 10, false, false),
				mkGlyph(1, 10, 4, true, false), // cap 1.5 -> max extra 2
				mkGlyph(2, 14, 10, false, false),
			},
			Width: 24,
		}
		justifyLine(line, 100, JustifyOptions{MaxWordSpacing: 1.5})
		if line.Width != 26 {
			t.Errorf("width = %f, want 26 (only capped extra distributed)", line.Width)
		}
		if line.Glyphs[2].X != 16 {
			t.Errorf("glyph 2 X = %f, want 16", line.Glyphs[2].X)
		}
	})

	t.Run("CJKGapWeightRatio", func(t *testing.T) {
		// Weight 2.0 CJK gaps receive twice the share of a word space.
		line := &Line{
			Glyphs: []ShapedGlyph{
				mkGlyph(0, 0, 16, false, true),
				mkGlyph(1, 16, 16, false, true),
				mkGlyph(2, 32, 4, true, false),
				mkGlyph(3, 36, 10, false, false),
			},
			Width: 46,
		}
		justifyLine(line, 55, JustifyOptions{CJKGapWeight: 2})
		// extra 9 split 2:1 -> 6 to CJK gap, 3 to word space.
		if got := line.Glyphs[1].X - 16; math.Abs(got-6) > justifyEps {
			t.Errorf("CJK gap expansion = %f, want 6", got)
		}
		if got := line.Glyphs[3].X - 36; math.Abs(got-9) > justifyEps {
			t.Errorf("glyph 3 shift = %f, want 9", got)
		}
	})
}

// TestJustifyLine_MixedFontRuns tests justification across runs from
// different fonts: shifts apply per glyph regardless of run boundaries
// and run advances stay consistent with the final line width.
func TestJustifyLine_MixedFontRuns(t *testing.T) {
	latinFace := layoutTestFace(t)
	cjkFace := justifyTestFace(t, "notoseriftc_autohint_metrics.ttf", 16)

	glyphs := []ShapedGlyph{
		{Cluster: 0, X: 0, XAdvance: 8},                // latin run
		{Cluster: 1, X: 8, XAdvance: 4, IsSpace: true}, // latin run (space)
		{Cluster: 2, X: 12, XAdvance: 16, IsCJK: true}, // cjk run
		{Cluster: 3, X: 28, XAdvance: 16, IsCJK: true}, // cjk run
	}
	line := &Line{
		Glyphs: glyphs,
		Runs: []ShapedRun{
			{Glyphs: glyphs[:2], Advance: 12, Face: latinFace, Size: 16},
			{Glyphs: glyphs[2:], Advance: 32, Face: cjkFace, Size: 16},
		},
		Width: 44,
	}

	justifyLine(line, 64, JustifyOptions{})
	if line.Width != 64 {
		t.Errorf("width = %f, want 64", line.Width)
	}

	// Run glyphs received the same shifts as line glyphs.
	for i, run := range line.Runs {
		for j, g := range run.Glyphs {
			found := false
			for _, lg := range line.Glyphs {
				if lg.Cluster == g.Cluster && lg.X == g.X {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("run %d glyph %d (cluster %d, X=%f) not in sync with line glyphs",
					i, j, g.Cluster, g.X)
			}
		}
	}

	// Sum of run advances equals the final line width (runs are contiguous).
	var sum float64
	for _, run := range line.Runs {
		sum += run.Advance
	}
	if math.Abs(sum-line.Width) > justifyEps {
		t.Errorf("run advance sum = %f, want line width %f", sum, line.Width)
	}

	// Both the word space and the CJK gap expanded.
	if line.Glyphs[2].X <= 12 {
		t.Errorf("word space did not expand: glyph 2 X = %f", line.Glyphs[2].X)
	}
	if line.Glyphs[3].X-line.Glyphs[2].X <= 16 {
		t.Errorf("CJK gap did not expand: gap = %f", line.Glyphs[3].X-line.Glyphs[2].X)
	}
}

// TestLayoutText_JustifyRunsConsistency verifies that after justification
// the per-run glyphs match the line glyphs and run advances stay
// consistent with glyph positions.
func TestLayoutText_JustifyRunsConsistency(t *testing.T) {
	face := layoutTestFace(t)
	text := "Hello مرحبا World هذا نص mixed مع كلمات عربية and Latin words " +
		"together in one paragraph that wraps around"

	opts := LayoutOptions{
		MaxWidth:    220,
		LineSpacing: 1.0,
		Alignment:   AlignJustify,
		Direction:   DirectionLTR,
	}
	layout := LayoutText(text, face, opts)

	for i := range layout.Lines {
		line := &layout.Lines[i]
		var n int
		for _, run := range line.Runs {
			n += len(run.Glyphs)
		}
		if n != len(line.Glyphs) {
			t.Errorf("line %d: run glyphs %d != line glyphs %d", i, n, len(line.Glyphs))
		}
		// Run glyphs must be position-identical to line glyphs in order.
		k := 0
		for _, run := range line.Runs {
			for _, g := range run.Glyphs {
				if k < len(line.Glyphs) && g.X != line.Glyphs[k].X {
					t.Errorf("line %d glyph %d: run X=%f != line X=%f",
						i, k, g.X, line.Glyphs[k].X)
				}
				k++
			}
		}
	}
}
