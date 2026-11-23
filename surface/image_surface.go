// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"image"
	"image/color"
	"image/draw"
	"sync"
	"unsafe"

	"github.com/gogpu/gg/internal/raster"
)

// ImageSurface is a CPU-based surface that renders to an *image.RGBA.
//
// It uses raster.AnalyticFiller for high-quality anti-aliased rendering.
// This is the default surface implementation for software rendering.
//
// All pixel addressing goes through an internal pixelView, which handles
// image.Rectangle bounds (including non-zero Min origins), per-row stride,
// clipping, overflow checks and source-over alpha compositing in one place.
//
// Although Surface is documented as single-goroutine, every method is
// serialized by an internal mutex so concurrent calls cannot corrupt the
// backing storage or race on the rasterizer state.
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

	// view is the single authority for pixel addressing. It may be invalid
	// when the surface was constructed from a nil/empty image or with a
	// stride that cannot address the declared bounds; in that state every
	// drawing operation is a safe no-op and initErr is surfaced by Flush.
	view    *pixelView
	initErr error

	// filler provides analytic anti-aliasing
	filler *raster.AnalyticFiller

	// edgeBuilder converts paths to edges
	edgeBuilder *raster.EdgeBuilder

	// closed tracks if Close has been called
	closed bool

	// mu serializes all public operations, including concurrent callers.
	mu sync.Mutex

	// scratch holds reusable buffers so DrawImage alias handling and
	// Snapshot do not allocate on every call once warm.
	scratch struct {
		alias []uint8
	}
}

// NewImageSurface creates a new CPU-based surface with the given dimensions.
// Non-positive dimensions are clamped to 1, matching the documented behavior.
func NewImageSurface(width, height int) *ImageSurface {
	if width <= 0 {
		width = 1
	}
	if height <= 0 {
		height = 1
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	s := &ImageSurface{
		width:       width,
		height:      height,
		img:         img,
		filler:      raster.NewAnalyticFiller(width, height),
		edgeBuilder: raster.NewEdgeBuilder(2), // 4x AA quality
	}
	s.view, s.initErr = newPixelView(img)
	return s
}

// NewImageSurfaceFromImage creates a surface backed by an existing image.
// The surface renders into the provided image directly. The image may have a
// non-zero Min origin or a padded stride; all addressing is origin/stride
// aware. If the image is nil, empty, has an invalid stride, or its Pix cannot
// hold the declared bounds, the returned surface performs no writes and the
// construction error is surfaced through Flush.
func NewImageSurfaceFromImage(img *image.RGBA) *ImageSurface {
	s := &ImageSurface{}
	view, err := newPixelView(img)
	s.view = view
	s.initErr = err
	if err != nil {
		// Keep width/height zero and rasterizers nil; all operations no-op.
		return s
	}
	bounds := view.Bounds()
	s.width = bounds.Dx()
	s.height = bounds.Dy()
	s.img = img
	s.filler = raster.NewAnalyticFiller(s.width, s.height)
	s.edgeBuilder = raster.NewEdgeBuilder(2)
	return s
}

// Width returns the surface width.
func (s *ImageSurface) Width() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.width
}

// Height returns the surface height.
func (s *ImageSurface) Height() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.height
}

// Clear fills the entire surface with the given color.
func (s *ImageSurface) Clear(c color.Color) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.view.Valid() {
		return
	}

	r, g, b, a := c.RGBA()
	//nolint:gosec // G115: safe - r>>8 is always in [0, 255]
	rgba := color.RGBA{
		R: uint8(r >> 8),
		G: uint8(g >> 8),
		B: uint8(b >> 8),
		A: uint8(a >> 8),
	}

	// Stride-aware solid fill through the pixel view: identical bytes to
	// the previous draw.Draw call, but works with padded strides and
	// non-zero origins without relying on draw.Draw's offset handling.
	s.view.fillSolid(s.view.Bounds(), rgba)
}

// Fill fills the given path using the specified style.
func (s *ImageSurface) Fill(path *Path, style FillStyle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.view.Valid() || path == nil || path.IsEmpty() {
		return
	}

	// Get fill color
	fillColor := s.resolveColor(style.Color, style.Pattern)

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
	s.edgeBuilder.BuildFromPath(path, raster.IdentityTransform{})

	if s.edgeBuilder.IsEmpty() {
		return
	}

	// Reset filler and render. All clipping and offset math lives in the
	// pixel view, so the callback no longer reaches into s.img directly.
	s.filler.Reset()
	s.filler.Fill(s.edgeBuilder, fillRule, func(y int, runs *raster.AlphaRuns) {
		s.blendRow(y, runs, fillColor)
	})
}

// Stroke strokes the given path using the specified style.
func (s *ImageSurface) Stroke(path *Path, style StrokeStyle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.view.Valid() || path == nil || path.IsEmpty() {
		return
	}

	// Get stroke color
	strokeColor := s.resolveColor(style.Color, style.Pattern)

	// Create stroke path by expanding the original path
	strokePath := s.expandStroke(path, style)
	if strokePath == nil || strokePath.IsEmpty() {
		return
	}

	// Build edges from expanded stroke path
	s.edgeBuilder.Reset()
	s.edgeBuilder.SetFlattenCurves(false)
	s.edgeBuilder.BuildFromPath(strokePath, raster.IdentityTransform{})

	if s.edgeBuilder.IsEmpty() {
		return
	}

	// Render as filled shape (stroke expansion creates filled outline)
	s.filler.Reset()
	s.filler.Fill(s.edgeBuilder, raster.FillRuleNonZero, func(y int, runs *raster.AlphaRuns) {
		s.blendRow(y, runs, strokeColor)
	})
}

// DrawImage draws an image at the specified position.
func (s *ImageSurface) DrawImage(img image.Image, at Point, opts *DrawImageOptions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.view.Valid() || img == nil {
		return
	}

	srcBounds := img.Bounds()
	if opts != nil && opts.SrcRect != nil {
		srcBounds = *opts.SrcRect
	}
	// Empty or inverted source rectangles iterate to zero pixels; keep the
	// same behavior explicitly rather than relying on loop mechanics.
	if srcBounds.Empty() {
		return
	}

	dstX := int(at.X)
	dstY := int(at.Y)

	alpha := 1.0
	if opts != nil {
		alpha = opts.Alpha
	}

	// Alias protection: if the source shares the destination's backing
	// storage, rendering in place could read pixels already overwritten.
	// Snapshot the needed source rows into a reused scratch buffer first.
	source := img
	if s.backingOverlaps(img) {
		copied := s.copySourceToScratch(img, srcBounds)
		if copied == nil {
			return
		}
		source = copied
		srcBounds = copied.Bounds()
	}

	// Nearest-neighbor blit in surface-local coordinates. Every
	// destination write goes through the pixel view, which translates to
	// the backing rectangle, clips out-of-range points and handles padded
	// strides.
	for sy := srcBounds.Min.Y; sy < srcBounds.Max.Y; sy++ {
		dy := dstY + (sy - srcBounds.Min.Y)
		if dy < 0 || dy >= s.height {
			continue
		}

		for sx := srcBounds.Min.X; sx < srcBounds.Max.X; sx++ {
			dx := dstX + (sx - srcBounds.Min.X)
			if dx < 0 || dx >= s.width {
				continue
			}

			srcColor := s.sampleSource(source, sx, sy, alpha)
			s.view.blendRGBA(dx, dy, srcColor)
		}
	}
}

// Flush ensures all pending operations are complete.
// For ImageSurface, this is a no-op apart from surfacing construction errors
// (nil/empty backing image, invalid stride or overflow-checked addressing).
func (s *ImageSurface) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.initErr
}

// Snapshot returns a copy of the current surface contents as a tightly packed
// RGBA image with a zero-origin rectangle. The returned slice is owned by the
// snapshot; it never aliases the surface backing storage.
func (s *ImageSurface) Snapshot() *image.RGBA {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.view.Valid() {
		return nil
	}

	result := image.NewRGBA(image.Rect(0, 0, s.width, s.height))
	s.view.snapshotInto(result)
	return result
}

// Close releases resources associated with the surface.
func (s *ImageSurface) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.img = nil
	s.view = nil
	s.filler = nil
	s.edgeBuilder = nil
	s.scratch.alias = nil
	return nil
}

// Image returns the underlying image.RGBA.
// This is a direct reference, not a copy: callers must not mutate the returned
// image concurrently with surface drawing. Snapshot should be preferred when
// an independent copy is required. Returns nil after Close or when the
// surface was constructed from an invalid backing image.
func (s *ImageSurface) Image() *image.RGBA {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.img
}

// Capabilities returns the surface capabilities.
func (s *ImageSurface) Capabilities() Capabilities {
	return Capabilities{
		SupportsSubSurface: false,
		SupportsResize:     false,
		SupportsClipping:   false,
		SupportsBlendModes: false,
		SupportsAntialias:  true,
		MaxWidth:           0, // Unlimited
		MaxHeight:          0,
	}
}

// resolveColor extracts color from Color or Pattern.
// Note: Pattern support is planned for future; currently uses color only.
func (s *ImageSurface) resolveColor(c color.Color, _ Pattern) color.RGBA {
	if c != nil {
		r, g, b, a := c.RGBA()
		//nolint:gosec // G115: safe - r>>8 is always in [0, 255]
		return color.RGBA{
			R: uint8(r >> 8),
			G: uint8(g >> 8),
			B: uint8(b >> 8),
			A: uint8(a >> 8),
		}
	}
	// Pattern would be sampled per-pixel, but for now use black
	return color.RGBA{0, 0, 0, 255}
}

// blendRow blends alpha runs onto a scanline. The pixel view performs all
// bounds checks, so rows and pixels outside the surface rectangle are
// clipped uniformly for both Fill and Stroke.
func (s *ImageSurface) blendRow(y int, runs *raster.AlphaRuns, c color.RGBA) {
	for x, alpha := range runs.Iter() {
		s.view.blendCoverage(x, y, c, alpha)
	}
}

// backingOverlaps reports whether img's pixel storage overlaps the surface
// backing storage. Only concrete RGBA images can share the Pix slice; any
// other image.Image type is considered independent.
func (s *ImageSurface) backingOverlaps(img image.Image) bool {
	src, ok := img.(*image.RGBA)
	if !ok || !s.view.Valid() {
		return false
	}
	if len(src.Pix) == 0 || len(s.view.pix) == 0 {
		return false
	}
	return rangesOverlap(
		unsafe.Pointer(&src.Pix[0]), len(src.Pix),
		unsafe.Pointer(&s.view.pix[0]), len(s.view.pix),
	)
}

// rangesOverlap reports whether two byte ranges intersect.
func rangesOverlap(p1 unsafe.Pointer, n1 int, p2 unsafe.Pointer, n2 int) bool {
	a0 := uintptr(p1)
	a1 := a0 + uintptr(n1)
	b0 := uintptr(p2)
	b1 := b0 + uintptr(n2)
	return a0 < b1 && b0 < a1
}

// copySourceToScratch copies the source rectangle of img into a tightly
// packed zero-origin *image.RGBA held in the surface's reusable scratch
// storage, so subsequent reads cannot observe in-place overwrites.
func (s *ImageSurface) copySourceToScratch(img image.Image, srcBounds image.Rectangle) *image.RGBA {
	w := srcBounds.Dx()
	h := srcBounds.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	pixels, ok1 := mulCheck(w, h)
	need, ok2 := mulCheck(pixels, bytesPerPixel)
	if !ok1 || !ok2 || need <= 0 {
		return nil
	}
	if cap(s.scratch.alias) < need {
		s.scratch.alias = make([]uint8, need)
	} else {
		s.scratch.alias = s.scratch.alias[:need]
		for i := range s.scratch.alias {
			s.scratch.alias[i] = 0
		}
	}
	cp := &image.RGBA{
		Pix:    s.scratch.alias,
		Stride: bytesPerPixel * w,
		Rect:   image.Rect(0, 0, w, h),
	}
	draw.Draw(cp, cp.Bounds(), img, srcBounds.Min, draw.Src)
	return cp
}

// sampleSource reads one source pixel and applies the global alpha factor,
// returning premultiplied 8-bit RGBA. Fast paths for *image.RGBA and
// *image.NRGBA avoid per-pixel color-interface allocations; the generic path
// preserves the exact pre-refactor behavior (including RGBA64 alpha scaling).
func (s *ImageSurface) sampleSource(src image.Image, x, y int, alpha float64) color.RGBA {
	switch typed := src.(type) {
	case *image.RGBA:
		i := typed.PixOffset(x, y)
		c := color.RGBA{
			R: typed.Pix[i+0],
			G: typed.Pix[i+1],
			B: typed.Pix[i+2],
			A: typed.Pix[i+3],
		}
		if alpha < 1.0 {
			c.A = scaleAlpha8(c.A, alpha)
		}
		return c
	case *image.NRGBA:
		i := typed.PixOffset(x, y)
		n := color.NRGBA{
			R: typed.Pix[i+0],
			G: typed.Pix[i+1],
			B: typed.Pix[i+2],
			A: typed.Pix[i+3],
		}
		if alpha < 1.0 {
			n.A = scaleAlpha8(n.A, alpha)
		}
		r, g, b, a := n.RGBA()
		//nolint:gosec // G115: safe - r>>8 is always in [0, 255]
		return color.RGBA{
			R: uint8(r >> 8),
			G: uint8(g >> 8),
			B: uint8(b >> 8),
			A: uint8(a >> 8),
		}
	default:
		srcColor := src.At(x, y)
		if alpha < 1.0 {
			srcColor = s.applyAlpha(srcColor, alpha)
		}
		r, g, b, a := srcColor.RGBA()
		//nolint:gosec // G115: safe - r>>8 is always in [0, 255]
		return color.RGBA{
			R: uint8(r >> 8),
			G: uint8(g >> 8),
			B: uint8(b >> 8),
			A: uint8(a >> 8),
		}
	}
}

// scaleAlpha8 reproduces the pre-refactor alpha scaling exactly. The old
// implementation received a 16-bit alpha (a8<<8 for *image.RGBA sources),
// computed uint16(float64(a16) * alpha), and the caller took the high byte
// via >>8. This helper works in the expanded 16-bit domain so results are
// bit-identical, including float truncation.
func scaleAlpha8(a8 uint8, alpha float64) uint8 {
	a16 := uint16(float64(uint16(a8)<<8) * alpha)
	//nolint:gosec // G115: safe - a16 >> 8 is in [0, 255]
	return uint8(a16 >> 8)
}

// applyAlpha multiplies a color's alpha by the given factor.
func (s *ImageSurface) applyAlpha(c color.Color, alpha float64) color.Color {
	r, g, b, a := c.RGBA()
	newA := uint16(float64(a) * alpha)
	//nolint:gosec // G115: safe - r,g,b are uint32 from RGBA() which fits uint16
	return color.RGBA64{
		R: uint16(r),
		G: uint16(g),
		B: uint16(b),
		A: newA,
	}
}

// Verify ImageSurface implements Surface interface.
var _ Surface = (*ImageSurface)(nil)
var _ CapableSurface = (*ImageSurface)(nil)
