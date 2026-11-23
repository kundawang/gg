// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"errors"
	"image"
	"image/color"
	"math"
)

// Standard RGBA layout: 4 bytes per pixel.
const bytesPerPixel = 4

var (
	// errNilImage is returned via ImageSurface.Flush when a surface was
	// constructed from a nil image.
	errNilImage = errors.New("surface: cannot create surface from nil image")
	// errInvalidStride is returned via ImageSurface.Flush when the backing
	// image has a stride that cannot address every pixel of its bounds.
	errInvalidStride = errors.New("surface: backing image has invalid stride")
	// errEmptyImage is returned via ImageSurface.Flush when the backing
	// image has empty bounds.
	errEmptyImage = errors.New("surface: backing image has empty bounds")
	// errPixOverflow is returned via ImageSurface.Flush when addressing the
	// backing image would overflow the integer address space or Pix.
	errPixOverflow = errors.New("surface: backing image pixel addressing overflow")
	// errPixTooSmall is returned via ImageSurface.Flush when Pix cannot hold
	// the declared rectangle at the given stride.
	errPixTooSmall = errors.New("surface: backing image Pix is smaller than bounds require")
)

// pixelView is the single authority for addressing the writable pixels of an
// image.RGBA. It centralizes:
//   - image.Rectangle bounds, including non-zero Min origins,
//   - per-row stride and padded backing storage,
//   - clipping of every read/write to the valid rectangle,
//   - integer-overflow-safe offset arithmetic,
//   - alpha-aware source-over compositing.
//
// A pixelView never exposes the underlying Pix slice; callers interact with
// pixels only through the bounded methods below.
type pixelView struct {
	pix    []uint8
	stride int
	rect   image.Rectangle // inclusive Min, exclusive Max, in surface-local pixel coords

	// base is the offset of rect.Min in pix. All per-pixel offsets are
	// base + checked(row*stride + col*4).
	base int
	// valid is false when the view could not be constructed safely.
	// Every accessor on an invalid view is a safe no-op / zero read.
	valid bool
}

// newPixelView constructs a pixelView for img. The returned error describes
// why the view is invalid, if any; an invalid view performs no writes.
func newPixelView(img *image.RGBA) (*pixelView, error) {
	if img == nil {
		return &pixelView{}, errNilImage
	}
	rect := img.Bounds()
	if rect.Empty() {
		return &pixelView{}, errEmptyImage
	}
	if img.Stride < bytesPerPixel*rect.Dx() {
		return &pixelView{}, errInvalidStride
	}

	base, ok := checkedOffset(img.Stride, rect.Min.X, rect.Min.Y, img.Rect)
	if !ok {
		return &pixelView{}, errPixOverflow
	}

	// Offset of the last addressed byte (last pixel's alpha channel).
	last, ok := checkedOffset(img.Stride, rect.Max.X-1, rect.Max.Y-1, img.Rect)
	if !ok {
		return &pixelView{}, errPixOverflow
	}
	if last+bytesPerPixel > len(img.Pix) {
		return &pixelView{}, errPixTooSmall
	}

	return &pixelView{
		pix:    img.Pix,
		stride: img.Stride,
		rect:   rect,
		base:   base,
		valid:  true,
	}, nil
}

// mulCheck returns a*b and false if the multiplication overflows int.
func mulCheck(a, b int) (int, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	r := a * b
	if r/b != a {
		return 0, false
	}
	return r, true
}

// addCheck returns a+b and false if the addition overflows int.
func addCheck(a, b int) (int, bool) {
	if (b > 0 && a > math.MaxInt-b) || (b < 0 && a < math.MinInt-b) {
		return 0, false
	}
	return a + b, true
}

// checkedOffset computes image.RGBA.PixOffset(x, y) with overflow checks.
// Stride may be negative only in theory; image.RGBA always uses a positive
// stride, which is also enforced by newPixelView.
func checkedOffset(stride, x, y int, rect image.Rectangle) (int, bool) {
	ys, ok1 := mulCheck(stride, y-rect.Min.Y)
	xs, ok2 := mulCheck(bytesPerPixel, x-rect.Min.X)
	off, ok3 := addCheck(ys, xs)
	// Match image.RGBA.PixOffset semantics: PixOffset(Min) == 0.
	return off, ok1 && ok2 && ok3 && off >= 0
}

// Valid reports whether the view addresses a usable backing image.
func (v *pixelView) Valid() bool { return v != nil && v.valid }

// Bounds returns the writable rectangle in surface-local coordinates:
// always starts at (0, 0) with Dx/Dy matching the surface dimensions.
func (v *pixelView) Bounds() image.Rectangle {
	if !v.Valid() {
		return image.Rectangle{}
	}
	return image.Rect(0, 0, v.rect.Dx(), v.rect.Dy())
}

// contains reports whether the surface-local point (x, y) is writable.
// Local coordinates start at (0, 0); the view translates them to the
// backing image rectangle, which may have a non-zero Min origin.
func (v *pixelView) contains(x, y int) bool {
	return v.Valid() &&
		x >= 0 && x < v.rect.Dx() &&
		y >= 0 && y < v.rect.Dy()
}

// offset returns the Pix index of local pixel (x, y), or false if the point
// is outside the view. Overflow safety is provided at construction time:
// newPixelView already proved that the address of the last in-range pixel
// fits in int and in Pix, so plain arithmetic on validated local
// coordinates cannot wrap. Keeping the hot path division-free matters for
// per-pixel blend performance.
func (v *pixelView) offset(x, y int) (int, bool) {
	if !v.contains(x, y) {
		return 0, false
	}
	return v.base + y*v.stride + x*bytesPerPixel, true
}

// blendRGBA blends premultiplied src with full coverage (alpha 255).
// Out-of-range points are clipped silently.
func (v *pixelView) blendRGBA(x, y int, src color.RGBA) {
	v.blendCoverage(x, y, src, 255)
}

// blendCoverage composites src onto the pixel at (x, y) with the given
// coverage alpha. The arithmetic is identical to the pre-refactor
// ImageSurface.blendPixelAlpha, including opaque fast path and rounding.
func (v *pixelView) blendCoverage(x, y int, src color.RGBA, alpha uint8) {
	idx, ok := v.offset(x, y)
	if !ok || alpha == 0 {
		return
	}

	if alpha == 255 && src.A == 255 {
		// Fully opaque - direct write
		v.pix[idx+0] = src.R
		v.pix[idx+1] = src.G
		v.pix[idx+2] = src.B
		v.pix[idx+3] = src.A
		return
	}

	// Source-over compositing with coverage
	// srcA = src.A * alpha / 255
	srcA := uint32(src.A) * uint32(alpha) / 255
	invSrcA := 255 - srcA

	dstR := uint32(v.pix[idx+0])
	dstG := uint32(v.pix[idx+1])
	dstB := uint32(v.pix[idx+2])
	dstA := uint32(v.pix[idx+3])

	outA := srcA + dstA*invSrcA/255
	if outA == 0 {
		return
	}

	outR := (uint32(src.R)*srcA + dstR*dstA*invSrcA/255) / outA
	outG := (uint32(src.G)*srcA + dstG*dstA*invSrcA/255) / outA
	outB := (uint32(src.B)*srcA + dstB*dstA*invSrcA/255) / outA

	//nolint:gosec // G115: safe - values are clamped to [0, 255]
	v.pix[idx+0] = uint8(outR)
	//nolint:gosec // G115: safe
	v.pix[idx+1] = uint8(outG)
	//nolint:gosec // G115: safe
	v.pix[idx+2] = uint8(outB)
	//nolint:gosec // G115: safe
	v.pix[idx+3] = uint8(outA)
}

// fillSolid writes c into every pixel of rect, clipped to the view.
// rect uses surface-local coordinates ((0,0) is the first surface pixel).
// The first row is grown by doubling memmoves and subsequent rows are
// filled with a single row-sized memmove, which approaches bulk-memory
// speed without any allocation, including for padded strides.
func (v *pixelView) fillSolid(rect image.Rectangle, c color.RGBA) {
	if !v.Valid() {
		return
	}
	clipped := rect.Intersect(image.Rect(0, 0, v.rect.Dx(), v.rect.Dy()))
	if clipped.Empty() {
		return
	}

	firstIdx, ok := v.offset(clipped.Min.X, clipped.Min.Y)
	if !ok {
		return
	}
	rowBytes := clipped.Dx() * bytesPerPixel

	// Seed the first pixel, then double the filled region within the row.
	v.pix[firstIdx+0] = c.R
	v.pix[firstIdx+1] = c.G
	v.pix[firstIdx+2] = c.B
	v.pix[firstIdx+3] = c.A
	for filled := bytesPerPixel; filled < rowBytes; filled *= 2 {
		take := filled
		if filled+take > rowBytes {
			take = rowBytes - filled
		}
		copy(v.pix[firstIdx+filled:firstIdx+filled+take],
			v.pix[firstIdx:firstIdx+take])
	}

	// Replicate the completed row; each row occupies a distinct stride
	// block so source and destination never overlap.
	for y := clipped.Min.Y + 1; y < clipped.Max.Y; y++ {
		idx, ok := v.offset(clipped.Min.X, y)
		if !ok {
			continue
		}
		copy(v.pix[idx:idx+rowBytes], v.pix[firstIdx:firstIdx+rowBytes])
	}
}

// snapshotInto copies the view's rectangle, row by row (stride-aware), into
// dst which must be tightly packed with 4*Dx() bytes per row and zero-origin.
func (v *pixelView) snapshotInto(dst *image.RGBA) {
	if !v.Valid() || dst == nil {
		return
	}
	w := v.rect.Dx()
	h := v.rect.Dy()
	for y := 0; y < h; y++ {
		srcRow, ok1 := v.offset(0, y)
		if !ok1 {
			continue
		}
		dstRow := dst.Stride * y
		copy(dst.Pix[dstRow:dstRow+bytesPerPixel*w],
			v.pix[srcRow:srcRow+bytesPerPixel*w])
	}
}
