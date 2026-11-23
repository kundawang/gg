// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"image"
	"image/color"
	"math"

	"github.com/gogpu/gg/internal/raster"
)

// ImageSurface is a CPU-based surface that renders to an *image.RGBA.
//
// It uses raster.AnalyticFiller for high-quality anti-aliased rendering.
// This is the default surface implementation for software rendering.
//
// Example:
//
//	s := surface.NewImageSurface(800, 600)
//	defer s.Close()
//
//	s.Clear(color.White)
//	path := surface.NewPath()
//	path.Circle(400, 300, 100)
//	s.Fill(path, surface.FillStyle{Color: color.RGBA{255, 0, 0, 255}})
//
//	img := s.Snapshot()
type ImageSurface struct {
	width  int
	height int
	img    *image.RGBA

	// filler provides analytic anti-aliasing
	filler *raster.AnalyticFiller

	// edgeBuilder converts paths to edges
	edgeBuilder *raster.EdgeBuilder

	// state is the current blend mode and opacity.
	state surfaceState

	// stateStack holds the states pushed by Save.
	stateStack []surfaceState

	// lastErr records the most recent rejected state operation.
	lastErr error

	// closed tracks if Close has been called
	closed bool
}

// NewImageSurface creates a new CPU-based surface with the given dimensions.
func NewImageSurface(width, height int) *ImageSurface {
	if width <= 0 {
		width = 1
	}
	if height <= 0 {
		height = 1
	}

	return &ImageSurface{
		width:       width,
		height:      height,
		img:         image.NewRGBA(image.Rect(0, 0, width, height)),
		filler:      raster.NewAnalyticFiller(width, height),
		edgeBuilder: raster.NewEdgeBuilder(2), // 4x AA quality
		state:       defaultState(),
		stateStack:  make([]surfaceState, 0, 4),
	}
}

// NewImageSurfaceFromImage creates a surface backed by an existing image.
// The surface will render into the provided image directly.
func NewImageSurfaceFromImage(img *image.RGBA) *ImageSurface {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	return &ImageSurface{
		width:       width,
		height:      height,
		img:         img,
		filler:      raster.NewAnalyticFiller(width, height),
		edgeBuilder: raster.NewEdgeBuilder(2),
		state:       defaultState(),
		stateStack:  make([]surfaceState, 0, 4),
	}
}

// Width returns the surface width.
func (s *ImageSurface) Width() int {
	return s.width
}

// Height returns the surface height.
func (s *ImageSurface) Height() int {
	return s.height
}

// Clear fills the entire surface with the given color, replacing prior
// contents rather than compositing over them.
//
// Clear resets the surface, so it ignores the current blend mode and opacity;
// [Surface.Clear] documents it as the way to reset a surface. A color with
// alpha 0 erases the surface to transparent black.
func (s *ImageSurface) Clear(c color.Color) {
	if s.closed || s.img == nil {
		return
	}

	src := premulBytes(c)
	if src.A == 0 {
		// A fully transparent (or nil) clear erases all previous content.
		for y := 0; y < s.height; y++ {
			row := s.img.Pix[y*s.img.Stride : y*s.img.Stride+s.width*4]
			clear(row)
		}
		return
	}
	for y := 0; y < s.height; y++ {
		row := s.img.Pix[y*s.img.Stride : y*s.img.Stride+s.width*4]
		for i := 0; i < len(row); i += 4 {
			row[i+0] = src.R
			row[i+1] = src.G
			row[i+2] = src.B
			row[i+3] = src.A
		}
	}
}

// Fill fills the given path using the specified style.
//
// The path is rasterized with the surface's current blend mode and opacity.
// Paths with non-finite coordinates are ignored, as are empty paths.
func (s *ImageSurface) Fill(path *Path, style FillStyle) {
	if s.closed || s.img == nil || path == nil || path.IsEmpty() {
		return
	}

	// Get fill color
	opacity := opacityByte(s.state.opacity)
	if opacity == 0 {
		return
	}
	fillColor := scalePremul(s.resolveColor(style.Color, style.Pattern), opacity)

	safePath, ok := clampPath(path)
	if !ok {
		return
	}

	// Convert fill rule
	var fillRule raster.FillRule
	switch style.Rule {
	case FillRuleEvenOdd:
		fillRule = raster.FillRuleEvenOdd
	default:
		fillRule = raster.FillRuleNonZero
	}

	// Build edges from path
	s.edgeBuilder.Reset()
	s.edgeBuilder.SetFlattenCurves(false)
	s.applyClipBounds()
	s.edgeBuilder.BuildFromPath(safePath, raster.IdentityTransform{})

	if s.edgeBuilder.IsEmpty() {
		return
	}

	mode := s.state.blendMode

	// Reset filler and render
	s.filler.Reset()
	s.filler.Fill(s.edgeBuilder, fillRule, func(y int, runs *raster.AlphaRuns) {
		if y < 0 || y >= s.height {
			return
		}
		s.blendRow(y, runs, fillColor, mode)
	})
}

// Stroke strokes the given path using the specified style.
//
// The stroke is rasterized with the surface's current blend mode and opacity.
func (s *ImageSurface) Stroke(path *Path, style StrokeStyle) {
	if s.closed || s.img == nil || path == nil || path.IsEmpty() {
		return
	}

	opacity := opacityByte(s.state.opacity)
	if opacity == 0 {
		return
	}

	safePath, ok := clampPath(path)
	if !ok {
		return
	}

	// Get stroke color, with surface opacity folded into the premultiplied
	// value so the per-pixel path only applies coverage.
	strokeColor := scalePremul(s.resolveColor(style.Color, style.Pattern), opacity)

	// Create stroke path by expanding the original path
	strokePath := s.expandStroke(safePath, style)
	if strokePath == nil || strokePath.IsEmpty() {
		return
	}

	safeStroke, ok := clampPath(strokePath)
	if !ok {
		return
	}

	// Build edges from expanded stroke path
	s.edgeBuilder.Reset()
	s.edgeBuilder.SetFlattenCurves(false)
	s.applyClipBounds()
	s.edgeBuilder.BuildFromPath(safeStroke, raster.IdentityTransform{})

	if s.edgeBuilder.IsEmpty() {
		return
	}

	mode := s.state.blendMode

	// Render as filled shape (stroke expansion creates filled outline)
	s.filler.Reset()
	s.filler.Fill(s.edgeBuilder, raster.FillRuleNonZero, func(y int, runs *raster.AlphaRuns) {
		if y < 0 || y >= s.height {
			return
		}
		s.blendRow(y, runs, strokeColor, mode)
	})
}

// DrawImage draws an image at the specified position.
//
// The image is composited with the surface's current blend mode and opacity.
// opts.Alpha is sanitized (clamped to [0, 1], NaN treated as 1), and the
// intersection with the surface bounds is clipped, so an image placed partly
// or entirely outside the surface is drawn without affecting unrelated pixels.
func (s *ImageSurface) DrawImage(img image.Image, at Point, opts *DrawImageOptions) {
	if s.closed || s.img == nil || img == nil {
		return
	}

	srcBounds := img.Bounds()
	if opts != nil && opts.SrcRect != nil {
		srcBounds = srcBounds.Intersect(*opts.SrcRect)
	}
	if srcBounds.Empty() {
		return
	}

	if math.IsNaN(at.X) || math.IsNaN(at.Y) || math.IsInf(at.X, 0) || math.IsInf(at.Y, 0) {
		return
	}

	alpha := 1.0
	if opts != nil {
		alpha = sanitizeOpacity(opts.Alpha)
	}
	opacity := opacityByte(s.state.opacity * alpha)
	if opacity == 0 {
		return
	}

	// Clamp the destination origin into a range where float64->int conversion is
	// well defined. Anything beyond this cannot overlap a surface whose
	// dimensions fit in an int, so the clipping below discards it anyway.
	const originLimit = float64(1 << 40)
	if at.X <= -originLimit || at.X >= originLimit || at.Y <= -originLimit || at.Y >= originLimit {
		return
	}
	dstX, dstY := int(at.X), int(at.Y)

	// Clamp the whole destination rectangle to the surface once, instead of
	// testing every source pixel. Widen before adding so a huge image cannot
	// overflow the rect corners.
	w := int64(srcBounds.Dx())
	h := int64(srcBounds.Dy())
	x0 := max(int64(dstX), 0)
	y0 := max(int64(dstY), 0)
	x1 := min(int64(dstX)+w, int64(s.width))
	y1 := min(int64(dstY)+h, int64(s.height))
	if x0 >= x1 || y0 >= y1 {
		return
	}

	s.drawImageRegion(img, srcBounds, dstX, dstY, int(x0), int(y0), int(x1), int(y1), opacity, s.state.blendMode)
}

// drawImageRegion blits the clipped destination rectangle [x0,y0)-(x1,y1) of
// img. srcBounds and the destination origin map surface pixels to source
// pixels; the caller has already validated and clipped both.
func (s *ImageSurface) drawImageRegion(
	img image.Image, srcBounds image.Rectangle,
	dstX, dstY, x0, y0, x1, y1 int,
	opacity uint32, mode BlendMode,
) {
	for dy := y0; dy < y1; dy++ {
		sy := srcBounds.Min.Y + (dy - dstY)
		if sy < srcBounds.Min.Y || sy >= srcBounds.Max.Y {
			continue
		}
		for dx := x0; dx < x1; dx++ {
			sx := srcBounds.Min.X + (dx - dstX)
			if sx < srcBounds.Min.X || sx >= srcBounds.Max.X {
				continue
			}
			src := scalePremul(premulBytes(img.At(sx, sy)), opacity)
			s.blendPremulPixel(dx, dy, src, 255, mode)
		}
	}
}

// Flush ensures all pending operations are complete.
// For ImageSurface, this is a no-op.
func (s *ImageSurface) Flush() error {
	return nil
}

// Snapshot returns a copy of the current surface contents.
func (s *ImageSurface) Snapshot() *image.RGBA {
	if s.closed {
		return nil
	}

	result := image.NewRGBA(image.Rect(0, 0, s.width, s.height))
	rowBytes := s.width * 4
	for y := 0; y < s.height; y++ {
		src := s.img.Pix[y*s.img.Stride : y*s.img.Stride+rowBytes]
		dst := result.Pix[y*result.Stride : y*result.Stride+rowBytes]
		copy(dst, src)
	}
	return result
}

// Close releases resources associated with the surface.
func (s *ImageSurface) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.img = nil
	s.filler = nil
	s.edgeBuilder = nil
	return nil
}

// Image returns the underlying image.RGBA.
// This is a direct reference, not a copy.
func (s *ImageSurface) Image() *image.RGBA {
	return s.img
}

// Capabilities returns the surface capabilities.
func (s *ImageSurface) Capabilities() Capabilities {
	return Capabilities{
		SupportsSubSurface: false,
		SupportsResize:     false,
		SupportsClipping:   false,
		SupportsBlendModes: true,
		SupportsAntialias:  true,
		MaxWidth:           0, // Unlimited
		MaxHeight:          0,
	}
}

// resolveColor extracts color from Color or Pattern.
// Note: Pattern support is planned for future; currently uses color only.
func (s *ImageSurface) resolveColor(c color.Color, _ Pattern) premulRGBA {
	if c != nil {
		return premulBytes(c)
	}
	// Pattern would be sampled per-pixel, but for now use black
	return premulRGBA{A: 255}
}

// blendRow blends alpha runs onto a scanline.
//
// c is the premultiplied fill color with opacity already folded in. Coverage is
// passed to blendPremulPixel, which applies it per mode: Porter-Duff results
// are interpolated with the destination, while separable modes scale the
// source alpha before blending.
func (s *ImageSurface) blendRow(y int, runs *raster.AlphaRuns, c premulRGBA, mode BlendMode) {
	for x, alpha := range runs.Iter() {
		if x < 0 || x >= s.width {
			continue
		}
		if alpha == 0 {
			continue
		}
		s.blendPremulPixel(x, y, c, alpha, mode)
	}
}

// blendPremulPixel composites a premultiplied source color with per-pixel
// coverage onto the image.
func (s *ImageSurface) blendPremulPixel(x, y int, src premulRGBA, coverage uint8, mode BlendMode) {
	if coverage == 0 {
		return
	}

	idx := s.pixOffset(x, y)
	dst := premulRGBA{
		R: s.img.Pix[idx+0],
		G: s.img.Pix[idx+1],
		B: s.img.Pix[idx+2],
		A: s.img.Pix[idx+3],
	}

	out := blendPremul(mode, src, dst, coverage)
	s.img.Pix[idx+0] = out.R
	s.img.Pix[idx+1] = out.G
	s.img.Pix[idx+2] = out.B
	s.img.Pix[idx+3] = out.A
}

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

// Verify ImageSurface implements Surface interface.
var _ Surface = (*ImageSurface)(nil)
var _ CapableSurface = (*ImageSurface)(nil)
var _ BlendableSurface = (*ImageSurface)(nil)
var _ StatefulSurface = (*ImageSurface)(nil)
