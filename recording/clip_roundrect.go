package recording

import (
	"math"

	"github.com/gogpu/gg"
)

// transformedClipRoundRect converts a user-space ClipRoundRectCommand into the
// exact device-space path that gg.Context.ClipRoundRect would push for the
// given current transform. It mirrors the context implementation branch for
// branch so that generic backends (which only receive transformed paths) keep
// pixel parity with a direct Context draw:
//
//   - radius <= 0 degenerates to a rectangle clip,
//   - translations, axis scales/reflections and uniform axis-aligned
//     transforms stay axis-aligned (rect or rounded rect),
//   - any rotation, shear or non-uniform axis scale becomes the transformed
//     user-space rounded-rectangle path, so elliptical/sheared corners and the
//     true quadrilateral bounds are preserved instead of collapsed to a box.
//
// A nil result means no clip path should be installed (for example when the
// transform collapses the shape or contains non-finite components). The
// raster backend's mask rasterizer naturally yields zero coverage for an
// empty/degenerate region, matching a direct call.
func transformedClipRoundRect(c ClipRoundRectCommand, m Matrix) *gg.Path {
	x, y, w, h := normalizeClipRect(c.X, c.Y, c.W, c.H)
	radius := c.Radius
	if radius <= 0 {
		if w <= 0 || h <= 0 || !matrixIsFinite(m) {
			return gg.BuildPath().Build()
		}
		return transformedRectClipPath(m, x, y, w, h)
	}

	// Clamp the user-space radius exactly like Path.RoundedRectangle and the
	// Context.ClipRoundRect axis-aligned branch do, so both playback paths
	// carry the same effective corner geometry.
	if maxRadius := math.Min(w, h) / 2; radius > maxRadius {
		radius = maxRadius
	}

	// Non-uniform axis scale, rotation or shear: transform the full
	// user-space rounded rectangle so the curved corners stay exact.
	if !isUniformAxisAligned(m) {
		if !matrixIsFinite(m) {
			return gg.BuildPath().Build()
		}
		userPath := gg.BuildPath().RoundRect(x, y, w, h, radius).Build()
		return userPath.Transform(toGGMatrix(m))
	}

	// Uniform axis-aligned transform (including reflections): the rounded
	// rectangle stays an axis-aligned rounded rectangle in device space.
	p1x, p1y := m.TransformPoint(x, y)
	p2x, p2y := m.TransformPoint(x+w, y+h)
	if !isFinite2(p1x, p1y) || !isFinite2(p2x, p2y) {
		return gg.BuildPath().Build()
	}

	devX := math.Min(p1x, p2x)
	devY := math.Min(p1y, p2y)
	devW := math.Abs(p2x - p1x)
	devH := math.Abs(p2y - p1y)
	if devW <= 0 || devH <= 0 {
		return gg.BuildPath().Build()
	}

	scaledRadius := radius * m.ScaleFactor()
	if maxRadius := math.Min(devW, devH) / 2; scaledRadius > maxRadius {
		scaledRadius = maxRadius
	}
	if scaledRadius <= 0 {
		return gg.BuildPath().Rect(devX, devY, devW, devH).Build()
	}
	return gg.BuildPath().RoundRect(devX, devY, devW, devH, scaledRadius).Build()
}

// transformedRectClipPath builds the device-space rectangle path used for the
// radius <= 0 fallback, matching Context.ClipRect for every affine transform.
func transformedRectClipPath(m Matrix, x, y, w, h float64) *gg.Path {
	if !isAxisAligned(m) {
		p := gg.NewPath()
		p1x, p1y := m.TransformPoint(x, y)
		p2x, p2y := m.TransformPoint(x+w, y)
		p3x, p3y := m.TransformPoint(x+w, y+h)
		p4x, p4y := m.TransformPoint(x, y+h)
		if !isFinite2(p1x, p1y) || !isFinite2(p2x, p2y) ||
			!isFinite2(p3x, p3y) || !isFinite2(p4x, p4y) {
			return gg.BuildPath().Build()
		}
		p.MoveTo(p1x, p1y)
		p.LineTo(p2x, p2y)
		p.LineTo(p3x, p3y)
		p.LineTo(p4x, p4y)
		p.Close()
		return p
	}

	p1x, p1y := m.TransformPoint(x, y)
	p2x, p2y := m.TransformPoint(x+w, y+h)
	if !isFinite2(p1x, p1y) || !isFinite2(p2x, p2y) {
		return gg.BuildPath().Build()
	}
	devX := math.Min(p1x, p2x)
	devY := math.Min(p1y, p2y)
	devW := math.Abs(p2x - p1x)
	devH := math.Abs(p2y - p1y)
	return gg.BuildPath().Rect(devX, devY, devW, devH).Build()
}

// normalizeClipRect rewrites negative dimensions into the geometric
// top-left corner plus positive extents, matching normalizeRect in gg.
func normalizeClipRect(x, y, w, h float64) (float64, float64, float64, float64) {
	if w < 0 {
		x += w
		w = -w
	}
	if h < 0 {
		y += h
		h = -h
	}
	return x, y, w, h
}

// isAxisAligned reports whether the linear part of m keeps the coordinate
// axes axis-aligned (possibly swapping them). Exact comparisons match the
// context: treating a tiny shear as zero causes large clipping errors.
func isAxisAligned(m Matrix) bool {
	return (m.B == 0 && m.D == 0) || (m.A == 0 && m.E == 0)
}

// isUniformAxisAligned reports whether m keeps rounded rectangles
// axis-aligned without turning circular corners into ellipses. Reflections
// are allowed; only the magnitudes of the two axis scales must match.
func isUniformAxisAligned(m Matrix) bool {
	switch {
	case m.B == 0 && m.D == 0:
		return math.Abs(m.A) == math.Abs(m.E)
	case m.A == 0 && m.E == 0:
		return math.Abs(m.B) == math.Abs(m.D)
	default:
		return false
	}
}

// matrixIsFinite reports whether every component of m is finite.
func matrixIsFinite(m Matrix) bool {
	return isFinite2(m.A, m.B) && isFinite2(m.D, m.E) && isFinite2(m.C, m.F)
}

func isFinite2(a, b float64) bool {
	return !math.IsNaN(a) && !math.IsInf(a, 0) &&
		!math.IsNaN(b) && !math.IsInf(b, 0)
}

func toGGMatrix(m Matrix) gg.Matrix {
	return gg.Matrix{A: m.A, B: m.B, C: m.C, D: m.D, E: m.E, F: m.F}
}
