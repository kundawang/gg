package gg

import (
	"errors"
	"fmt"
	"math"
)

// ErrColorMatrixSize is returned when a color matrix does not contain exactly
// 20 coefficients (4 rows of 5 columns).
var ErrColorMatrixSize = errors.New("gg: color matrix must have exactly 20 elements")

// colorMatrixCoefficients is the number of coefficients in a 4x5 matrix.
const colorMatrixCoefficients = 20

// ColorMatrix is a 4x5 RGBA color transformation matrix in row-major order.
//
// The transformation is applied to straight-alpha colors in the [0, 1] range:
//
//	[R']   [m0  m1  m2  m3  m4 ]   [R]
//	[G']   [m5  m6  m7  m8  m9 ]   [G]
//	[B'] = [m10 m11 m12 m13 m14] * [B]
//	[A']   [m15 m16 m17 m18 m19]   [A]
//	                                [1]
//
// The fifth column (m4, m9, m14, m19) is a bias/offset in [0, 1] units, not a
// 0-255 byte offset. The fourth column scales by the input alpha, as in SVG
// feColorMatrix and Skia SkColorMatrix.
//
// The matrix is applied to a layer's composited contents when PopLayer runs,
// before the layer's opacity and blend mode are applied. Fully transparent
// pixels stay transparent: the bias cannot create alpha from a zero-alpha
// pixel. Output channels are clamped to [0, 1].
//
// A layer with a non-finite coefficient (NaN or ±Inf) is rendered without the
// transformation. The zero value is the all-zero matrix, which produces
// transparent black; use [IdentityColorMatrix] for a pass-through.
type ColorMatrix [colorMatrixCoefficients]float64

// IdentityColorMatrix returns the 4x5 matrix that leaves every pixel unchanged.
func IdentityColorMatrix() ColorMatrix {
	return ColorMatrix{
		1, 0, 0, 0, 0,
		0, 1, 0, 0, 0,
		0, 0, 1, 0, 0,
		0, 0, 0, 1, 0,
	}
}

// NewColorMatrix builds a ColorMatrix from a slice of exactly 20 row-major
// coefficients. It returns [ErrColorMatrixSize] for any other length.
//
// Non-finite coefficients are accepted here but are ignored when the matrix is
// attached to a layer (see [ColorMatrix.IsFinite] and
// [Context.PushLayerWithColorMatrix]).
func NewColorMatrix(values []float64) (ColorMatrix, error) {
	var m ColorMatrix
	if len(values) != len(m) {
		return m, fmt.Errorf("%w: got %d", ErrColorMatrixSize, len(values))
	}
	copy(m[:], values)
	return m, nil
}

// IsIdentity reports whether the matrix is exactly the identity transform.
func (m ColorMatrix) IsIdentity() bool {
	for i, v := range m {
		want := 0.0
		if i == 0 || i == 6 || i == 12 || i == 18 {
			want = 1.0
		}
		if v != want {
			return false
		}
	}
	return true
}

// IsFinite reports whether every coefficient is a finite number (not NaN and
// not ±Inf). A layer attached to a non-finite matrix is composited unchanged.
func (m ColorMatrix) IsFinite() bool {
	for _, v := range m {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// SaturationColorMatrix returns a matrix that adjusts saturation.
//
// saturation 0 produces grayscale using the Rec. 709 luminance weights
// (0.2126 R + 0.7152 G + 0.0722 B); 1 leaves colors unchanged; values above 1
// oversaturate. Alpha is unchanged.
func SaturationColorMatrix(saturation float64) ColorMatrix {
	const (
		lumR = 0.2126
		lumG = 0.7152
		lumB = 0.0722
	)
	inv := 1 - saturation
	return ColorMatrix{
		lumR*inv + saturation, lumG * inv, lumB * inv, 0, 0,
		lumR * inv, lumG*inv + saturation, lumB * inv, 0, 0,
		lumR * inv, lumG * inv, lumB*inv + saturation, 0, 0,
		0, 0, 0, 1, 0,
	}
}

// GrayscaleColorMatrix returns the Rec. 709 grayscale matrix. It is shorthand
// for SaturationColorMatrix(0).
func GrayscaleColorMatrix() ColorMatrix {
	return SaturationColorMatrix(0)
}

// TintColorMatrix returns a matrix that blends every RGB channel toward tint.
// The blend factor is tint.A: 0 leaves colors unchanged, 1 replaces them with
// tint's RGB. Alpha is unchanged. Tint components are clamped to [0, 1].
//
// Unlike multiplying by a colored light, this keeps the layer's alpha and only
// moves hue; it is the usual "damage flash" transform for HUD layers.
func TintColorMatrix(tint RGBA) ColorMatrix {
	f := clampUnit(tint.A)
	inv := 1 - f
	return ColorMatrix{
		inv, 0, 0, 0, clampUnit(tint.R) * f,
		0, inv, 0, 0, clampUnit(tint.G) * f,
		0, 0, inv, 0, clampUnit(tint.B) * f,
		0, 0, 0, 1, 0,
	}
}

// transformColorMatrix applies the matrix to one premultiplied source pixel and
// returns the resulting premultiplied pixel. A fully transparent source stays
// fully transparent. NaN results are written as zero (defined behavior for
// overflowing finite coefficients).
func (m *ColorMatrix) transformColorMatrix(pr, pg, pb, pa uint8) (uint8, uint8, uint8, uint8) {
	if pa == 0 {
		return 0, 0, 0, 0
	}

	// Un-premultiply to straight-alpha [0, 1] before the transform.
	a := float64(pa) / 255.0
	invA := 1 / a
	r := (float64(pr) / 255.0) * invA
	g := (float64(pg) / 255.0) * invA
	b := (float64(pb) / 255.0) * invA

	nr := clampUnit(m[0]*r + m[1]*g + m[2]*b + m[3]*a + m[4])
	ng := clampUnit(m[5]*r + m[6]*g + m[7]*b + m[8]*a + m[9])
	nb := clampUnit(m[10]*r + m[11]*g + m[12]*b + m[13]*a + m[14])
	na := clampUnit(m[15]*r + m[16]*g + m[17]*b + m[18]*a + m[19])

	if na == 0 {
		return 0, 0, 0, 0
	}

	// Re-premultiply for storage and compositing.
	return unitToByte(nr * na), unitToByte(ng * na), unitToByte(nb * na), unitToByte(na)
}

// clampUnit clamps v to [0, 1]. NaN (which is neither < 0 nor > 1) is mapped to
// zero so a degenerate transform has a defined result.
func clampUnit(v float64) float64 {
	switch {
	case math.IsNaN(v):
		return 0
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

// unitToByte converts a clamped [0, 1] value to an 8-bit channel with rounding.
func unitToByte(v float64) uint8 {
	return uint8(clampUnit(v)*255.0 + 0.5)
}
