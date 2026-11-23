// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"errors"
	"fmt"
	"image"
	"image/color"

	"github.com/gogpu/gg/internal/raster"
)

// Size limits for ImageSurface buffers. They are reported through
// Capabilities.MaxWidth/MaxHeight so callers can validate dimensions
// before attempting a resize or allocation.
const (
	// maxSurfaceDimension is the largest supported width or height.
	maxSurfaceDimension = 1 << 15 // 32768

	// maxSurfacePixels caps the total pixel count so a single buffer
	// stays below 1 GiB of RGBA data (and int32-safe on 32-bit builds).
	maxSurfacePixels = 1 << 28 // 268435456 pixels = 1 GiB RGBA
)

// Resize errors.
var (
	// ErrSurfaceClosed is returned when operating on a closed surface.
	ErrSurfaceClosed = errors.New("surface: surface is closed")

	// ErrInvalidDimensions is returned for zero or negative dimensions.
	ErrInvalidDimensions = errors.New("surface: dimensions must be positive")

	// ErrSurfaceTooLarge is returned when dimensions exceed
	// maxSurfaceDimension or maxSurfacePixels.
	ErrSurfaceTooLarge = errors.New("surface: dimensions exceed maximum supported size")
)

// ResizeAnchor specifies how existing content is positioned within the
// new dimensions when a surface is resized.
type ResizeAnchor int

const (
	// AnchorTopLeft pins existing content to the top-left corner (default).
	AnchorTopLeft ResizeAnchor = iota

	// AnchorTop centers content horizontally, pinned to the top edge.
	AnchorTop

	// AnchorTopRight pins content to the top-right corner.
	AnchorTopRight

	// AnchorLeft centers content vertically, pinned to the left edge.
	AnchorLeft

	// AnchorCenter centers content both horizontally and vertically.
	AnchorCenter

	// AnchorRight centers content vertically, pinned to the right edge.
	AnchorRight

	// AnchorBottomLeft pins content to the bottom-left corner.
	AnchorBottomLeft

	// AnchorBottom centers content horizontally, pinned to the bottom edge.
	AnchorBottom

	// AnchorBottomRight pins content to the bottom-right corner.
	AnchorBottomRight
)

// anchorOffset returns the (dx, dy) translation applied to existing content
// for the given anchor when moving from (oldW, oldH) to (newW, newH).
// Unknown anchor values fall back to AnchorTopLeft.
func anchorOffset(anchor ResizeAnchor, oldW, oldH, newW, newH int) (dx, dy int) {
	switch anchor {
	case AnchorTop, AnchorCenter, AnchorBottom:
		dx = (newW - oldW) / 2
	case AnchorTopRight, AnchorRight, AnchorBottomRight:
		dx = newW - oldW
	}

	switch anchor {
	case AnchorLeft, AnchorCenter, AnchorRight:
		dy = (newH - oldH) / 2
	case AnchorBottomLeft, AnchorBottom, AnchorBottomRight:
		dy = newH - oldH
	}

	return dx, dy
}

// ResizeOptions controls how ImageSurface.ResizeWithOptions repositions
// existing content when the surface dimensions change.
type ResizeOptions struct {
	// Anchor determines where the old content is placed within the new
	// dimensions. The zero value is AnchorTopLeft.
	Anchor ResizeAnchor

	// OffsetX and OffsetY shift the anchored content by an additional
	// number of pixels. Negative values move content left/up; content
	// shifted outside the new bounds is clipped away.
	OffsetX, OffsetY int

	// ClearColor fills the area not covered by the copied content.
	// A nil ClearColor leaves new areas fully transparent.
	ClearColor color.Color
}

// validateResizeDimensions checks dimensions against surface limits.
func validateResizeDimensions(width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("%w: got %dx%d", ErrInvalidDimensions, width, height)
	}
	if width > maxSurfaceDimension || height > maxSurfaceDimension {
		return fmt.Errorf("%w: %dx%d exceeds max dimension %d",
			ErrSurfaceTooLarge, width, height, maxSurfaceDimension)
	}
	// int64 math keeps the product overflow-free on every platform.
	if int64(width)*int64(height) > maxSurfacePixels {
		return fmt.Errorf("%w: %dx%d exceeds max pixel count %d",
			ErrSurfaceTooLarge, width, height, maxSurfacePixels)
	}
	return nil
}

// Resize changes the surface dimensions, preserving existing content
// anchored to the top-left corner. Newly exposed areas are left transparent.
//
// It is equivalent to ResizeWithOptions with a zero ResizeOptions and
// satisfies the ResizableSurface interface.
func (s *ImageSurface) Resize(width, height int) error {
	return s.ResizeWithOptions(width, height, nil)
}

// ResizeWithOptions changes the surface dimensions while preserving existing
// content according to opts (nil opts uses zero-value defaults).
//
// The old content is copied byte-for-byte (no blending, so semi-transparent
// pixels are preserved exactly) to its anchored position plus opts.OffsetX/Y.
// Pixels that fall outside the new bounds are clipped; newly exposed areas
// are filled with opts.ClearColor (transparent when nil).
//
// Semantics:
//   - The backing buffer is always reallocated with stride = 4*width and
//     origin (0,0), unless the call is a no-op (same dimensions and zero
//     effective offset).
//   - Snapshots taken before the resize are independent copies and remain
//     valid and unchanged.
//   - A surface created with NewImageSurfaceFromImage detaches from the
//     original image on the first non-no-op resize: subsequent drawing no
//     longer writes to the caller's image.
//   - References obtained via Image() before the resize still point to the
//     old buffer and are no longer updated by the surface.
//
// Resize is safe to call concurrently with other surface methods; operations
// are serialized internally.
func (s *ImageSurface) ResizeWithOptions(width, height int, opts *ResizeOptions) error {
	if err := validateResizeDimensions(width, height); err != nil {
		return err
	}

	var o ResizeOptions
	if opts != nil {
		o = *opts
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrSurfaceClosed
	}

	offX, offY := anchorOffset(o.Anchor, s.width, s.height, width, height)
	offX += o.OffsetX
	offY += o.OffsetY

	// No-op when nothing would change.
	if width == s.width && height == s.height && offX == 0 && offY == 0 {
		return nil
	}

	newImg := image.NewRGBA(image.Rect(0, 0, width, height))

	// Fill newly exposed area with the clear color (exact byte fill, no
	// blending, so the result matches the requested color exactly).
	if o.ClearColor != nil {
		r, g, b, a := o.ClearColor.RGBA()
		//nolint:gosec // G115: safe - values from RGBA() >> 8 are in [0, 255]
		clearPx := [4]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
		if clearPx != [4]uint8{} {
			fillRGBA(newImg, clearPx)
		}
	}

	// Copy the overlapping region of the old content byte-for-byte.
	// Logical surface coordinate (0,0) corresponds to the old image's
	// bounds minimum, so images with a non-zero origin copy correctly.
	oldB := s.img.Bounds()
	x0 := max(offX, 0)
	y0 := max(offY, 0)
	x1 := min(offX+s.width, width)
	y1 := min(offY+s.height, height)
	if x1 > x0 && y1 > y0 {
		n := (x1 - x0) * 4
		for dy := y0; dy < y1; dy++ {
			srcOff := s.img.PixOffset(oldB.Min.X+x0-offX, oldB.Min.Y+dy-offY)
			dstOff := newImg.PixOffset(x0, dy)
			copy(newImg.Pix[dstOff:dstOff+n], s.img.Pix[srcOff:srcOff+n])
		}
	}

	s.img = newImg
	s.width = width
	s.height = height
	s.filler = raster.NewAnalyticFiller(width, height)
	return nil
}

// fillRGBA fills the entire image with a single pixel value.
func fillRGBA(img *image.RGBA, px [4]uint8) {
	row := img.Pix[:img.Stride]
	for x := 0; x < img.Bounds().Dx(); x++ {
		i := x * 4
		row[i+0] = px[0]
		row[i+1] = px[1]
		row[i+2] = px[2]
		row[i+3] = px[3]
	}
	for y := 1; y < img.Bounds().Dy(); y++ {
		off := y * img.Stride
		copy(img.Pix[off:off+img.Stride], row)
	}
}
