// bidi_layout.go — UAX #9 visual reordering for laid-out lines.
//
// The segmenter already resolves mixed-direction *segments* (logical order),
// but LayoutText used to concatenate their shaped runs in logical order.
// This file adds the missing visual stage:
//
//   - resolveParagraphLevels runs the full UAX #9 algorithm (P1–I2 plus
//     bracket rule N0) over a paragraph and returns per-rune levels;
//   - shapeBidiSegments shapes each level/script segment while filtering
//     X9 formatting controls and remembering each run's rune range;
//   - reorderLineVisual applies rule L2 to a finished line: odd-level runs
//     are reversed as whole clusters (glyphs belonging to one source cluster
//     stay together, preserving GPOS geometry, advances, faces and original
//     text indices), then all glyphs are repositioned in visual order, and
//     the line's run slice is rebuilt to match the visual sequence.
//
// Pure LTR paragraphs take a fast path that leaves every glyph coordinate
// untouched, so existing single-direction output is byte-identical.
package text

import (
	"github.com/gogpu/gg/internal/bidi"
)

// paragraphBidi holds the resolved UAX #9 state for one paragraph.
type paragraphBidi struct {
	resolver   *bidix.Resolver
	runes      []rune
	byteToRune []int // byte offset -> rune index (len(para)+1 entries)
}

// resolveParagraphLevels runs UAX #9 over a single paragraph (no line
// separators). baseDirection maps LayoutOptions.Direction to the forced
// paragraph level; DirectionLTR/unknown leaves P2/P3 auto-detection.
func resolveParagraphLevels(para string, base Direction) *paragraphBidi {
	// range over the string yields the true byte offset of every decoded
	// rune, including RuneError placeholders for invalid UTF-8. This keeps
	// the byte/rune maps consistent even when []rune(para) would expand a
	// single bad byte into a (3-byte-encoded) replacement rune.
	runes := make([]rune, 0, len(para))
	byteToRune := make([]int, len(para)+1)

	offsets := make([]int, 0, len(para)+1)
	for byteOff, r := range para {
		offsets = append(offsets, byteOff)
		runes = append(runes, r)
	}
	offsets = append(offsets, len(para))
	n := len(runes)

	// Every byte in [offsets[i], offsets[i+1]) belongs to rune i.
	for k := range byteToRune {
		byteToRune[k] = n // sentinel for the trailing entry
	}
	for i := 0; i < n; i++ {
		for b := offsets[i]; b < offsets[i+1]; b++ {
			byteToRune[b] = i
		}
	}

	pb := &paragraphBidi{
		runes:      runes,
		byteToRune: byteToRune,
	}

	baseMode := bidix.DirectionDefault
	if base == DirectionRTL {
		baseMode = bidix.DirectionRightToLeft
	} else if base == DirectionLTR {
		baseMode = bidix.DirectionLeftToRight
	}
	pb.resolver = bidix.NewResolver(para, baseMode)
	return pb
}

// runeIndexForByteOffset translates a paragraph-global byte offset to the
// rune index of the rune starting at that offset.
func (pb *paragraphBidi) runeIndexForByteOffset(byteOff int) int {
	if byteOff < 0 {
		return 0
	}
	if byteOff >= len(pb.byteToRune) {
		return len(pb.runes)
	}
	return pb.byteToRune[byteOff]
}

// subRunRuneMapper resolves a paragraph byte offset to a rune index. It lets
// the wrapping pass tighten sub-run ranges without exposing the bidi internals
// to layout.go.
type subRunRuneMapper interface {
	runeIndexForByteOffset(byteOff int) int
}

// filterBidiControls removes the explicit embedding/override/isolate
// characters that rule X9 takes out of the rendered stream, while keeping
// every other rune (including invalid-UTF-8 RuneError) intact.
func filterBidiControls(s string) string {
	hasControl := false
	for _, r := range s {
		if bidix.IsBidiControl(r) {
			hasControl = true
			break
		}
	}
	if !hasControl {
		return s
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if !bidix.IsBidiControl(r) {
			out = append(out, r)
		}
	}
	return string(out)
}

// bidiShapedRun is a shaped run tagged with the source rune range needed for
// L2 reordering. Glyph clusters are paragraph-global byte offsets.
type bidiShapedRun struct {
	ShapedRun
	runeStart int
	runeEnd   int
}

// shapeBidiSegments shapes every level/script segment of a paragraph.
// X9 control characters are excluded from shaping (they produce no glyphs)
// but rune ranges still cover the original segment, so level lookup for the
// surviving glyphs is exact.
func shapeBidiSegments(pb *paragraphBidi, segments []Segment, face Face, metrics Metrics) []bidiShapedRun {
	runs := make([]bidiShapedRun, 0, len(segments))
	size := face.Size()
	var xOffset float64

	for _, seg := range segments {
		shapedText := filterBidiControls(seg.Text)
		glyphs := Shape(shapedText, face)
		if len(glyphs) == 0 {
			continue
		}

		// Segment byte offsets are relative to the paragraph.
		runeStart := pb.byteToRune[seg.Start]
		runeEnd := pb.byteToRune[seg.End]
		if runeEnd < runeStart {
			runeEnd = runeStart
		}
		level := pb.resolver.LevelAt(runeStart)

		// Glyphs come back with cluster indices local to shapedText (in
		// rune units). Convert them to paragraph-global byte offsets and
		// shift X by the accumulated logical advance.
		clusterBytes := pb.filteredClusterByteOffsets(seg, shapedText)
		for i := range glyphs {
			byteOff := seg.Start
			if c := glyphs[i].Cluster; c >= 0 && c < len(clusterBytes) {
				byteOff = seg.Start + clusterBytes[c]
			}
			glyphs[i].Cluster = byteOff
			glyphs[i].X += xOffset
		}

		var advance float64
		lastGlyph := &glyphs[len(glyphs)-1]
		advance = lastGlyph.X - xOffset + lastGlyph.XAdvance

		dir := DirectionLTR
		if level%2 == 1 {
			dir = DirectionRTL
		}
		runs = append(runs, bidiShapedRun{
			ShapedRun: ShapedRun{
				Glyphs:    glyphs,
				Advance:   advance,
				Ascent:    metrics.Ascent,
				Descent:   metrics.Descent,
				Direction: dir,
				Face:      face,
				Size:      size,
				BidiLevel: level,
				RuneStart: runeStart,
				RuneEnd:   runeEnd,
			},
			runeStart: runeStart,
			runeEnd:   runeEnd,
		})

		xOffset += advance
	}

	return runs
}

// filteredClusterByteOffsets returns, for each rune of the control-filtered
// segment text, its byte offset inside seg.Text.
func (pb *paragraphBidi) filteredClusterByteOffsets(seg Segment, shapedText string) []int {
	offsets := make([]int, 0, len([]rune(shapedText)))
	byteOff := 0
	for _, r := range seg.Text {
		if !bidix.IsBidiControl(r) {
			offsets = append(offsets, byteOff)
		}
		byteOff += len(string(r))
	}
	return offsets
}

// reorderLineVisual applies UAX #9 rule L2 to a finished line.
//
// The line must currently hold runs in logical order with glyph X positions
// laid out in logical order (the shapeBidiSegments output, or slices produced
// by the wrapping pass). On return the line holds:
//   - runs in visual order, each containing its visual-order glyphs;
//   - Glyphs in visual order with monotonically increasing X;
//   - unchanged Width, advances, faces and per-glyph source byte clusters.
//
// The visual sequence is derived from the same rule-L2 reordering array that
// passes the official BidiCharacterTest conformance suite, rather than a
// separate run-level simulation. Pure-LTR lines take an untouched fast path.
func reorderLineVisual(line *Line, pb *paragraphBidi) {
	if line == nil || len(line.Runs) == 0 || pb == nil {
		return
	}

	runs := line.Runs
	hasRTL := false
	for i := range runs {
		if runs[i].BidiLevel%2 == 1 {
			hasRTL = true
			break
		}
	}
	if !hasRTL {
		// Fast path: ensure the flattened glyph slice matches run order.
		rebuildLineGlyphs(line)
		return
	}

	// Determine the paragraph rune span covered by this line.
	lineStart, lineEnd := lineRuneSpan(runs)
	if lineEnd <= lineStart {
		rebuildLineGlyphs(line)
		return
	}

	// Official rule-L2 visual-to-logical rune order for this span.
	reorder := pb.resolver.LineReorder(lineStart, lineEnd)
	if len(reorder) == 0 {
		rebuildLineGlyphs(line)
		return
	}

	// Map each logical rune index to the run that renders it. X9 control
	// runes (no glyphs) map to -1 and are skipped during walk.
	runeToRun := make([]int, lineEnd-lineStart)
	for i := range runeToRun {
		runeToRun[i] = -1
	}
	for runIdx := range runs {
		lo := runs[runIdx].RuneStart - lineStart
		hi := runs[runIdx].RuneEnd - lineStart
		if lo < 0 {
			lo = 0
		}
		if hi > len(runeToRun) {
			hi = len(runeToRun)
		}
		for k := lo; k < hi; k++ {
			runeToRun[k] = runIdx
		}
	}

	// Walk the L2 visual rune order; the first time each run is encountered
	// fixes its visual position.
	seen := make(map[int]bool, len(runs))
	visualOrder := make([]int, 0, len(runs))
	for _, runeIdx := range reorder {
		local := runeIdx - lineStart
		if local < 0 || local >= len(runeToRun) {
			continue
		}
		runIdx := runeToRun[local]
		if runIdx < 0 || seen[runIdx] {
			continue
		}
		seen[runIdx] = true
		visualOrder = append(visualOrder, runIdx)
	}
	// Defensive: any run not reached (e.g. rune-range metadata gaps) keeps
	// its logical position rather than being dropped.
	for runIdx := range runs {
		if !seen[runIdx] {
			visualOrder = append(visualOrder, runIdx)
		}
	}

	// Emit runs in visual order. Odd-level runs have their glyph stream
	// stably reversed (clusters and GPOS placement preserved); even-level
	// runs keep theirs. Every run is laid out left-to-right in its box, so
	// the flattened glyph stream has monotonic X.
	visualRuns := make([]ShapedRun, 0, len(visualOrder))
	var pen float64
	visualGlyphs := make([]ShapedGlyph, 0, len(line.Glyphs))
	for _, runIdx := range visualOrder {
		run := runs[runIdx]
		if len(run.Glyphs) == 0 {
			continue
		}
		rtl := run.BidiLevel%2 == 1
		pen = layoutRunVisual(&run, pen, rtl)
		visualRuns = append(visualRuns, run)
		visualGlyphs = append(visualGlyphs, run.Glyphs...)
	}

	line.Runs = visualRuns
	line.Glyphs = visualGlyphs
}

// lineRuneSpan returns the half-open paragraph-rune range covered by a
// logical-ordered run slice.
func lineRuneSpan(runs []ShapedRun) (start, end int) {
	start = runs[0].RuneStart
	end = runs[0].RuneEnd
	for i := 1; i < len(runs); i++ {
		if runs[i].RuneStart < start {
			start = runs[i].RuneStart
		}
		if runs[i].RuneEnd > end {
			end = runs[i].RuneEnd
		}
	}
	return start, end
}

// layoutRunVisual lays a run's glyphs out inside the box beginning at x0 and
// returns the pen after the box. When rtl is true the glyph stream is first
// stably reversed (equal-cluster groups keep their internal order, so
// combining marks stay on their base); LTR runs keep their order.
//
// Every glyph carries a placement offset: its shaper X minus the cumulative
// advance of the preceding glyphs. That offset is captured before any
// reordering and re-added at the glyph's new visual position, so GPOS kerning
// and placement survive the reversal. XAdvance and source clusters are never
// modified.
func layoutRunVisual(run *ShapedRun, x0 float64, rtl bool) float64 {
	glyphs := run.Glyphs
	n := len(glyphs)
	if n == 0 {
		return x0
	}

	// 1. Capture placement offsets relative to the run's own origin. The
	//    shaped X values may carry a paragraph-level offset (each run was
	//    shifted by the accumulated logical advance), so subtract the first
	//    glyph's base X to obtain run-local coordinates, then remove the
	//    cumulative advance to isolate the per-glyph GPOS placement.
	baseX := glyphs[0].X
	placements := make([]float64, n)
	var total float64
	for i := 0; i < n; i++ {
		local := glyphs[i].X - baseX
		placements[i] = local - total
		total += glyphs[i].XAdvance
	}

	// 2. Build the visual index sequence: reversed-with-stable-clusters for
	//    RTL, identity for LTR.
	visualIdx := make([]int, n)
	if rtl {
		write := 0
		for i := n - 1; i >= 0; {
			j := i
			for j > 0 && glyphs[j-1].Cluster == glyphs[i].Cluster {
				j--
			}
			for k := j; k <= i; k++ { // group keeps internal order
				visualIdx[write] = k
				write++
			}
			i = j - 1
		}
	} else {
		for i := 0; i < n; i++ {
			visualIdx[i] = i
		}
	}

	// 3. Emit in visual order from x0, re-adding each glyph's own placement.
	out := make([]ShapedGlyph, n)
	pen := x0
	for visualPos, srcIdx := range visualIdx {
		g := glyphs[srcIdx]
		g.X = pen + placements[srcIdx]
		out[visualPos] = g
		pen += g.XAdvance
	}
	run.Glyphs = out
	run.Advance = total
	return pen
}

// rebuildLineGlyphs refreshes line.Glyphs from line.Runs without changing
// any positions (used on the pure-LTR fast path).
func rebuildLineGlyphs(line *Line) {
	total := 0
	for i := range line.Runs {
		total += len(line.Runs[i].Glyphs)
	}
	glyphs := make([]ShapedGlyph, 0, total)
	for i := range line.Runs {
		glyphs = append(glyphs, line.Runs[i].Glyphs...)
	}
	line.Glyphs = glyphs
}
