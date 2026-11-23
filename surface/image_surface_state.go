// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"math"

	"github.com/gogpu/gg/internal/raster"
)

// maxSafeCoord bounds path coordinates before they reach the fixed-point
// rasterizer.
//
// The edge builder clips edges to the surface rect, but the sentinel verticals
// it emits at the clip boundaries keep their original Y extent. A vertical
// spanning the full height of a huge path therefore still reaches the analytic
// filler's fixed-point conversion, which multiplies by 256 at 4x AA and shifts
// left another 8 bits into SkFixed. A coordinate beyond ±32768 overflows int32
// there, and the overflowed trapezoid can cover, or spill across, the whole
// surface.
//
// 2^15 is the largest magnitude that survives every fixed-point step. A shape
// entirely beyond this bound cannot touch a surface whose pixels live in
// [0, width) with width <= 32768 (the practical limit for an in-memory RGBA
// surface), so clamping there never changes visible pixels; the clip rect
// discards the remainder.
const maxSafeCoord = 1<<15 - 1

// surfaceState is the graphics state saved by Save and restored by Restore.
type surfaceState struct {
	blendMode BlendMode
	opacity   float64
}

// defaultState returns the initial graphics state.
func defaultState() surfaceState {
	return surfaceState{
		blendMode: BlendModeSourceOver,
		opacity:   1,
	}
}

// BlendMode returns the current blend mode.
//
// It reports the last successfully applied mode. It does not report a rejected
// mode: SetBlendMode and TrySetBlendMode leave the state unchanged on error.
func (s *ImageSurface) BlendMode() BlendMode {
	return s.state.blendMode
}

// SetBlendMode sets the blend mode for subsequent Fill, Stroke and DrawImage
// operations.
//
// A mode outside the supported set (see [BlendMode.IsValid]) is ignored: the
// current mode and the surface pixels are left unchanged. Because this method
// has no return value (it implements [BlendableSurface]), use
// [ImageSurface.TrySetBlendMode] or [ImageSurface.LastError] to observe the
// rejection with a diagnosable error.
func (s *ImageSurface) SetBlendMode(mode BlendMode) {
	_ = s.TrySetBlendMode(mode)
}

// TrySetBlendMode sets the blend mode and reports whether it was accepted.
//
// It returns [ErrSurfaceClosed] if the surface is closed, or an
// [*UnknownBlendModeError] (matching [ErrUnknownBlendMode]) if mode is not
// implemented. In both error cases the current blend mode is preserved.
func (s *ImageSurface) TrySetBlendMode(mode BlendMode) error {
	if s.closed {
		return s.recordError(ErrSurfaceClosed)
	}
	if !mode.IsValid() {
		return s.recordError(&UnknownBlendModeError{Mode: mode})
	}
	s.state.blendMode = mode
	s.lastErr = nil
	return nil
}

// SetOpacity sets the opacity applied to subsequent Fill, Stroke and DrawImage
// operations.
//
// The value is clamped to [0, 1]. NaN is treated as the default 1 so that a
// non-finite input cannot silently erase drawing. Opacity 0 makes drawing
// operations no-ops; it does not modify the pixels.
//
// It returns [ErrSurfaceClosed] if the surface is closed.
func (s *ImageSurface) SetOpacity(opacity float64) error {
	if s.closed {
		return s.recordError(ErrSurfaceClosed)
	}
	s.state.opacity = sanitizeOpacity(opacity)
	s.lastErr = nil
	return nil
}

// Opacity returns the current opacity in [0, 1].
func (s *ImageSurface) Opacity() float64 {
	return s.state.opacity
}

// Save pushes the current blend mode and opacity onto a state stack.
//
// Save has no effect on a closed surface.
func (s *ImageSurface) Save() {
	if s.closed {
		return
	}
	s.stateStack = append(s.stateStack, s.state)
}

// Restore restores the blend mode and opacity saved by the matching Save.
//
// Restoring with an empty stack is a no-op, matching the behavior of
// [Context.Pop]: an unbalanced Restore never panics and never changes state.
func (s *ImageSurface) Restore() {
	if s.closed {
		return
	}
	n := len(s.stateStack)
	if n == 0 {
		return
	}
	s.state = s.stateStack[n-1]
	s.stateStack = s.stateStack[:n-1]
}

// StateDepth returns the number of saved states on the stack.
func (s *ImageSurface) StateDepth() int {
	return len(s.stateStack)
}

// LastError returns the error recorded by the most recent failing state
// operation, or nil if the last state operation succeeded.
//
// Drawing methods report no errors by design; they either draw or do nothing.
// LastError lets callers that use the no-return [ImageSurface.SetBlendMode]
// still diagnose why a mode was not applied.
func (s *ImageSurface) LastError() error {
	return s.lastErr
}

// recordError stores err and returns it unchanged.
func (s *ImageSurface) recordError(err error) error {
	s.lastErr = err
	return err
}

// pixOffset returns the index of the pixel at surface coordinates (x, y) in the
// backing image.
//
// The backing image created by NewImageSurfaceFromImage may have a non-zero
// origin or a stride larger than its width, so image.RGBA.PixOffset cannot be
// used with surface-local coordinates directly. Surface coordinates are
// relative to the image's own rectangle origin, matching Width, Height and
// Snapshot.
func (s *ImageSurface) pixOffset(x, y int) int {
	return y*s.img.Stride + x*4
}

// surfaceClipRect returns the clip rectangle applied to every rasterization.
// It extends one pixel past the surface on each side so anti-aliased edge
// pixels and curve flattening near the border are unaffected.
func (s *ImageSurface) surfaceClipRect() raster.Rect {
	return raster.Rect{
		MinX: -2,
		MinY: -2,
		MaxX: float32(s.width) + 2,
		MaxY: float32(s.height) + 2,
	}
}

// applyClipBounds configures the edge builder for the current surface size.
func (s *ImageSurface) applyClipBounds() {
	r := s.surfaceClipRect()
	s.edgeBuilder.SetClipRect(&r)
}

// pathFinite reports whether every coordinate in path is finite.
func pathFinite(path *Path) bool {
	pts := path.Points()
	for _, v := range pts {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false
		}
	}
	return true
}

// clampPath returns a path safe for the fixed-point rasterizer.
//
// It reports false when the path contains a non-finite coordinate, in which
// case the caller must draw nothing: NaN cannot be clipped meaningfully and
// would otherwise reach int32 conversions and per-pixel span allocation.
//
// Finite coordinates beyond ±maxSafeCoord are clamped into that range. The
// surface clip rect discards everything outside the surface, so geometry wholly
// outside is unaffected. A degenerate shape may still be produced when one
// endpoint of a segment sits far outside and the other crosses the surface: the
// clamped endpoint can move a visible boundary. This is preferred to the
// alternative, a fixed-point overflow that can cover the entire surface, and
// only affects paths whose coordinates exceed ±32767 - far outside any normal
// drawing. The original path is never modified; if clamping is needed a copy is
// returned.
func clampPath(path *Path) (*Path, bool) {
	if !pathFinite(path) {
		return nil, false
	}
	pts := path.Points()
	needsClamp := false
	for _, v := range pts {
		f := float64(v)
		if f < -maxSafeCoord || f > maxSafeCoord {
			needsClamp = true
			break
		}
	}
	if !needsClamp {
		return path, true
	}

	clamped := path.Clone()
	for i, v := range clamped.points {
		f := float64(v)
		if f > maxSafeCoord {
			clamped.points[i] = maxSafeCoord
		} else if f < -maxSafeCoord {
			clamped.points[i] = -maxSafeCoord
		}
	}
	return clamped, true
}
