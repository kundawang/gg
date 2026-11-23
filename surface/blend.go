// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"errors"
	"fmt"
	"image/color"
	"math"
)

// Sentinel errors returned by ImageSurface blend/state methods.
var (
	// ErrSurfaceClosed is returned when a state method needs a live surface but
	// Close has already been called. Drawing methods stay safe no-ops after
	// Close; error-returning methods report the condition instead.
	ErrSurfaceClosed = errors.New("surface: surface is closed")

	// ErrUnknownBlendMode is returned when a blend mode outside the set
	// implemented by ImageSurface is requested. The surface's blend mode and
	// its pixels are left unchanged.
	ErrUnknownBlendMode = errors.New("surface: unknown blend mode")

	// ErrUnsupportedBlendMode is an alias for ErrUnknownBlendMode, for callers
	// that spell the condition "unsupported" rather than "unknown".
	ErrUnsupportedBlendMode = ErrUnknownBlendMode
)

// UnknownBlendModeError describes a rejected blend mode.
//
// It unwraps to ErrUnknownBlendMode, so both
// errors.Is(err, ErrUnknownBlendMode) and errors.As(err, &target) work.
type UnknownBlendModeError struct {
	// Mode is the rejected blend mode value.
	Mode BlendMode
}

// Error implements error.
func (e *UnknownBlendModeError) Error() string {
	return fmt.Sprintf("surface: unknown blend mode %d (%s)", uint8(e.Mode), e.Mode)
}

// Unwrap returns ErrUnknownBlendMode.
func (e *UnknownBlendModeError) Unwrap() error { return ErrUnknownBlendMode }

// IsValid reports whether m is a blend mode implemented by ImageSurface:
// SourceOver, Multiply, Screen, Overlay, Clear or Copy.
func (m BlendMode) IsValid() bool {
	return m <= BlendModeCopy
}

// String returns the blend mode name, or "Unknown" for unrecognized values.
func (m BlendMode) String() string {
	switch m {
	case BlendModeSourceOver:
		return "SourceOver"
	case BlendModeMultiply:
		return "Multiply"
	case BlendModeScreen:
		return "Screen"
	case BlendModeOverlay:
		return "Overlay"
	case BlendModeClear:
		return "Clear"
	case BlendModeCopy:
		return "Copy"
	default:
		return "Unknown"
	}
}

// premulRGBA is a color with premultiplied alpha, one byte per channel.
// It mirrors the storage layout of image.RGBA.
type premulRGBA struct {
	R, G, B, A uint8
}

// premulBytes converts any color.Color to premultiplied bytes.
//
// color.Color.RGBA is documented to return premultiplied 16-bit values, but a
// non-conforming implementation can return values outside that contract (for
// example after converting a NaN float). Those values are saturated here and
// the premultiplied invariant R,G,B <= A is enforced, so a malformed color can
// never write an invalid pixel.
func premulBytes(c color.Color) premulRGBA {
	if c == nil {
		// A nil color carries no channel information; return transparent
		// black so callers that treat the result as a source draw nothing.
		return premulRGBA{}
	}
	r, g, b, a := c.RGBA()
	if r > 0xffff {
		r = 0xffff
	}
	if g > 0xffff {
		g = 0xffff
	}
	if b > 0xffff {
		b = 0xffff
	}
	if a > 0xffff {
		a = 0xffff
	}
	if r > a {
		r = a
	}
	if g > a {
		g = a
	}
	if b > a {
		b = a
	}
	return premulRGBA{
		//nolint:gosec // G115: values are clamped to 16 bits, so >>8 fits uint8.
		R: uint8(r >> 8),
		//nolint:gosec // G115: clamped above.
		G: uint8(g >> 8),
		//nolint:gosec // G115: clamped above.
		B: uint8(b >> 8),
		//nolint:gosec // G115: clamped above.
		A: uint8(a >> 8),
	}
}

// sanitizeOpacity maps an opacity value into [0, 1].
// NaN is treated as the default 1.0; -Inf clamps to 0 and +Inf to 1.
func sanitizeOpacity(v float64) float64 {
	switch {
	case math.IsNaN(v):
		return 1
	case v <= 0:
		return 0
	case v >= 1:
		return 1
	default:
		return v
	}
}

// opacityByte converts an opacity in [0, 1] to a 0-255 factor.
func opacityByte(v float64) uint32 {
	v = sanitizeOpacity(v)
	//nolint:gosec // G115: v is in [0, 1], v*255+0.5 is in [0, 255.5].
	return uint32(v*255 + 0.5)
}

// div255 divides a 16-bit product by 255 with round-to-nearest.
func div255(v uint32) uint8 {
	return uint8((v + 127) / 255) //nolint:gosec // G115: (65025+127)/255 == 255.
}

// mulByte returns a*b/255 with round-to-nearest.
func mulByte(a, b uint8) uint8 {
	return div255(uint32(a) * uint32(b))
}

// addByte adds two bytes with saturation.
func addByte(a, b uint8) uint8 {
	sum := uint16(a) + uint16(b)
	if sum > 255 {
		return 255
	}
	return uint8(sum)
}

// scalePremul scales every premultiplied channel, including alpha, by the
// 0-255 factor f. Because all channels are premultiplied, one factor applies
// uniformly and the invariant channel <= alpha is preserved.
func scalePremul(c premulRGBA, f uint32) premulRGBA {
	if f == 255 {
		return c
	}
	return premulRGBA{
		R: div255(uint32(c.R) * f),
		G: div255(uint32(c.G) * f),
		B: div255(uint32(c.B) * f),
		A: div255(uint32(c.A) * f),
	}
}

// unpremul converts a premultiplied channel back to its straight value.
func unpremul(c, a uint8) uint8 {
	if a == 0 {
		return 0
	}
	return uint8((uint32(c)*255 + uint32(a)/2) / uint32(a)) //nolint:gosec // c <= a, result <= 255.
}

// blendChannel applies a separable blend function to straight (unpremultiplied)
// source and destination channels.
func blendChannel(mode BlendMode, s, d uint8) uint8 {
	switch mode {
	case BlendModeMultiply:
		return mulByte(s, d)
	case BlendModeScreen:
		return 255 - mulByte(255-s, 255-d)
	case BlendModeOverlay:
		// Overlay(Cs, Cb) = HardLight(Cb, Cs): the backdrop decides the branch.
		if d <= 127 {
			return div255(2 * uint32(s) * uint32(d))
		}
		return 255 - div255(2*uint32(255-s)*uint32(255-d))
	default:
		return s
	}
}

// blendPremul combines a premultiplied source over a premultiplied destination.
//
// Clear, Copy and SourceOver are Porter-Duff operators and apply to the full
// source value; their result is then interpolated with the destination by the
// pixel coverage so that anti-aliased edges fade between the two outcomes.
// The separable modes (Multiply, Screen, Overlay) instead use the W3C
// Compositing and Blending Level 1 formula, where coverage scales the source
// alpha before compositing:
//
//	Co = Sa*(1-Da)*Cs + Sa*Da*B(Cs, Cb) + (1-Sa)*Da*Cb
//	ao = Sa + Da*(1 - Sa)
//
// Reference: https://www.w3.org/TR/compositing-1/#blending
func blendPremul(mode BlendMode, s, d premulRGBA, coverage uint8) premulRGBA {
	switch mode {
	case BlendModeClear:
		return lerpPremul(d, premulRGBA{}, coverage)
	case BlendModeCopy:
		return lerpPremul(d, s, coverage)
	case BlendModeSourceOver:
		invSa := 255 - s.A
		out := premulRGBA{
			R: addByte(s.R, mulByte(d.R, invSa)),
			G: addByte(s.G, mulByte(d.G, invSa)),
			B: addByte(s.B, mulByte(d.B, invSa)),
			A: addByte(s.A, mulByte(d.A, invSa)),
		}
		return lerpPremul(d, out, coverage)
	default:
		return separableBlendPremul(mode, scalePremul(s, uint32(coverage)), d)
	}
}

// separableBlendPremul implements the W3C separable blend formula. The source
// is expected to be premultiplied and already scaled by coverage.
func separableBlendPremul(mode BlendMode, s, d premulRGBA) premulRGBA {
	if s.A == 0 {
		return d
	}
	if d.A == 0 {
		return s
	}
	invSa := 255 - s.A
	invDa := 255 - d.A

	out := premulRGBA{
		R: addByte(mulByte(d.R, invSa), mulByte(s.R, invDa)),
		G: addByte(mulByte(d.G, invSa), mulByte(s.G, invDa)),
		B: addByte(mulByte(d.B, invSa), mulByte(s.B, invDa)),
		A: addByte(s.A, mulByte(d.A, invSa)),
	}

	saDa := mulByte(s.A, d.A)
	bs := premulRGBA{
		R: blendChannel(mode, unpremul(s.R, s.A), unpremul(d.R, d.A)),
		G: blendChannel(mode, unpremul(s.G, s.A), unpremul(d.G, d.A)),
		B: blendChannel(mode, unpremul(s.B, s.A), unpremul(d.B, d.A)),
	}
	out.R = clampToAlpha(addByte(out.R, mulByte(saDa, bs.R)), out.A)
	out.G = clampToAlpha(addByte(out.G, mulByte(saDa, bs.G)), out.A)
	out.B = clampToAlpha(addByte(out.B, mulByte(saDa, bs.B)), out.A)
	return out
}

// lerpPremul interpolates from dst to src by coverage/255.
func lerpPremul(dst, src premulRGBA, coverage uint8) premulRGBA {
	if coverage == 255 {
		return src
	}
	if coverage == 0 {
		return dst
	}
	t := uint32(coverage)
	inv := 255 - t
	return premulRGBA{
		R: div255(uint32(dst.R)*inv + uint32(src.R)*t),
		G: div255(uint32(dst.G)*inv + uint32(src.G)*t),
		B: div255(uint32(dst.B)*inv + uint32(src.B)*t),
		A: div255(uint32(dst.A)*inv + uint32(src.A)*t),
	}
}

// clampToAlpha enforces the premultiplied invariant c <= a.
func clampToAlpha(c, a uint8) uint8 {
	if c > a {
		return a
	}
	return c
}
