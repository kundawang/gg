// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"github.com/gogpu/gg/internal/raster"
)

// expandStroke creates a filled path representing the stroke outline.
// This is a simplified implementation using parallel offset curves.
func (s *ImageSurface) expandStroke(path *Path, style StrokeStyle) *Path {
	if style.Width <= 0 {
		return nil
	}

	halfWidth := style.Width / 2.0
	result := NewPath()

	// Simple stroke expansion: create parallel paths
	// This is a basic implementation; production code would handle
	// caps, joins, and miters properly.

	var segments []strokeSegment
	var curX, curY, startX, startY float32

	pointIdx := 0
	for _, verb := range path.verbs {
		switch verb {
		case raster.MoveTo:
			startX = path.points[pointIdx]
			startY = path.points[pointIdx+1]
			curX, curY = startX, startY
			pointIdx += 2

		case raster.LineTo:
			x, y := path.points[pointIdx], path.points[pointIdx+1]
			segments = append(segments, strokeSegment{
				x0: curX, y0: curY,
				x1: x, y1: y,
			})
			curX, curY = x, y
			pointIdx += 2

		case raster.QuadTo:
			// Flatten quad to lines for stroke
			cx, cy := path.points[pointIdx], path.points[pointIdx+1]
			x, y := path.points[pointIdx+2], path.points[pointIdx+3]
			s.flattenQuadForStroke(curX, curY, cx, cy, x, y, &segments)
			curX, curY = x, y
			pointIdx += 4

		case raster.CubicTo:
			// Flatten cubic to lines for stroke
			c1x, c1y := path.points[pointIdx], path.points[pointIdx+1]
			c2x, c2y := path.points[pointIdx+2], path.points[pointIdx+3]
			x, y := path.points[pointIdx+4], path.points[pointIdx+5]
			s.flattenCubicForStroke(curX, curY, c1x, c1y, c2x, c2y, x, y, &segments)
			curX, curY = x, y
			pointIdx += 6

		case raster.Close:
			if curX != startX || curY != startY {
				segments = append(segments, strokeSegment{
					x0: curX, y0: curY,
					x1: startX, y1: startY,
				})
			}
			curX, curY = startX, startY
		}
	}

	if len(segments) == 0 {
		return nil
	}

	// Create expanded outline
	s.buildStrokeOutline(result, segments, halfWidth, style)
	return result
}

// strokeSegment represents a line segment for stroke expansion.
type strokeSegment struct {
	x0, y0, x1, y1 float32
}

// flattenQuadForStroke flattens a quadratic curve to line segments.
func (s *ImageSurface) flattenQuadForStroke(x0, y0, cx, cy, x1, y1 float32, segments *[]strokeSegment) {
	const tolerance = 0.5
	s.flattenQuadRecursive(x0, y0, cx, cy, x1, y1, tolerance, 0, segments)
}

func (s *ImageSurface) flattenQuadRecursive(x0, y0, cx, cy, x1, y1, tol float32, depth int, segments *[]strokeSegment) {
	if depth > 10 {
		*segments = append(*segments, strokeSegment{x0, y0, x1, y1})
		return
	}

	dx := x1 - x0
	dy := y1 - y0
	dcx := cx - x0
	dcy := cy - y0
	cross := dcx*dy - dcy*dx
	lenSq := dx*dx + dy*dy

	if lenSq < 1e-6 || cross*cross/lenSq < tol*tol {
		*segments = append(*segments, strokeSegment{x0, y0, x1, y1})
		return
	}

	q0x := (x0 + cx) * 0.5
	q0y := (y0 + cy) * 0.5
	q1x := (cx + x1) * 0.5
	q1y := (cy + y1) * 0.5
	mx := (q0x + q1x) * 0.5
	my := (q0y + q1y) * 0.5

	s.flattenQuadRecursive(x0, y0, q0x, q0y, mx, my, tol, depth+1, segments)
	s.flattenQuadRecursive(mx, my, q1x, q1y, x1, y1, tol, depth+1, segments)
}

// flattenCubicForStroke flattens a cubic curve to line segments.
func (s *ImageSurface) flattenCubicForStroke(x0, y0, c1x, c1y, c2x, c2y, x1, y1 float32, segments *[]strokeSegment) {
	const tolerance = 0.5
	s.flattenCubicRecursive(x0, y0, c1x, c1y, c2x, c2y, x1, y1, tolerance, 0, segments)
}

func (s *ImageSurface) flattenCubicRecursive(x0, y0, c1x, c1y, c2x, c2y, x1, y1, tol float32, depth int, segments *[]strokeSegment) {
	if depth > 10 {
		*segments = append(*segments, strokeSegment{x0, y0, x1, y1})
		return
	}

	dx := x1 - x0
	dy := y1 - y0
	lenSq := dx*dx + dy*dy

	if lenSq < 1e-6 {
		*segments = append(*segments, strokeSegment{x0, y0, x1, y1})
		return
	}

	dc1x := c1x - x0
	dc1y := c1y - y0
	cross1 := dc1x*dy - dc1y*dx

	dc2x := c2x - x0
	dc2y := c2y - y0
	cross2 := dc2x*dy - dc2y*dx

	maxCross := cross1
	if maxCross < 0 {
		maxCross = -maxCross
	}
	if cross2 > maxCross {
		maxCross = cross2
	}
	if -cross2 > maxCross {
		maxCross = -cross2
	}

	if maxCross*maxCross/lenSq < tol*tol {
		*segments = append(*segments, strokeSegment{x0, y0, x1, y1})
		return
	}

	m01x := (x0 + c1x) * 0.5
	m01y := (y0 + c1y) * 0.5
	m12x := (c1x + c2x) * 0.5
	m12y := (c1y + c2y) * 0.5
	m23x := (c2x + x1) * 0.5
	m23y := (c2y + y1) * 0.5
	m012x := (m01x + m12x) * 0.5
	m012y := (m01y + m12y) * 0.5
	m123x := (m12x + m23x) * 0.5
	m123y := (m12y + m23y) * 0.5
	mx := (m012x + m123x) * 0.5
	my := (m012y + m123y) * 0.5

	s.flattenCubicRecursive(x0, y0, m01x, m01y, m012x, m012y, mx, my, tol, depth+1, segments)
	s.flattenCubicRecursive(mx, my, m123x, m123y, m23x, m23y, x1, y1, tol, depth+1, segments)
}

// buildStrokeOutline creates the outline path from line segments.
// Note: style parameter reserved for future cap/join handling.
func (s *ImageSurface) buildStrokeOutline(result *Path, segments []strokeSegment, halfWidth float64, _ StrokeStyle) {
	if len(segments) == 0 {
		return
	}

	hw := float32(halfWidth)

	// Forward pass: create one side of the stroke
	type offsetPoint struct{ x, y float32 }
	forward := make([]offsetPoint, 0, len(segments)+1)
	backward := make([]offsetPoint, 0, len(segments)+1)

	for i, seg := range segments {
		dx := seg.x1 - seg.x0
		dy := seg.y1 - seg.y0
		length := float32(1e-10)
		if dx != 0 || dy != 0 {
			length = float32(sqrtf32(dx*dx + dy*dy))
		}

		// Normal vector
		nx := -dy / length * hw
		ny := dx / length * hw

		if i == 0 {
			// Start cap (butt for now)
			forward = append(forward, offsetPoint{seg.x0 + nx, seg.y0 + ny})
			backward = append(backward, offsetPoint{seg.x0 - nx, seg.y0 - ny})
		}

		// End of segment
		forward = append(forward, offsetPoint{seg.x1 + nx, seg.y1 + ny})
		backward = append(backward, offsetPoint{seg.x1 - nx, seg.y1 - ny})
	}

	// Build the outline path
	if len(forward) > 0 {
		result.MoveTo(float64(forward[0].x), float64(forward[0].y))
		for i := 1; i < len(forward); i++ {
			result.LineTo(float64(forward[i].x), float64(forward[i].y))
		}
	}

	// Connect to backward side
	for i := len(backward) - 1; i >= 0; i-- {
		result.LineTo(float64(backward[i].x), float64(backward[i].y))
	}

	result.Close()
}

// sqrtf32 computes square root for float32.
func sqrtf32(x float32) float32 {
	if x <= 0 {
		return 0
	}
	// Newton-Raphson iteration for sqrt
	r := x
	for i := 0; i < 3; i++ {
		r = 0.5 * (r + x/r)
	}
	return r
}
