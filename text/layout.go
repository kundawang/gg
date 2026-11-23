package text

import (
	"context"
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
	// AlignJustify distributes text evenly (future implementation).
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

			// Apply horizontal alignment
			applyAlignment(line, opts.Alignment, opts.MaxWidth)

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

	// Resolve full UAX #9 numeric levels (P1–I2, N0) for the paragraph and
	// shape each segment with its rune range and level attached.
	pb := resolveParagraphLevels(para, opts.Direction)
	runs := shapeBidiSegments(pb, segments, face, metrics)
	if len(runs) == 0 {
		return nil
	}

	// If no wrapping needed or WrapNone, return as single visually ordered line
	if opts.MaxWidth <= 0 || opts.WrapMode == WrapNone {
		line := createBidiLine(runs)
		reorderLineVisual(&line, pb)
		return []Line{line}
	}

	// Wrap lines at MaxWidth using the specified wrap mode, then apply the
	// UAX #9 L2 visual reordering independently to each resulting line.
	lines := wrapBidiLines(runs, para, opts.MaxWidth, opts.WrapMode, metrics, pb)
	for i := range lines {
		reorderLineVisual(&lines[i], pb)
	}
	return lines
}

// plainRuns strips the internal bidi rune-range tags for APIs that consume
// ordinary ShapedRun slices.
func plainRuns(runs []bidiShapedRun) []ShapedRun {
	out := make([]ShapedRun, len(runs))
	for i := range runs {
		out[i] = runs[i].ShapedRun
	}
	return out
}

// createBidiLine creates a Line from bidi-tagged runs in logical order.
func createBidiLine(runs []bidiShapedRun) Line {
	return createLine(plainRuns(runs))
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

// wrapBidiLines wraps bidi-tagged runs into lines at maxWidth. It mirrors
// wrapLinesWithMode but preserves UAX #9 levels on sub-runs so that every
// produced line can be visually reordered independently.
func wrapBidiLines(runs []bidiShapedRun, text string, maxWidth float64, mode WrapMode, metrics Metrics, pb *paragraphBidi) []Line {
	if len(runs) == 0 {
		return nil
	}

	plain := plainRuns(runs)

	// Flatten all glyphs with their run info for easier processing
	infos := make([]glyphInfo, 0)
	for runIdx, run := range plain {
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
		line := createLineFromGlyphsBidi(infos[lineStart:breakAt], plain, startX, metrics, pb)
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
		line := createLineFromGlyphsBidi(infos[lineStart:], plain, startX, metrics, pb)
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

// createLineFromGlyphsBidi is the bidi-aware counterpart of the legacy
// glyph-slice line builder: sub-runs inherit the resolved level and rune
// range of their source run so the L2 pass sees complete metadata. The
// optional mapper (nil tolerated) narrows each wrapped sub-run's rune range
// to the glyphs it actually contains on that line.
func createLineFromGlyphsBidi(infos []glyphInfo, runs []ShapedRun, startX float64, metrics Metrics, mapper subRunRuneMapper) Line {
	line := Line{
		Runs:    make([]ShapedRun, 0),
		Glyphs:  make([]ShapedGlyph, 0, len(infos)),
		Ascent:  metrics.Ascent,
		Descent: metrics.Descent,
	}

	if len(infos) == 0 {
		return line
	}

	// Track runs; each sub-run derives its own rune span from the glyphs
	// it actually contains (wrapping can split a source run across lines),
	// which is required for correct per-line rule-L2 reordering.
	currentRunIdx := -1

	for _, info := range infos {
		// Clone glyph and adjust X position to start from 0
		g := info.glyph
		g.X -= startX

		line.Glyphs = append(line.Glyphs, g)

		// Track runs
		if info.runIdx != currentRunIdx && info.runIdx < len(runs) {
			currentRunIdx = info.runIdx
			srcRun := &runs[info.runIdx]
			line.Runs = append(line.Runs, ShapedRun{
				Glyphs:    make([]ShapedGlyph, 0),
				Direction: srcRun.Direction,
				Face:      srcRun.Face,
				Size:      srcRun.Size,
				Ascent:    srcRun.Ascent,
				Descent:   srcRun.Descent,
				BidiLevel: srcRun.BidiLevel,
				RuneStart: srcRun.RuneStart,
				RuneEnd:   srcRun.RuneEnd,
			})
		}

		if len(line.Runs) > 0 {
			lastRunIdx := len(line.Runs) - 1
			line.Runs[lastRunIdx].Glyphs = append(line.Runs[lastRunIdx].Glyphs, g)
		}
	}

	// Narrow every sub-run's rune range to the span covered by its own
	// glyph clusters, using the paragraph byte->rune map when available.
	if mapper != nil {
		for i := range line.Runs {
			run := &line.Runs[i]
			if len(run.Glyphs) == 0 {
				continue
			}
			firstByte := run.Glyphs[0].Cluster
			lastByte := run.Glyphs[len(run.Glyphs)-1].Cluster
			run.RuneStart = mapper.runeIndexForByteOffset(firstByte)
			// RuneEnd is exclusive.
			run.RuneEnd = mapper.runeIndexForByteOffset(lastByte) + 1
		}
	}

	// Calculate line width and run advances
	if len(line.Glyphs) > 0 {
		lastGlyph := &line.Glyphs[len(line.Glyphs)-1]
		line.Width = lastGlyph.X + lastGlyph.XAdvance
	}

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
func applyAlignment(line *Line, alignment Alignment, maxWidth float64) {
	if len(line.Glyphs) == 0 {
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
	case AlignJustify:
		// Justify: distribute extra space between words
		// For now, treat as left-aligned (future implementation)
		return
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

// isWordBreakRune returns true if the rune is a word break opportunity.
func isWordBreakRune(r rune) bool {
	return unicode.IsSpace(r) || isCJK(r)
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
