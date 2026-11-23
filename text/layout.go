package text

import (
	"context"
	"math"
	"strings"
	"unicode"
)

// Alignment specifies text horizontal alignment within the layout width.
type Alignment int

const (
	// AlignLeft aligns text to the left edge (default).
	AlignLeft Alignment = iota
	// AlignCenter centers text horizontally.
	AlignCenter
	// AlignRight aligns text to the right edge.
	AlignRight
	// AlignJustify distributes extra width across word spaces and CJK
	// gaps so that every non-final line of a paragraph spans MaxWidth.
	// The final line of each paragraph uses JustifyOptions.LastLineAlignment.
	AlignJustify
)

// String returns the string representation of the alignment.
func (a Alignment) String() string {
	switch a {
	case AlignLeft:
		return "Left"
	case AlignCenter:
		return "Center"
	case AlignRight:
		return "Right"
	case AlignJustify:
		return "Justify"
	default:
		return unknownStr
	}
}

// defaultCJKGapWeight is the expansion weight of inter-CJK gaps used when
// JustifyOptions.CJKGapWeight is zero.
const defaultCJKGapWeight = 1.0

// JustifyOptions configures justified alignment (AlignJustify).
// The zero value is valid and uses the documented defaults.
type JustifyOptions struct {
	// MaxWordSpacing caps the total width of an expanded word space at
	// this multiple of its original advance (e.g. 2.5 allows a space to
	// grow to 250% of its natural width). Extra width that cannot be
	// placed within the cap flows to uncapped expansion points (CJK gaps);
	// if none remain, the line keeps a smaller final width.
	// Values <= 0 (the default) mean no cap.
	MaxWordSpacing float64

	// CJKGapWeight is the expansion weight of gaps between two adjacent
	// CJK glyphs, relative to a word space (weight 1.0).
	// Zero selects the default (1.0); a negative value disables CJK gap
	// expansion entirely.
	CJKGapWeight float64

	// LastLineAlignment is applied to the final line of each paragraph
	// (the line before a hard break, or the only line of an unwrapped
	// paragraph). The zero value (AlignLeft) is the common choice.
	// AlignJustify here is treated as AlignLeft: final lines are never
	// stretched.
	LastLineAlignment Alignment
}

// LayoutOptions configures text layout behavior.
type LayoutOptions struct {
	// MaxWidth is the maximum line width in pixels.
	// If 0, no line wrapping is performed (single-line paragraphs).
	MaxWidth float64

	// LineSpacing is a multiplier for line height.
	// 1.0 uses the font's natural line height; 1.5 adds 50% extra space.
	LineSpacing float64

	// Alignment specifies horizontal text alignment.
	Alignment Alignment

	// Direction is the base text direction (LTR or RTL).
	// Used for paragraph-level direction when no strong directional text is present.
	Direction Direction

	// WrapMode specifies how text is wrapped when it exceeds MaxWidth.
	// Default is WrapWordChar which breaks at word boundaries first,
	// then falls back to character boundaries for long words.
	WrapMode WrapMode

	// Justify configures AlignJustify behavior (word-spacing cap,
	// CJK gap weight, last-line policy). Ignored for other alignments.
	Justify JustifyOptions
}

// DefaultLayoutOptions returns sensible default layout options.
func DefaultLayoutOptions() LayoutOptions {
	return LayoutOptions{
		MaxWidth:    0, // No wrapping (no MaxWidth constraint)
		LineSpacing: 1.0,
		Alignment:   AlignLeft,
		Direction:   DirectionLTR,
		// WrapMode defaults to WrapWordChar (zero value)
	}
}

// Line represents a positioned line of text ready for rendering.
type Line struct {
	// Runs contains the shaped runs that make up this line.
	// Multiple runs occur with mixed scripts or directions.
	Runs []ShapedRun

	// Glyphs contains all glyphs from Runs, positioned for rendering.
	// Glyph X positions are absolute within the layout.
	Glyphs []ShapedGlyph

	// Width is the total advance width of all glyphs in this line.
	Width float64

	// Ascent is the maximum ascent of all runs (distance above baseline).
	Ascent float64

	// Descent is the maximum descent of all runs (distance below baseline).
	Descent float64

	// Y is the baseline Y position of this line within the layout.
	Y float64
}

// Height returns the total height of the line (ascent + descent).
func (l *Line) Height() float64 {
	return l.Ascent + l.Descent
}

// Layout represents the result of text layout.
type Layout struct {
	// Lines contains all lines of laid out text.
	Lines []Line

	// Width is the maximum width among all lines.
	Width float64

	// Height is the total height of all lines.
	Height float64
}

// LayoutText performs text layout with the given options.
// It segments text by direction/script, shapes each segment,
// wraps lines if MaxWidth > 0, and positions lines with alignment.
// The font size is obtained from face.Size().
//
// For cancellable layout, use LayoutTextWithContext.
func LayoutText(text string, face Face, opts LayoutOptions) *Layout {
	layout, _ := LayoutTextWithContext(context.Background(), text, face, opts)
	return layout
}

// LayoutTextWithContext performs text layout with the given options and cancellation support.
// It segments text by direction/script, shapes each segment,
// wraps lines if MaxWidth > 0, and positions lines with alignment.
// The font size is obtained from face.Size().
//
// The context can be used to cancel long-running layout operations.
// When canceled, returns nil and ctx.Err().
func LayoutTextWithContext(ctx context.Context, text string, face Face, opts LayoutOptions) (*Layout, error) {
	if text == "" {
		return &Layout{}, nil
	}
	if face == nil {
		return &Layout{}, nil
	}

	// Check for cancellation at start
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// Apply defaults for zero values
	if opts.LineSpacing <= 0 {
		opts.LineSpacing = 1.0
	}

	// Get font metrics for line height calculation
	metrics := face.Metrics()

	// Split by hard line breaks
	paragraphs := splitParagraphs(text)

	layout := &Layout{
		Lines: make([]Line, 0, len(paragraphs)),
	}

	var y float64

	for i, para := range paragraphs {
		// Check for cancellation periodically (every 8 paragraphs)
		if i%8 == 0 && i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
		}

		paraLines := layoutParagraph(para, face, opts, metrics)

		for j := range paraLines {
			line := &paraLines[j]

			// Calculate baseline position: previous Y + line ascent
			if len(layout.Lines) == 0 && j == 0 {
				// Very first line of entire layout
				y = line.Ascent
			} else {
				// Get previous line: from current paragraph or from layout.Lines
				var prevLine *Line
				if j > 0 {
					prevLine = &paraLines[j-1]
				} else {
					prevLine = &layout.Lines[len(layout.Lines)-1]
				}
				lineGap := metrics.LineGap * opts.LineSpacing
				y = prevLine.Y + prevLine.Descent + lineGap + line.Ascent
			}

			line.Y = y

			// Apply horizontal alignment. The last line of each paragraph
			// (before a hard break) follows the last-line policy.
			isLastLine := j == len(paraLines)-1
			applyAlignment(line, opts.Alignment, opts.MaxWidth, opts.Justify, isLastLine)

			// Track layout bounds
			if line.Width > layout.Width {
				layout.Width = line.Width
			}
		}

		layout.Lines = append(layout.Lines, paraLines...)
	}

	// Calculate total height
	if len(layout.Lines) > 0 {
		lastLine := &layout.Lines[len(layout.Lines)-1]
		layout.Height = lastLine.Y + lastLine.Descent
	}

	return layout, nil
}

// LayoutTextSimple is a convenience wrapper with default options.
// The font size is obtained from face.Size().
func LayoutTextSimple(text string, face Face) *Layout {
	return LayoutText(text, face, DefaultLayoutOptions())
}

// splitParagraphs splits text by hard line breaks.
func splitParagraphs(text string) []string {
	// Normalize line endings
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	return strings.Split(text, "\n")
}

// layoutParagraph lays out a single paragraph (no hard breaks).
func layoutParagraph(para string, face Face, opts LayoutOptions, metrics Metrics) []Line {
	if para == "" {
		// Empty paragraph still produces a line (for line height)
		return []Line{{
			Runs:    nil,
			Glyphs:  nil,
			Width:   0,
			Ascent:  metrics.Ascent,
			Descent: metrics.Descent,
		}}
	}

	// Segment the paragraph by direction and script
	segmenter := NewBuiltinSegmenterWithDirection(opts.Direction)
	segments := segmenter.Segment(para)

	if len(segments) == 0 {
		return nil
	}

	// Shape each segment
	runs := shapeSegments(segments, face, metrics)

	// If no wrapping needed or WrapNone, return as single line
	if opts.MaxWidth <= 0 || opts.WrapMode == WrapNone {
		return []Line{createLine(runs)}
	}

	// Wrap lines at MaxWidth using the specified wrap mode
	return wrapLinesWithMode(runs, para, opts.MaxWidth, opts.WrapMode, metrics)
}

// shapeSegments shapes each segment and returns ShapedRuns.
func shapeSegments(segments []Segment, face Face, metrics Metrics) []ShapedRun {
	runs := make([]ShapedRun, 0, len(segments))
	size := face.Size()
	var xOffset float64

	for _, seg := range segments {
		glyphs := Shape(seg.Text, face)
		if len(glyphs) == 0 {
			continue
		}

		// Adjust glyph positions by xOffset
		for i := range glyphs {
			glyphs[i].X += xOffset
			// Adjust cluster indices to be relative to paragraph start
			glyphs[i].Cluster += seg.Start
		}

		// Calculate run advance
		var advance float64
		if len(glyphs) > 0 {
			lastGlyph := &glyphs[len(glyphs)-1]
			advance = lastGlyph.X - xOffset + lastGlyph.XAdvance
		}

		run := ShapedRun{
			Glyphs:    glyphs,
			Advance:   advance,
			Ascent:    metrics.Ascent,
			Descent:   metrics.Descent,
			Direction: seg.Direction,
			Face:      face,
			Size:      size,
		}
		runs = append(runs, run)

		xOffset += advance
	}

	return runs
}

// createLine creates a Line from runs.
func createLine(runs []ShapedRun) Line {
	line := Line{
		Runs:   runs,
		Glyphs: make([]ShapedGlyph, 0),
	}

	for i := range runs {
		run := &runs[i]
		line.Glyphs = append(line.Glyphs, run.Glyphs...)
		line.Width += run.Advance

		if run.Ascent > line.Ascent {
			line.Ascent = run.Ascent
		}
		if run.Descent > line.Descent {
			line.Descent = run.Descent
		}
	}

	return line
}

// glyphInfo is used internally for line wrapping.
type glyphInfo struct {
	glyph   ShapedGlyph
	runIdx  int
	isBreak bool
}

// wrapLinesWithMode wraps runs into lines at maxWidth using the specified wrap mode.
func wrapLinesWithMode(runs []ShapedRun, text string, maxWidth float64, mode WrapMode, metrics Metrics) []Line {
	if len(runs) == 0 {
		return nil
	}

	// Flatten all glyphs with their run info for easier processing
	infos := make([]glyphInfo, 0)
	for runIdx, run := range runs {
		for _, g := range run.Glyphs {
			infos = append(infos, glyphInfo{
				glyph:   g,
				runIdx:  runIdx,
				isBreak: false,
			})
		}
	}

	if len(infos) == 0 {
		return []Line{{
			Ascent:  metrics.Ascent,
			Descent: metrics.Descent,
		}}
	}

	// Mark break opportunities based on wrap mode
	if text != "" && mode != WrapNone {
		markBreakOpportunitiesEnhanced(infos, text, mode)
	} else {
		markBreakOpportunities(infos)
	}

	// Greedy line breaking
	lines := make([]Line, 0)
	lineStart := 0
	lastBreak := -1
	startX := infos[0].glyph.X

	for i := range infos {
		info := &infos[i]
		glyphEnd := info.glyph.X - startX + info.glyph.XAdvance

		if info.isBreak {
			lastBreak = i
		}

		// Check if line exceeds max width
		if glyphEnd <= maxWidth || lineStart >= i {
			continue
		}

		// Calculate break position based on mode
		breakAt, shouldBreak := calculateBreakPosition(i, lineStart, lastBreak, mode)
		if !shouldBreak {
			continue
		}

		// Create line from lineStart to breakAt
		line := createLineFromGlyphs(infos[lineStart:breakAt], runs, startX, metrics)
		lines = append(lines, line)

		// Start new line
		lineStart = breakAt
		lastBreak = -1
		if lineStart < len(infos) {
			startX = infos[lineStart].glyph.X
		}
	}

	// Add remaining glyphs as final line
	if lineStart < len(infos) {
		line := createLineFromGlyphs(infos[lineStart:], runs, startX, metrics)
		lines = append(lines, line)
	}

	return lines
}

// calculateBreakPosition determines where to break a line based on wrap mode.
// Returns (breakPosition, shouldBreak).
func calculateBreakPosition(currentPos, lineStart, lastBreak int, mode WrapMode) (int, bool) {
	switch {
	case lastBreak > lineStart:
		// Break at last break opportunity
		return lastBreak + 1, true
	case mode == WrapWordChar || mode == WrapChar:
		// Fall back to character break
		return currentPos, true
	case mode == WrapWord:
		// Don't break, let it overflow until we find a break point
		return 0, false
	default:
		return currentPos, true
	}
}

// markBreakOpportunities marks positions where line breaks can occur.
// Currently uses a simple heuristic - every glyph is a potential break point.
// Future: Use Unicode line breaking algorithm (UAX #14).
func markBreakOpportunities(infos []glyphInfo) {
	// Simple heuristic: allow break at every glyph boundary.
	// This is a placeholder for proper Unicode line breaking.
	for i := range infos {
		infos[i].isBreak = true
	}
}

// createLineFromGlyphs creates a Line from a slice of glyph infos.
func createLineFromGlyphs(infos []glyphInfo, runs []ShapedRun, startX float64, metrics Metrics) Line {
	line := Line{
		Runs:    make([]ShapedRun, 0),
		Glyphs:  make([]ShapedGlyph, 0, len(infos)),
		Ascent:  metrics.Ascent,
		Descent: metrics.Descent,
	}

	if len(infos) == 0 {
		return line
	}

	// Collect glyphs and adjust X positions
	currentRunIdx := -1
	var currentRun *ShapedRun

	for _, info := range infos {
		// Clone glyph and adjust X position to start from 0
		g := info.glyph
		g.X -= startX

		line.Glyphs = append(line.Glyphs, g)

		// Track runs
		if info.runIdx != currentRunIdx && info.runIdx < len(runs) {
			currentRunIdx = info.runIdx
			srcRun := &runs[info.runIdx]
			// Create a new run for this line
			currentRun = &ShapedRun{
				Glyphs:    make([]ShapedGlyph, 0),
				Direction: srcRun.Direction,
				Face:      srcRun.Face,
				Size:      srcRun.Size,
				Ascent:    srcRun.Ascent,
				Descent:   srcRun.Descent,
			}
			line.Runs = append(line.Runs, *currentRun)
		}

		// Add glyph to current run
		if len(line.Runs) > 0 {
			lastRunIdx := len(line.Runs) - 1
			line.Runs[lastRunIdx].Glyphs = append(line.Runs[lastRunIdx].Glyphs, g)
		}
	}

	// Calculate line width and run advances
	if len(line.Glyphs) > 0 {
		lastGlyph := &line.Glyphs[len(line.Glyphs)-1]
		line.Width = lastGlyph.X + lastGlyph.XAdvance
	}

	// Update run advances
	for i := range line.Runs {
		run := &line.Runs[i]
		if len(run.Glyphs) > 0 {
			firstG := &run.Glyphs[0]
			lastG := &run.Glyphs[len(run.Glyphs)-1]
			run.Advance = lastG.X - firstG.X + lastG.XAdvance
		}
	}

	return line
}

// applyAlignment adjusts glyph X positions based on alignment.
// isLastLine reports whether the line is the final line of its paragraph
// (relevant only for AlignJustify).
func applyAlignment(line *Line, alignment Alignment, maxWidth float64, justify JustifyOptions, isLastLine bool) {
	if len(line.Glyphs) == 0 {
		return
	}

	if alignment == AlignJustify {
		if isLastLine {
			// Final line of a paragraph: apply the configured policy.
			// AlignJustify here would stretch the last line; treat as left.
			last := justify.LastLineAlignment
			if last == AlignJustify {
				last = AlignLeft
			}
			applyAlignment(line, last, maxWidth, justify, false)
			return
		}
		justifyLine(line, maxWidth, justify)
		return
	}

	var offset float64

	switch alignment {
	case AlignLeft:
		// No adjustment needed
		return
	case AlignCenter:
		containerWidth := maxWidth
		if containerWidth <= 0 {
			containerWidth = line.Width
		}
		offset = (containerWidth - line.Width) / 2
	case AlignRight:
		containerWidth := maxWidth
		if containerWidth <= 0 {
			containerWidth = line.Width
		}
		offset = containerWidth - line.Width
	}

	if offset <= 0 {
		return
	}

	// Adjust all glyph positions
	for i := range line.Glyphs {
		line.Glyphs[i].X += offset
	}

	// Adjust run glyphs as well
	for i := range line.Runs {
		for j := range line.Runs[i].Glyphs {
			line.Runs[i].Glyphs[j].X += offset
		}
	}
}

// expansionPoint is a gap after a glyph where justification may add width.
type expansionPoint struct {
	// afterGlyph is the index of the glyph preceding the gap.
	afterGlyph int
	// weight is the relative share of extra width this gap receives.
	weight float64
	// maxExtra caps the extra width added at this gap (math.Inf(1) if uncapped).
	maxExtra float64
}

// justifyLine distributes extra width across a line's expansion points so
// the line spans maxWidth. Expansion points are gaps after word-space
// glyphs (weight 1, optionally capped) and gaps between adjacent CJK
// glyphs (weight JustifyOptions.CJKGapWeight). Gaps inside a cluster
// (ligatures, contextual forms) are never expanded.
//
// The function is deterministic and total: it never leaks width into
// other lines, and leaves the line unchanged when justification is
// impossible or undefined (no expansion points, non-positive or
// non-finite maxWidth, or a line already wider than maxWidth).
func justifyLine(line *Line, maxWidth float64, opts JustifyOptions) {
	if len(line.Glyphs) < 2 {
		return
	}
	if maxWidth <= 0 || math.IsNaN(maxWidth) || math.IsInf(maxWidth, 0) {
		return
	}
	extra := maxWidth - line.Width
	if extra <= 0 {
		return
	}

	cjkWeight := opts.CJKGapWeight
	switch {
	case cjkWeight == 0:
		cjkWeight = defaultCJKGapWeight
	case cjkWeight < 0:
		cjkWeight = 0
	}

	// Collect expansion points between adjacent glyphs (visual order).
	glyphs := line.Glyphs
	points := make([]expansionPoint, 0, len(glyphs)/2)
	for i := 0; i+1 < len(glyphs); i++ {
		// Never split a cluster: glyphs sharing a cluster index
		// (ligatures, Arabic contextual forms) move as a unit.
		if glyphs[i].Cluster == glyphs[i+1].Cluster {
			continue
		}
		switch {
		case glyphs[i].IsSpace:
			maxExtra := math.Inf(1)
			if opts.MaxWordSpacing > 0 {
				adv := glyphs[i].XAdvance
				if adv < 0 {
					adv = 0
				}
				maxExtra = (opts.MaxWordSpacing - 1) * adv
				if maxExtra < 0 {
					maxExtra = 0
				}
			}
			points = append(points, expansionPoint{afterGlyph: i, weight: 1, maxExtra: maxExtra})
		case cjkWeight > 0 && glyphs[i].IsCJK && glyphs[i+1].IsCJK:
			points = append(points, expansionPoint{afterGlyph: i, weight: cjkWeight, maxExtra: math.Inf(1)})
		}
	}
	if len(points) == 0 {
		return
	}

	// Distribute `extra` proportionally to weights, respecting per-point
	// caps (water-filling: capped points are saturated and the remainder
	// is redistributed among the rest).
	extras := make([]float64, len(points))
	active := make([]bool, len(points))
	for i := range active {
		active[i] = true
	}
	remaining := extra
	for remaining > 1e-9 {
		var totalWeight float64
		for k := range points {
			if active[k] {
				totalWeight += points[k].weight
			}
		}
		if totalWeight <= 0 {
			break
		}
		saturated := false
		for k := range points {
			if !active[k] {
				continue
			}
			share := remaining * points[k].weight / totalWeight
			if extras[k]+share >= points[k].maxExtra {
				remaining -= points[k].maxExtra - extras[k]
				extras[k] = points[k].maxExtra
				active[k] = false
				saturated = true
			}
		}
		if !saturated {
			// No point hits its cap: distribute everything and finish.
			for k := range points {
				if active[k] {
					extras[k] += remaining * points[k].weight / totalWeight
				}
			}
			remaining = 0
		}
	}
	distributed := extra - remaining
	if distributed <= 0 {
		return
	}

	// Accumulate per-gap extras into per-glyph shifts, then apply.
	gapExtra := make([]float64, len(glyphs)-1)
	for k, p := range points {
		gapExtra[p.afterGlyph] = extras[k]
	}
	shifts := make([]float64, len(glyphs))
	var acc float64
	for i := 1; i < len(glyphs); i++ {
		acc += gapExtra[i-1]
		shifts[i] = acc
	}
	for i := range glyphs {
		glyphs[i].X += shifts[i]
	}

	// Apply the same shifts to run glyphs. Runs partition the line's
	// glyphs in order, so a running index maps each run glyph to its
	// shift; run advances are then recomputed from positions.
	gi := 0
	for i := range line.Runs {
		run := &line.Runs[i]
		for j := range run.Glyphs {
			if gi < len(shifts) {
				run.Glyphs[j].X += shifts[gi]
			}
			gi++
		}
		if len(run.Glyphs) > 0 {
			firstG := &run.Glyphs[0]
			lastG := &run.Glyphs[len(run.Glyphs)-1]
			run.Advance = lastG.X - firstG.X + lastG.XAdvance
		}
	}

	line.Width += distributed
}

// isWordBreakRune returns true if the rune is a word break opportunity.
func isWordBreakRune(r rune) bool {
	return unicode.IsSpace(r) || isCJK(r)
}

// isWordSpaceRune returns true if the rune is a Unicode space separator
// (category Zs: U+0020, U+00A0, U+2000–U+200A, …). Such runes mark
// expandable word-space gaps for justified alignment.
func isWordSpaceRune(r rune) bool {
	return unicode.Is(unicode.Zs, r)
}

// isCJK returns true if the rune is a CJK character.
// CJK characters can break anywhere (no word boundaries).
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
		(r >= 0x3400 && r <= 0x4DBF) || // CJK Extension A
		(r >= 0x20000 && r <= 0x2A6DF) || // CJK Extension B
		(r >= 0x3040 && r <= 0x309F) || // Hiragana
		(r >= 0x30A0 && r <= 0x30FF) || // Katakana
		(r >= 0xAC00 && r <= 0xD7AF) // Hangul Syllables
}
