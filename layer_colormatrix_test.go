package gg

import (
	"errors"
	"math"
	"testing"
)

// --- helper functions ---

// premulPixel returns the raw premultiplied bytes of the context's pixmap.
func premulPixel(pm *Pixmap, x, y int) [4]uint8 {
	i := (y*pm.Width() + x) * 4
	return [4]uint8{pm.Data()[i], pm.Data()[i+1], pm.Data()[i+2], pm.Data()[i+3]}
}

func nearByte(got uint8, want, tol int) bool {
	d := int(got) - want
	if d < 0 {
		d = -d
	}
	return d <= tol
}

func assertPremul(t *testing.T, label string, got [4]uint8, want [4]uint8, tol int) {
	t.Helper()
	for c := 0; c < 4; c++ {
		if !nearByte(got[c], int(want[c]), tol) {
			t.Fatalf("%s: premultiplied pixel = %v, want %v (±%d)", label, got, want, tol)
		}
	}
}

// --- ColorMatrix API ---

func TestColorMatrixSizeValidation(t *testing.T) {
	for _, n := range []int{0, 19, 21} {
		values := make([]float64, n)
		if _, err := NewColorMatrix(values); !errors.Is(err, ErrColorMatrixSize) {
			t.Errorf("NewColorMatrix(len=%d) error = %v, want ErrColorMatrixSize", n, err)
		}
	}

	values := make([]float64, 20)
	values[0], values[6], values[12], values[18] = 1, 1, 1, 1
	m, err := NewColorMatrix(values)
	if err != nil {
		t.Fatalf("NewColorMatrix(valid) = %v", err)
	}
	if !m.IsIdentity() {
		t.Error("NewColorMatrix(identity slice) should be identity")
	}
}

func TestColorMatrixIdentityAndSaturation(t *testing.T) {
	if !IdentityColorMatrix().IsIdentity() {
		t.Error("IdentityColorMatrix().IsIdentity() = false")
	}
	// SATURATION 1 must be identity too (Rec. 709 weights sum to 1).
	if !SaturationColorMatrix(1).IsIdentity() {
		t.Errorf("SaturationColorMatrix(1) should be identity, got %v", SaturationColorMatrix(1))
	}
	if GrayscaleColorMatrix() != SaturationColorMatrix(0) {
		t.Error("GrayscaleColorMatrix should equal SaturationColorMatrix(0)")
	}
}

func TestColorMatrixNonFinite(t *testing.T) {
	finite := IdentityColorMatrix()
	cases := map[string]ColorMatrix{
		"nan":  func() ColorMatrix { m := finite; m[4] = math.NaN(); return m }(),
		"+inf": func() ColorMatrix { m := finite; m[7] = math.Inf(1); return m }(),
		"-inf": func() ColorMatrix { m := finite; m[19] = math.Inf(-1); return m }(),
	}
	for name, m := range cases {
		if m.IsFinite() {
			t.Errorf("%s: IsFinite() = true, want false", name)
		}
	}
}

// --- Layer color matrix behavior ---

// TestLayerColorMatrix_SaturationOnOpaquePixels checks a saturation transform
// against hand-computed Rec. 709 math.
func TestLayerColorMatrix_SaturationOnOpaquePixels(t *testing.T) {
	dc := NewContext(8, 8)
	defer func() { _ = dc.Close() }()

	dc.PushLayerWithColorMatrix(BlendNormal, 1.0, GrayscaleColorMatrix())
	dc.SetRGBA(1, 0, 0, 1)
	dc.DrawRectangle(0, 0, 8, 8)
	if err := dc.Fill(); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	dc.PopLayer()

	// Grayscale red = 0.2126 -> 54/255 in every channel, opaque.
	want := [4]uint8{54, 54, 54, 255}
	assertPremul(t, "grayscale red", premulPixel(dc.pixmap, 4, 4), want, 2)
}

// TestLayerColorMatrix_KeepsTranslucentAlpha checks that a translucent pixel
// keeps its alpha through a saturation transform and keeps the right hue.
func TestLayerColorMatrix_KeepsTranslucentAlpha(t *testing.T) {
	for _, matrix := range []struct {
		name string
		m    ColorMatrix
	}{
		{"identity", IdentityColorMatrix()},
		{"saturation", SaturationColorMatrix(0)},
	} {
		t.Run(matrix.name, func(t *testing.T) {
			dc := NewContext(8, 8)
			defer func() { _ = dc.Close() }()

			dc.PushLayerWithColorMatrix(BlendNormal, 1.0, matrix.m)
			dc.SetRGBA(0, 1, 0, 0.5) // semi-transparent green
			dc.DrawRectangle(0, 0, 8, 8)
			if err := dc.Fill(); err != nil {
				t.Fatalf("Fill: %v", err)
			}
			dc.PopLayer()

			got := premulPixel(dc.pixmap, 4, 4)
			// Alpha must be unchanged (128) for both matrices.
			if !nearByte(got[3], 128, 2) {
				t.Fatalf("alpha = %d, want 128", got[3])
			}
			if matrix.name == "saturation" {
				// Grayscale green = 0.7152, premultiplied by 0.5 -> 91.
				want := [4]uint8{91, 91, 91, 128}
				assertPremul(t, "grayscale semi-green", got, want, 3)
			} else {
				// Identity leaves premultiplied green: 0,128,0,128.
				want := [4]uint8{0, 128, 0, 128}
				assertPremul(t, "identity semi-green", got, want, 2)
			}
		})
	}
}

// TestLayerColorMatrix_EmptyLayerNoOp checks a layer that draws nothing.
func TestLayerColorMatrix_EmptyLayerNoOp(t *testing.T) {
	dc := NewContext(8, 8)
	defer func() { _ = dc.Close() }()
	dc.ClearWithColor(RGBA{R: 0.2, G: 0.4, B: 0.6, A: 1})
	base := dc.pixmap
	before := premulPixel(dc.pixmap, 4, 4)

	dc.PushLayerWithColorMatrix(BlendNormal, 1.0, TintColorMatrix(Red))
	dc.PopLayer()

	after := premulPixel(dc.pixmap, 4, 4)
	if before != after {
		t.Errorf("empty layer changed pixels: before=%v after=%v", before, after)
	}
	if dc.pixmap != base {
		t.Error("empty layer push/pop did not restore the base pixmap")
	}
	if dc.basePixmap != nil {
		t.Error("basePixmap should be nil after the layer stack empties")
	}
}

// TestLayerColorMatrix_ZeroAlphaContract checks that a fully transparent source
// produces no visible output no matter what the matrix bias is.
func TestLayerColorMatrix_ZeroAlphaContract(t *testing.T) {
	dc := NewContext(4, 4)
	defer func() { _ = dc.Close() }()

	// Bias the transform to force alpha=1 and tons of red everywhere.
	m := IdentityColorMatrix()
	m[0], m[4] = 0, 1             // R' = 1
	m[5], m[9] = 0, 1             // G' = 1
	m[10], m[14] = 0, 1           // B' = 1
	m[15], m[18], m[19] = 0, 0, 1 // A' = 1

	dc.PushLayerWithColorMatrix(BlendNormal, 1.0, m)
	// Draw nothing: layer stays fully transparent.
	dc.PopLayer()

	want := [4]uint8{0, 0, 0, 0}
	assertPremul(t, "empty + biased matrix", premulPixel(dc.pixmap, 2, 2), want, 0)
}

// TestLayerColorMatrix_NonFiniteIgnored checks that a NaN/Inf coefficient makes
// the layer composite unchanged rather than corrupt the parent.
func TestLayerColorMatrix_NonFiniteIgnored(t *testing.T) {
	m := IdentityColorMatrix()
	m[4] = math.NaN()

	dc := NewContext(4, 4)
	defer func() { _ = dc.Close() }()
	dc.ClearWithColor(RGBA{R: 0.1, G: 0.2, B: 0.3, A: 1})
	before := premulPixel(dc.pixmap, 2, 2)

	dc.PushLayerWithColorMatrix(BlendNormal, 1.0, m)
	dc.SetRGBA(1, 1, 1, 1)
	dc.DrawRectangle(0, 0, 4, 4)
	if err := dc.Fill(); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	dc.PopLayer()

	// The layer must still draw (unchanged), so the pixel becomes white.
	assertPremul(t, "non-finite treated as no-op", premulPixel(dc.pixmap, 2, 2), [4]uint8{255, 255, 255, 255}, 2)
	if before == [4]uint8{255, 255, 255, 255} {
		t.Fatal("test setup broken: background already white")
	}
}

// TestLayerColorMatrix_ExtremeBiasClamps checks ±huge biases clamp to [0,255].
func TestLayerColorMatrix_ExtremeBiasClamps(t *testing.T) {
	dc := NewContext(4, 4)
	defer func() { _ = dc.Close() }()

	high := IdentityColorMatrix()
	high[4], high[9], high[14], high[19] = 1e9, 1e9, 1e9, 1e9
	dc.PushLayerWithColorMatrix(BlendNormal, 1.0, high)
	dc.SetRGBA(0, 0, 0, 1)
	dc.DrawRectangle(0, 0, 4, 4)
	_ = dc.Fill()
	dc.PopLayer()
	assertPremul(t, "huge positive bias", premulPixel(dc.pixmap, 2, 2), [4]uint8{255, 255, 255, 255}, 0)

	dc2 := NewContext(4, 4)
	defer func() { _ = dc2.Close() }()
	low := IdentityColorMatrix()
	low[0], low[6], low[12] = 1, 1, 1
	low[4], low[9], low[14] = -1e9, -1e9, -1e9
	dc2.PushLayerWithColorMatrix(BlendNormal, 1.0, low)
	dc2.SetRGBA(1, 1, 1, 1)
	dc2.DrawRectangle(0, 0, 4, 4)
	_ = dc2.Fill()
	dc2.PopLayer()
	assertPremul(t, "huge negative bias", premulPixel(dc2.pixmap, 2, 2), [4]uint8{0, 0, 0, 255}, 0)
}

// TestLayerColorMatrix_NestedOnlyInnerTransformed checks that a matrix on an
// inner layer does not alter pixels outside it, and that an outer matrix
// applies to the composite of the inner one (matrix runs first).
func TestLayerColorMatrix_NestedOnlyInnerTransformed(t *testing.T) {
	const size = 16
	dc := NewContext(size, size)
	defer func() { _ = dc.Close() }()
	dc.ClearWithColor(White)

	// Inner layer: grayscale. Outer layer: identity/tint saturated red.
	dc.PushLayerWithColorMatrix(BlendNormal, 1.0, TintColorMatrix(RGBA{R: 1, A: 1}))
	dc.PushLayerWithColorMatrix(BlendNormal, 1.0, GrayscaleColorMatrix())

	// Left half red, right half blue (different hues -> same gray).
	dc.SetRGBA(1, 0, 0, 1)
	dc.DrawRectangle(0, 0, size/2, size)
	_ = dc.Fill()
	dc.SetRGBA(0, 0, 1, 1)
	dc.DrawRectangle(size/2, 0, size/2, size)
	_ = dc.Fill()

	dc.PopLayer() // grayscale applied here
	dc.PopLayer() // red tint applied here

	// Both halves must be identical (grayscale collapsed hue), and red.
	left := premulPixel(dc.pixmap, 4, 8)
	right := premulPixel(dc.pixmap, 12, 8)
	if left != right {
		t.Fatalf("grayscale should make both halves identical: left=%v right=%v", left, right)
	}
	if left[0] <= 250 || left[1] >= 5 || left[2] >= 5 {
		t.Errorf("red tint over gray should be red, got %v", left)
	}
}

// TestLayerColorMatrix_OrderWithOpacity checks that the matrix runs before
// opacity: 50% opacity of a tinted layer halves alpha but not hue.
func TestLayerColorMatrix_OrderWithOpacity(t *testing.T) {
	dc := NewContext(4, 4)
	defer func() { _ = dc.Close() }()

	dc.PushLayerWithColorMatrix(BlendNormal, 0.5, TintColorMatrix(RGBA{R: 1, A: 1}))
	dc.SetRGBA(0, 0, 1, 1) // blue, tinted to red, then 50% opacity
	dc.DrawRectangle(0, 0, 4, 4)
	_ = dc.Fill()
	dc.PopLayer()

	// Tinted color is opaque red; at 50% opacity it is premultiplied 127/0/0/127.
	got := premulPixel(dc.pixmap, 2, 2)
	assertPremul(t, "tint then opacity", got, [4]uint8{127, 0, 0, 127}, 2)
}

// TestLayerColorMatrix_MatchesDirectFill checks that a layer with an identity
// matrix and no opacity produces the same bytes as drawing directly when the
// shape is a full opaque rectangle.
func TestLayerColorMatrix_MatchesDirectFill(t *testing.T) {
	const size = 8
	direct := NewContext(size, size)
	defer func() { _ = direct.Close() }()
	direct.SetRGBA(1, 0, 0, 1)
	direct.DrawRectangle(0, 0, size, size)
	_ = direct.Fill()

	layered := NewContext(size, size)
	defer func() { _ = layered.Close() }()
	layered.PushLayerWithColorMatrix(BlendNormal, 1.0, IdentityColorMatrix())
	layered.SetRGBA(1, 0, 0, 1)
	layered.DrawRectangle(0, 0, size, size)
	_ = layered.Fill()
	layered.PopLayer()

	if got, want := premulPixel(layered.pixmap, 4, 4), premulPixel(direct.pixmap, 4, 4); got != want {
		t.Errorf("layered %v != direct %v", got, want)
	}
}

// TestLayerColorMatrix_PremulCompositingIsAlphaCorrect verifies the compositor
// bug that motivated the rewrite: a 50% red layer over white must equal the
// direct fill, not a darker pink.
func TestLayerColorMatrix_PremulCompositingIsAlphaCorrect(t *testing.T) {
	const size = 4
	direct := NewContext(size, size)
	defer func() { _ = direct.Close() }()
	direct.ClearWithColor(White)
	direct.SetRGBA(1, 0, 0, 0.5)
	direct.DrawRectangle(0, 0, size, size)
	_ = direct.Fill()

	layered := NewContext(size, size)
	defer func() { _ = layered.Close() }()
	layered.ClearWithColor(White)
	layered.PushLayer(BlendNormal, 1.0)
	layered.SetRGBA(1, 0, 0, 0.5)
	layered.DrawRectangle(0, 0, size, size)
	_ = layered.Fill()
	layered.PopLayer()

	gotDirect := premulPixel(direct.pixmap, 2, 2)
	gotLayered := premulPixel(layered.pixmap, 2, 2)
	assertPremul(t, "layered vs direct", gotLayered, gotDirect, 1)
}

// TestLayerColorMatrix_OutsideBoundsUnaffected checks that pixels outside the
// drawn content of a transformed layer are byte-for-byte unchanged.
func TestLayerColorMatrix_OutsideBoundsUnaffected(t *testing.T) {
	const size = 8
	dc := NewContext(size, size)
	defer func() { _ = dc.Close() }()
	dc.ClearWithColor(RGBA{R: 0.3, G: 0.6, B: 0.9, A: 1})
	before := premulPixel(dc.pixmap, 6, 6)

	dc.PushLayerWithColorMatrix(BlendNormal, 1.0, SaturationColorMatrix(0))
	dc.SetRGBA(1, 0, 0, 1)
	dc.DrawRectangle(0, 0, 4, 4)
	_ = dc.Fill()
	dc.PopLayer()

	if after := premulPixel(dc.pixmap, 6, 6); after != before {
		t.Errorf("pixel outside layer content changed: before=%v after=%v", before, after)
	}
}
