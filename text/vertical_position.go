// Absolute column positioning and layout bounds for vertical layout.
package text

// positionVerticalColumns translates per-column local glyph coordinates
// into layout-absolute coordinates, resolves the column progression
// direction (TTB defaults to right-to-left, BTT to left-to-right), applies
// block-axis alignment and computes final layout bounds.
func positionVerticalColumns(layout *Layout, face Face, opts LayoutOptions) {
	cols := layout.Columns
	if len(cols) == 0 {
		return
	}

	gap := columnGap(face, opts)
	pitch := columnPitch(cols, gap)
	flow := resolveColumnFlow(opts.Vertical, opts.Direction)
	blockWidth := columnBlockWidth(cols, pitch)
	maxHeight := layout.maxColumnHeight()

	// Alignment offsets the whole column block along the progression axis.
	startCenter := firstCenterLine(cols, blockWidth, opts.Alignment, flow)

	for i := range cols {
		col := &layout.Columns[i]

		var centerX float64
		if flow == ColumnFlowRightToLeft {
			centerX = startCenter - float64(i)*pitch
		} else {
			centerX = startCenter + float64(i)*pitch
		}
		col.X = centerX

		applyColumnTranslation(col, centerX, 0)
	}

	layout.Height = maxHeight
	layout.Width = blockWidth
}

// columnGap returns the extra spacing between adjacent columns.
func columnGap(face Face, opts LayoutOptions) float64 {
	gap := opts.Vertical.ColumnSpacing
	if vmp, ok := face.Source().Parsed().(VerticalMetricProvider); ok {
		gap += vmp.VerticalLineGap(face.Size())
	}
	if gap < 0 {
		return 0
	}
	return gap
}

// columnPitch is the center-line distance between adjacent columns: the
// widest column extent plus the configured gap.
func columnPitch(cols []Column, gap float64) float64 {
	var maxWidth float64
	for i := range cols {
		if cols[i].Width > maxWidth {
			maxWidth = cols[i].Width
		}
	}
	return maxWidth + gap
}

// resolveColumnFlow maps the explicit flow override, defaulting to
// right-to-left for TTB and left-to-right for BTT.
func resolveColumnFlow(vo *VerticalOptions, dir Direction) ColumnFlow {
	if vo != nil && vo.Flow != ColumnFlowDefault {
		return vo.Flow
	}
	if dir == DirectionBTT {
		return ColumnFlowLeftToRight
	}
	return ColumnFlowRightToLeft
}

// columnBlockWidth is the total horizontal extent of all columns.
func columnBlockWidth(cols []Column, pitch float64) float64 {
	if len(cols) == 0 {
		return 0
	}
	first := cols[0].Width / 2
	last := cols[len(cols)-1].Width / 2
	return first + float64(len(cols)-1)*pitch + last
}

// maxColumnHeight returns the tallest column height.
func (l *Layout) maxColumnHeight() float64 {
	maxHeight := 0.0
	for i := range l.Columns {
		if l.Columns[i].Height > maxHeight {
			maxHeight = l.Columns[i].Height
		}
	}
	return maxHeight
}

// firstCenterLine returns the center line of the first column, honoring
// horizontal alignment within the block's own width (no external container
// width is available; MaxWidth acts as the column-height constraint).
func firstCenterLine(cols []Column, blockWidth float64, align Alignment, flow ColumnFlow) float64 {
	firstHalf := cols[0].Width / 2
	switch align {
	case AlignCenter:
		return blockWidth / 2
	case AlignRight:
		return blockWidth - firstHalf
	default: // AlignLeft, AlignJustify (justify is a horizontal-only feature)
		if flow == ColumnFlowRightToLeft {
			// Right-to-left: the first column is the rightmost.
			return blockWidth - firstHalf
		}
		return firstHalf
	}
}

// applyColumnTranslation moves a column's glyphs from local (column-relative)
// coordinates to layout-absolute coordinates. Local X is relative to the
// column center line (positive = right); local Y to the column top.
func applyColumnTranslation(col *Column, centerX, topY float64) {
	for i := range col.Glyphs {
		col.Glyphs[i].X += centerX
		col.Glyphs[i].Y += topY
	}
	for i := range col.Runs {
		for j := range col.Runs[i].Glyphs {
			col.Runs[i].Glyphs[j].X += centerX
			col.Runs[i].Glyphs[j].Y += topY
		}
	}
}
