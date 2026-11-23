// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"image"
	"image/color"
)

// Surface is the core rendering target abstraction.
//
// A Surface represents a 2D canvas that can be drawn to. Implementations may
// use CPU-based software rendering, GPU acceleration, or any other backend.
//
// Surfaces are NOT thread-safe. Each surface should be used from a single
// goroutine, or external synchronization must be used.
//
// Example usage:
//
//	s := surface.NewImageSurface(800, 600)
//	defer s.Close()
//
//	s.Clear(color.White)
//	s.Fill(path, surface.FillStyle{Color: color.RGBA{255, 0, 0, 255}})
//	img := s.Snapshot()
type Surface interface {
	// Width returns the surface width in pixels.
	Width() int

	// Height returns the surface height in pixels.
	Height() int

	// Clear fills the entire surface with the given color.
	// This is typically the fastest way to reset the surface.
	// Clear is not affected by a surface's blend mode or opacity.
	Clear(c color.Color)

	// Fill fills the given path using the specified style.
	// The path is not modified or consumed.
	Fill(path *Path, style FillStyle)

	// Stroke strokes the given path using the specified style.
	// The path is not modified or consumed.
	Stroke(path *Path, style StrokeStyle)

	// DrawImage draws an image at the specified position.
	// If opts is nil, default options are used.
	DrawImage(img image.Image, at Point, opts *DrawImageOptions)

	// Flush ensures all pending drawing operations are complete.
	// For CPU surfaces, this is typically a no-op.
	// For GPU surfaces, this may submit commands and wait for completion.
	// Returns an error if flushing fails.
	Flush() error

	// Snapshot returns the current surface contents as an RGBA image.
	// The returned image is a copy; modifications to it do not affect the surface.
	// This may be slow for GPU surfaces as it requires readback.
	Snapshot() *image.RGBA

	// Close releases all resources associated with the surface.
	// After Close, the surface must not be used.
	// Close is idempotent; multiple calls are safe.
	Close() error
}

// SubSurface is an optional interface for surfaces that support sub-regions.
type SubSurface interface {
	Surface

	// CreateSubSurface creates a new surface backed by a region of this surface.
	// Drawing to the sub-surface affects the parent surface.
	CreateSubSurface(bounds image.Rectangle) (Surface, error)
}

// ResizableSurface is an optional interface for surfaces that support resizing.
type ResizableSurface interface {
	Surface

	// Resize changes the surface dimensions.
	// Existing content may be discarded or preserved depending on implementation.
	Resize(width, height int) error
}

// ClippableSurface is an optional interface for surfaces with clipping support.
type ClippableSurface interface {
	Surface

	// SetClip sets the clipping region.
	// Only pixels within the path will be affected by subsequent operations.
	SetClip(path *Path)

	// ClearClip removes the clipping region.
	ClearClip()

	// PushClip saves the current clip state.
	PushClip()

	// PopClip restores the previous clip state.
	PopClip()
}

// BlendableSurface is an optional interface for surfaces with blend mode support.
//
// ImageSurface implements it. The blend mode set here applies to Fill, Stroke
// and DrawImage; Clear always resets the surface and ignores it.
// [ImageSurface.TrySetBlendMode] is the error-reporting variant for callers
// that need to detect a rejected mode.
type BlendableSurface interface {
	Surface

	// SetBlendMode sets the blend mode for subsequent operations.
	// An unsupported mode is ignored; see ImageSurface.TrySetBlendMode.
	SetBlendMode(mode BlendMode)

	// BlendMode returns the current blend mode.
	BlendMode() BlendMode
}

// StatefulSurface is an optional interface for surfaces that keep a savable
// drawing state (blend mode and opacity).
//
// ImageSurface implements it. Save/Restore nest like a stack: an unbalanced
// Restore on an empty stack is a no-op.
type StatefulSurface interface {
	Surface

	// Save pushes the current blend mode and opacity.
	Save()

	// Restore pops the most recently saved state, if any.
	Restore()

	// SetOpacity sets the opacity applied to subsequent drawing operations.
	// Values are clamped to [0, 1]; NaN is treated as 1.
	SetOpacity(opacity float64) error

	// Opacity returns the current opacity in [0, 1].
	Opacity() float64
}

// BlendMode specifies how source and destination colors are combined.
//
// Values are combined using premultiplied-alpha source-over compositing for
// the Porter-Duff modes (SourceOver, Clear, Copy) and the W3C Compositing and
// Blending Level 1 formula for the separable modes (Multiply, Screen,
// Overlay). Overlay's branches are chosen by the destination (backdrop), which
// matches the specification's definition of overlay as hard-light with the
// source and backdrop swapped.
type BlendMode uint8

const (
	// BlendModeSourceOver is the default Porter-Duff source-over mode.
	BlendModeSourceOver BlendMode = iota

	// BlendModeMultiply multiplies source and destination colors.
	BlendModeMultiply

	// BlendModeScreen is the inverse of multiply.
	BlendModeScreen

	// BlendModeOverlay combines multiply and screen. The destination decides
	// which branch a channel takes.
	BlendModeOverlay

	// BlendModeClear clears the destination: every covered pixel becomes
	// transparent black, regardless of the source color.
	BlendModeClear

	// BlendModeCopy replaces destination with source, including its alpha.
	BlendModeCopy
)

// Capabilities describes the optional features a surface supports.
type Capabilities struct {
	// SupportsSubSurface indicates CreateSubSurface is available.
	SupportsSubSurface bool

	// SupportsResize indicates Resize is available.
	SupportsResize bool

	// SupportsClipping indicates clipping operations are available.
	SupportsClipping bool

	// SupportsBlendModes indicates blend mode control is available.
	SupportsBlendModes bool

	// SupportsAntialias indicates anti-aliased rendering is available.
	SupportsAntialias bool

	// MaxWidth is the maximum supported width (0 = unlimited).
	MaxWidth int

	// MaxHeight is the maximum supported height (0 = unlimited).
	MaxHeight int
}

// CapableSurface is an optional interface for querying surface capabilities.
type CapableSurface interface {
	Surface

	// Capabilities returns the surface's capabilities.
	Capabilities() Capabilities
}
