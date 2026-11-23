// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"image"
	"image/color"
	"math"
)

// This file is an independent, deliberately naive reference implementation of
// the blending rules ImageSurface implements. It works in float64 on
// non-premultiplied colors and converts at the very end, so it shares no code
// with the integer premultiplied path in blend.go. Tests compare the two to
// catch formula and rounding mistakes rather than restating the implementation.

// refColor is a straight (non-premultiplied) color in [0, 1].
type refColor struct {
	R, G, B, A float64
}

// refFromColor converts a color.Color to straight floats using the documented
// premultiplied contract of color.Color.RGBA.
func refFromColor(c color.Color) refColor {
	if c == nil {
		return refColor{R: 0, G: 0, B: 0, A: 1}
	}
	r, g, b, a := c.RGBA()
	if a == 0 {
		return refColor{}
	}
	af := float64(a) / 65535
	return refColor{
		R: float64(r) / float64(a),
		G: float64(g) / float64(a),
		B: float64(b) / float64(a),
		A: af,
	}
}

// referenceCoveredPixel is the expected value of one pixel after a drawing
// operation with the given coverage and opacity.
//
// Porter-Duff modes (SourceOver, Clear, Copy) are first evaluated at full
// strength, then interpolated with the destination by coverage. The separable
// modes instead fold coverage*opacity into the source alpha before applying
// the W3C formula. This mirrors how the surface treats coverage and is stated
// directly in float64, independent of the integer implementation.
func referenceCoveredPixel(mode BlendMode, src color.Color, coverage, opacity float64, dst premulRGBA) premulRGBA {
	s := refFromColor(src)
	d := refFromPremul(dst)

	switch mode {
	case BlendModeClear, BlendModeCopy, BlendModeSourceOver:
		full := referenceBlend(mode, scaleRef(s, opacity), d)
		return refLerpPremul(refToPremulBytes(d), full, coverage)
	default:
		return referenceBlend(mode, scaleRef(s, coverage*opacity), d)
	}
}

// refLerpPremul interpolates premultiplied results by coverage.
func refLerpPremul(dst, src premulRGBA, t float64) premulRGBA {
	blend := func(a, b uint8) uint8 {
		v := math.Round(float64(a) + (float64(b)-float64(a))*t)
		if v <= 0 {
			return 0
		}
		if v >= 255 {
			return 255
		}
		return uint8(v)
	}
	return premulRGBA{
		R: blend(dst.R, src.R),
		G: blend(dst.G, src.G),
		B: blend(dst.B, src.B),
		A: blend(dst.A, src.A),
	}
}

// referenceBlend composites src over dst in straight-alpha float64 using the
// W3C Compositing and Blending Level 1 definitions, and returns premultiplied
// bytes ready to compare with a surface snapshot.
func referenceBlend(mode BlendMode, src, dst refColor) premulRGBA {
	var out refColor
	switch mode {
	case BlendModeClear:
		out = refColor{}
	case BlendModeCopy:
		out = src
	case BlendModeSourceOver:
		sa := src.A
		da := dst.A * (1 - sa)
		oa := sa + da
		if oa == 0 {
			return premulRGBA{}
		}
		out = refColor{
			R: (src.R*sa + dst.R*da) / oa,
			G: (src.G*sa + dst.G*da) / oa,
			B: (src.B*sa + dst.B*da) / oa,
			A: oa,
		}
	default:
		return refSeparablePremul(mode, src, dst)
	}
	return refToPremulBytes(out)
}

// refSeparablePremul implements the W3C blending plus source-over compositing
// directly in premultiplied output space:
//
//	Co = Sa*(1-Da)*Cs + Sa*Da*B(Cb, Cs) + Da*(1-Sa)*Cb
//	ao = Sa + Da*(1-Sa)
//
// Co is already premultiplied, so the result is converted to bytes without
// scaling by alpha again.
func refSeparablePremul(mode BlendMode, s, d refColor) premulRGBA {
	oa := s.A + d.A*(1-s.A)
	if oa == 0 {
		return premulRGBA{}
	}
	channel := func(sc, dc float64) float64 {
		blended := refBlendChannel(mode, sc, dc)
		return s.A*(1-d.A)*sc + s.A*d.A*blended + d.A*(1-s.A)*dc
	}
	return premulRGBA{
		R: refPremulChannel(channel(s.R, d.R)),
		G: refPremulChannel(channel(s.G, d.G)),
		B: refPremulChannel(channel(s.B, d.B)),
		A: refPremulChannel(oa),
	}
}

// refPremulChannel converts one already-premultiplied float channel to a byte.
func refPremulChannel(v float64) uint8 {
	if math.IsNaN(v) {
		return 0
	}
	v *= 255
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(math.Round(v))
}

// refBlendChannel applies the separable blend function to one straight channel.
func refBlendChannel(mode BlendMode, s, d float64) float64 {
	switch mode {
	case BlendModeMultiply:
		return s * d
	case BlendModeScreen:
		return 1 - (1-s)*(1-d)
	case BlendModeOverlay:
		if d <= 0.5 {
			return 2 * s * d
		}
		return 1 - 2*(1-s)*(1-d)
	default:
		return s
	}
}

// refToPremulBytes converts straight floats to premultiplied bytes, clamping.
func refToPremulBytes(c refColor) premulRGBA {
	clamp := func(v float64) uint8 {
		if math.IsNaN(v) {
			return 0
		}
		v *= 255
		if v <= 0 {
			return 0
		}
		if v >= 255 {
			return 255
		}
		return uint8(math.Round(v))
	}
	return premulRGBA{
		R: clamp(c.R * c.A),
		G: clamp(c.G * c.A),
		B: clamp(c.B * c.A),
		A: clamp(c.A),
	}
}

// refFromPremul converts premultiplied bytes to straight floats.
func refFromPremul(p premulRGBA) refColor {
	if p.A == 0 {
		return refColor{}
	}
	a := float64(p.A) / 255
	return refColor{
		R: float64(p.R) / float64(p.A),
		G: float64(p.G) / float64(p.A),
		B: float64(p.B) / float64(p.A),
		A: a,
	}
}

// solidImage returns a w×h image filled with c. It is a helper for image
// fixtures so tests can control the exact source pixels.
func solidImage(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}
