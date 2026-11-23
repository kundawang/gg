// Vertical layout engine — builds Columns from shaped horizontal runs.
package text

import (
	"context"
	"unicode"
)

// verticalGlyph is a shaped glyph annotated with everything the vertical
// engine needs: its source rune/class, resolved orientation, and the
// vertical metrics in Y-down pixel space.
type verticalGlyph struct {
	glyph     ShapedGlyph
	r         rune
	class     VerticalClass
	orient    VerticalOrient
	vAdvance  float64 // block-axis advance (upright) or 0 (sideways runs)
	originX   float64 // upright only: vertical origin offset from h-origin
	originY   float64 // upright only: vertical origin offset from h-baseline
	runIdx    int     // source run index (for rebuilding per-column runs)
	breakable bool    // wrap opportunity after this glyph
}

// layoutTextVertical is the vertical entry point called from
// LayoutTextWithContext. It mirrors the horizontal pipeline:
// paragraphs → columns (with optional height-constrained wrapping) →
// absolute glyph positions and layout bounds.
func layoutTextVertical(ctx context.Context, text string, face Face, opts LayoutOptions) (*Layout, error) {
	paragraphs := splitParagraphs(text)
	metrics := face.Metrics()

	layout := &Layout{Vertical: true, Columns: make([]Column, 0, len(paragraphs))}

	for i, para := range paragraphs {
		if i%8 == 0 && i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
		}

		cols := layoutVerticalParagraph(para, face, opts, metrics)
		layout.Columns = append(layout.Columns, cols...)
	}

	if len(layout.Columns) == 0 {
		return layout, nil
	}

	positionVerticalColumns(layout, face, opts)
	return layout, nil
}

// layoutVerticalParagraph shapes one paragraph and splits it into one or
// more columns (hard-break paragraphs arrive pre-split, so this handles
// MaxWidth-driven column wrapping).
func layoutVerticalParagraph(para string, face Face, opts LayoutOptions, metrics Metrics) []Column {
	// Empty paragraph still reserves a column of natural line height,
	// matching the empty-line behavior of horizontal layout.
	if para == "" {
		return []Column{emptyVerticalColumn(metrics)}
	}

	segmenter := NewBuiltinSegmenterWithDirection(opts.Direction)
	segments := segmenter.Segment(para)
	if len(segments) == 0 {
		return nil
	}

	runs := shapeSegments(segments, face, metrics)
	if len(runs) == 0 {
		return []Column{emptyVerticalColumn(metrics)}
	}

	vgs := annotateVerticalGlyphs(runs, segments, para, face, opts.Vertical)
	if len(vgs) == 0 {
		return []Column{emptyVerticalColumn(metrics)}
	}

	maxHeight := opts.MaxWidth // reused as the column-height constraint
	if !finiteWidth(maxHeight) || opts.WrapMode == WrapNone {
		maxHeight = 0
	}

	chunks := splitVerticalChunks(vgs, maxHeight, opts.WrapMode)
	columns := make([]Column, 0, len(chunks))
	for _, chunk := range chunks {
		columns = append(columns, buildVerticalColumn(chunk, runs, metrics, opts.Direction))
	}
	return columns
}

// emptyVerticalColumn returns a zero-glyph column sized to the font's line
// height so empty paragraphs keep their vertical rhythm.
func emptyVerticalColumn(metrics Metrics) Column {
	return Column{
		Height:  metrics.Ascent + metrics.Descent,
		Ascent:  metrics.Ascent,
		Descent: metrics.Descent,
	}
}

// annotateVerticalGlyphs flattens shaped runs and resolves each glyph's
// vertical class, orientation and vertical metrics.
//
// The shaper produces rune-index clusters relative to each segment;
// shapeSegments then adds a BYTE offset (seg.Start) for horizontal use,
// which is not a valid rune index for mixed-script paragraphs. Vertical
// layout therefore remaps clusters to paragraph-global RUNE indices
// (stable and directly usable for hit testing), tracking the running rune
// count at the start of each segment.
func annotateVerticalGlyphs(runs []ShapedRun, segments []Segment, para string, face Face, vo *VerticalOptions) []verticalGlyph {
	runes := []rune(para)
	parsed := face.Source().Parsed()
	vmp, _ := parsed.(VerticalMetricProvider)
	size := face.Size()

	// Rune index at the start of each segment, in segment order.
	segmentRuneStart := make([]int, len(segments)+1)
	for i, seg := range segments {
		segmentRuneStart[i+1] = segmentRuneStart[i] + len([]rune(seg.Text))
	}

	out := make([]verticalGlyph, 0)
	for runIdx := range runs {
		run := &runs[runIdx]
		// Recover the shaper-local cluster by subtracting the segment byte
		// offset that shapeSegments added.
		byteOffset := 0
		if runIdx < len(segments) {
			byteOffset = segments[runIdx].Start
		}

		for i := range run.Glyphs {
			g := run.Glyphs[i]
			localCluster := g.Cluster - byteOffset
			globalRuneIdx := segmentRuneStart[runIdx] + localCluster
			r := runeAt(runes, globalRuneIdx)
			class := ClassifyVerticalRune(r)
			orient := resolveVerticalOrient(r, class, vo, out)

			vg := verticalGlyph{
				glyph:     g,
				r:         r,
				class:     class,
				orient:    orient,
				runIdx:    runIdx,
				breakable: isWordBreakRune(r),
			}
			// Expose the paragraph-global rune index as the logical cluster.
			vg.glyph.Cluster = globalRuneIdx
			vg.glyph.VerticalOrient = orient
			if orient == VerticalOrientUpright {
				vg.vAdvance, vg.originX, vg.originY = verticalGlyphMetrics(vmp, uint16(g.GID), size)
			}
			out = append(out, vg)
		}
	}
	return out
}

// runeAt safely indexes the paragraph rune slice.
func runeAt(runes []rune, idx int) rune {
	if idx >= 0 && idx < len(runes) {
		return runes[idx]
	}
	return 0
}

// resolveVerticalOrient applies per-class overrides, the global policy and
// the mark-inherits-base rule in that order.
//
// Mark inheritance is driven by the source rune, not the glyph ID: missing
// glyphs map to GID 0, which can collide with a real combining mark's
// notdef and would otherwise inherit the wrong orientation.
func resolveVerticalOrient(r rune, class VerticalClass, vo *VerticalOptions, prior []verticalGlyph) VerticalOrient {
	if class == VerticalClassMark && len(prior) > 0 && unicode.IsMark(r) {
		return prior[len(prior)-1].orient
	}
	if mode, ok := classOverride(vo, class); ok {
		return mode
	}
	if vo != nil && vo.Orientation == VerticalOrientSideways {
		return VerticalOrientSideways
	}
	return autoVerticalOrient(class)
}

// classOverride reports an explicit per-class orientation override.
func classOverride(vo *VerticalOptions, class VerticalClass) (VerticalOrient, bool) {
	if vo == nil || vo.ClassModes == nil {
		return 0, false
	}
	mode, ok := vo.ClassModes[class]
	return mode, ok
}

// verticalGlyphMetrics returns (advance height, originX, originY) for an
// upright glyph, synthesizing em-square metrics when the font has no
// vhea/vmtx tables.
func verticalGlyphMetrics(vmp VerticalMetricProvider, gid uint16, size float64) (advance, ox, oy float64) {
	if vmp == nil {
		return size, size / 2, -size / 2
	}
	ox, oy = vmp.VerticalOrigin(gid, size)
	return vmp.VerticalAdvance(gid, size), ox, oy
}
