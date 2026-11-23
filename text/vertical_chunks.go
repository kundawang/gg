// Vertical column breaking, construction and positioning.
package text

// splitVerticalChunks greedily splits annotated glyphs into column-sized
// chunks at the given maximum block-axis height. A maxHeight of zero means
// a single (unbounded) column. Break handling mirrors the horizontal
// wrap modes:
//
//   - WrapWordChar: prefer word/ideograph boundaries, fall back to chars.
//   - WrapChar: break at any glyph boundary.
//   - WrapWord: only word boundaries; long runs overflow one column.
func splitVerticalChunks(vgs []verticalGlyph, maxHeight float64, mode WrapMode) [][]verticalGlyph {
	if len(vgs) == 0 {
		return nil
	}
	if maxHeight <= 0 || mode == WrapNone {
		return [][]verticalGlyph{vgs}
	}

	chunks := make([][]verticalGlyph, 0)
	start := 0
	lastBreak := -1
	pen := 0.0

	for i := range vgs {
		h := verticalGlyphHeight(&vgs[i])
		if vgs[i].breakable {
			lastBreak = i
		}

		// First glyph always fits, even when larger than the column.
		if pen+h <= maxHeight || start == i {
			pen += h
			continue
		}

		breakAt, ok := verticalBreakPosition(i, start, lastBreak, mode)
		if !ok {
			pen += h // WrapWord: overflow until a break opportunity appears
			continue
		}

		chunks = append(chunks, vgs[start:breakAt])
		start = breakAt
		lastBreak = -1
		pen = sumHeights(vgs[start:i])
	}

	chunks = append(chunks, vgs[start:])
	return chunks
}

// verticalBreakPosition decides where to break an overflowing column.
func verticalBreakPosition(current, start, lastBreak int, mode WrapMode) (int, bool) {
	switch {
	case lastBreak > start:
		return lastBreak + 1, true
	case mode == WrapWordChar || mode == WrapChar:
		return current, true
	default: // WrapWord: no valid break yet
		return 0, false
	}
}

// verticalGlyphHeight is the block-axis extent of one glyph. Sideways
// glyphs measure along the horizontal advance since the run is rotated.
func verticalGlyphHeight(vg *verticalGlyph) float64 {
	if vg.orient == VerticalOrientSideways {
		return vg.glyph.XAdvance
	}
	return vg.vAdvance
}

// sumHeights totals the block-axis extent of a glyph slice.
func sumHeights(vgs []verticalGlyph) float64 {
	total := 0.0
	for i := range vgs {
		total += verticalGlyphHeight(&vgs[i])
	}
	return total
}

// buildVerticalColumn turns one chunk into a positioned Column. Glyph X/Y
// coordinates are column-local until positionVerticalColumns translates
// them:
//
//   - local X is relative to the column center line (positive = right).
//   - local Y is relative to the column top (positive = downward).
//
// For BTT the block axis is flipped: the first logical glyph is placed at
// the column bottom and subsequent glyphs grow upward.
func buildVerticalColumn(chunk []verticalGlyph, runs []ShapedRun, metrics Metrics, dir Direction) Column {
	col := Column{
		Runs:    make([]ShapedRun, 0),
		Glyphs:  make([]ShapedGlyph, 0, len(chunk)),
		Ascent:  metrics.Ascent,
		Descent: metrics.Descent,
	}

	halfWidth := columnHalfExtent(metrics)
	penY := 0.0
	for i := 0; i < len(chunk); {
		vg := &chunk[i]

		if vg.orient == VerticalOrientSideways {
			next := placeSidewaysRun(chunk, i, &col, penY)
			penY = next
			i = skipSidewaysRun(chunk, i)
			continue
		}

		g := vg.glyph
		// Upright: the glyph's vertical origin lands on (center line, pen).
		// originX is the offset from the horizontal glyph origin to the
		// vertical origin; originY moves the baseline to the pen position.
		g.X = vg.originX
		g.Y = penY + vg.originY
		g.YAdvance = vg.vAdvance
		g.VerticalOrient = VerticalOrientUpright
		col.Glyphs = append(col.Glyphs, g)
		penY += vg.vAdvance
		i++
	}

	col.Height = penY
	if dir == DirectionBTT {
		flipColumnBottomToTop(&col)
	}
	col.Ascent = halfWidth
	col.Descent = halfWidth
	col.Width = halfWidth * 2
	rebuildVerticalRuns(&col, chunk, runs)
	return col
}

// flipColumnBottomToTop mirrors local Y coordinates so the column reads
// bottom-to-top: glyph 0 ends up at the largest Y (bottom of the column).
func flipColumnBottomToTop(col *Column) {
	height := col.Height
	for i := range col.Glyphs {
		col.Glyphs[i].Y = height - col.Glyphs[i].Y
	}
	for i := range col.Runs {
		for j := range col.Runs[i].Glyphs {
			col.Runs[i].Glyphs[j].Y = height - col.Runs[i].Glyphs[j].Y
		}
	}
}

// columnHalfExtent is the distance from the center line to each edge.
// Upright em boxes need half the line height; sideways Latin runs need the
// full font ascent/descent. Take the larger so neither overflows.
func columnHalfExtent(metrics Metrics) float64 {
	emHalf := (metrics.Ascent + metrics.Descent - metrics.LineGap) / 2
	if metrics.Ascent > emHalf {
		emHalf = metrics.Ascent
	}
	if metrics.Descent > emHalf {
		emHalf = metrics.Descent
	}
	return emHalf
}

// placeSidewaysRun positions a maximal contiguous sideways run starting at
// chunk[start] and returns the updated block-axis pen.
//
// The run is rendered rotated 90° clockwise about the column center line.
// Pre-rotation coordinates are retained for the renderer:
//
//   - glyph Y stores the block-axis position of the horizontal baseline
//     (pen start + the shaped horizontal X).
//   - glyph X stores the cross-axis offset from the center line (the
//     shaped Y offsets, typically zero for plain horizontal shaping).
func placeSidewaysRun(chunk []verticalGlyph, start int, col *Column, penY float64) float64 {
	end := skipSidewaysRun(chunk, start)

	var runWidth float64
	for k := start; k < end; k++ {
		g := chunk[k].glyph
		g.VerticalOrient = VerticalOrientSideways
		// Pre-rotation coordinates for the renderer:
		// glyph X stores the cross-axis offset from the center line (the
		// shaped Y offsets, typically zero for plain horizontal shaping).
		// glyph Y stores the block-axis position of the horizontal
		// baseline (pen start + the shaped horizontal X).
		g.X = g.Y
		g.Y = penY + chunk[k].glyph.X
		// Block-axis advance mirrors the horizontal advance once rotated.
		g.YAdvance = g.XAdvance
		col.Glyphs = append(col.Glyphs, g)

		if edge := chunk[k].glyph.X + chunk[k].glyph.XAdvance; edge > runWidth {
			runWidth = edge
		}
	}
	return penY + runWidth
}

// skipSidewaysRun returns the index just past the contiguous sideways run
// beginning at start.
func skipSidewaysRun(chunk []verticalGlyph, start int) int {
	end := start
	for end < len(chunk) && chunk[end].orient == VerticalOrientSideways {
		end++
	}
	return end
}

// rebuildVerticalRuns groups the column's glyphs back into ShapedRuns in
// source-run order, preserving each run's face, size, direction and metrics.
// chunk and col.Glyphs are emitted in the same order, one glyph per entry,
// so source run indices recover by walking chunk in lockstep.
func rebuildVerticalRuns(col *Column, chunk []verticalGlyph, runs []ShapedRun) {
	if len(chunk) == 0 {
		return
	}

	runGlyphs := map[int][]ShapedGlyph{}
	order := make([]int, 0)

	for i := range chunk {
		idx := chunk[i].runIdx
		if _, ok := runGlyphs[idx]; !ok {
			order = append(order, idx)
			runGlyphs[idx] = make([]ShapedGlyph, 0)
		}
		runGlyphs[idx] = append(runGlyphs[idx], col.Glyphs[i])
	}

	for _, idx := range order {
		src := &runs[idx]
		glyphs := runGlyphs[idx]
		col.Runs = append(col.Runs, ShapedRun{
			Glyphs:    glyphs,
			Advance:   verticalRunAdvance(glyphs),
			Ascent:    src.Ascent,
			Descent:   src.Descent,
			Direction: src.Direction,
			Face:      src.Face,
			Size:      src.Size,
		})
	}
}

// verticalRunAdvance is the block-axis extent of a run within a column.
func verticalRunAdvance(glyphs []ShapedGlyph) float64 {
	if len(glyphs) == 0 {
		return 0
	}
	first := &glyphs[0]
	last := &glyphs[len(glyphs)-1]
	if last.VerticalOrient == VerticalOrientSideways {
		return last.Y + last.XAdvance - first.Y
	}
	return last.Y + last.YAdvance - first.Y
}
