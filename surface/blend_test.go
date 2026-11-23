// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"errors"
	"image"
	"image/color"
	"math"
	"testing"
)

// blendModes is every mode ImageSurface documents as supported.
var blendModes = []BlendMode{
	BlendModeSourceOver,
	BlendModeMultiply,
	BlendModeScreen,
	BlendModeOverlay,
	BlendModeClear,
	BlendModeCopy,
}

// testColors exercises opaque, translucent and fully transparent sources over
// opaque, translucent and transparent destinations.
var testColors = []struct {
	name string
	r    uint8
	g    uint8
	b    uint8
	a    uint8
}{
	{"opaque-red", 255, 0, 0, 255},
	{"opaque-blue", 0, 0, 255, 255},
	{"opaque-green", 0, 255, 0, 255},
	{"opaque-white", 255, 255, 255, 255},
	{"opaque-black", 0, 0, 0, 255},
	{"opaque-gray", 128, 128, 128, 255},
	{"half-red", 255, 0, 0, 128},
	{"quarter-blue", 0, 0, 255, 64},
	{"mostly-green", 0, 255, 0, 200},
	{"transparent", 0, 0, 0, 0},
}

func colorFrom(r, g, b, a uint8) color.NRGBA {
	return color.NRGBA{R: r, G: g, B: b, A: a}
}

// maxBlendDiff is the tolerance for comparing against the float64 reference.
// The integer path rounds in premultiplied 8-bit space, so a channel may differ
// from the reference by a couple of units at low alpha.
const maxBlendDiff = 2

func diffBytes(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func assertPixelNear(t *testing.T, got premulRGBA, want premulRGBA, context string) {
	t.Helper()
	for _, ch := range []struct {
		name     string
		got, exp uint8
	}{
		{"R", got.R, want.R},
		{"G", got.G, want.G},
		{"B", got.B, want.B},
		{"A", got.A, want.A},
	} {
		if diffBytes(ch.got, ch.exp) > maxBlendDiff {
			t.Errorf("%s: %s = %d, want %d (got %+v, want %+v)",
				context, ch.name, ch.got, ch.exp, got, want)
		}
	}
}

func pixelAt(img *image.RGBA, x, y int) premulRGBA {
	c := img.RGBAAt(x, y)
	return premulRGBA{R: c.R, G: c.G, B: c.B, A: c.A}
}

// --- 4. Semi-transparent red over blue, every mode, against the reference ---

func TestImageSurfaceBlendModesMatchReference(t *testing.T) {
	bg := colorFrom(0, 0, 255, 255)  // opaque blue
	src := colorFrom(255, 0, 0, 128) // half-transparent red

	for _, mode := range blendModes {
		t.Run(mode.String(), func(t *testing.T) {
			s := NewImageSurface(8, 8)
			defer s.Close()
			s.Clear(bg)

			if err := s.TrySetBlendMode(mode); err != nil {
				t.Fatalf("TrySetBlendMode(%v): %v", mode, err)
			}
			path := NewPath()
			path.Rectangle(0, 0, 8, 8)
			s.Fill(path, FillStyle{Color: src})

			got := pixelAt(s.Snapshot(), 4, 4)
			expect := referenceBlend(mode, refFromColor(src), refFromColor(bg))
			assertPixelNear(t, got, expect, "fill "+mode.String())
		})
	}
}

func TestImageSurfaceSemiTransparentRedOverBlue(t *testing.T) {
	s := NewImageSurface(4, 4)
	defer s.Close()
	s.Clear(colorFrom(0, 0, 255, 255))

	path := NewPath()
	path.Rectangle(0, 0, 4, 4)
	s.Fill(path, FillStyle{Color: colorFrom(255, 0, 0, 128)})

	p := pixelAt(s.Snapshot(), 2, 2)
	// out = src + dst*(1-Sa): R = 128*255/255 ... in premultiplied bytes
	// Sa = 128/255; R = 128 (premultiplied), B = 255*(1-128/255) = 127.
	if diffBytes(p.R, 128) > 1 {
		t.Errorf("R = %d, want ~128", p.R)
	}
	if p.G > 1 {
		t.Errorf("G = %d, want ~0", p.G)
	}
	if diffBytes(p.B, 127) > 1 {
		t.Errorf("B = %d, want ~127", p.B)
	}
	if p.A != 255 {
		t.Errorf("A = %d, want 255", p.A)
	}
}

// --- All source/destination colors, all modes, against the reference --------

func TestImageSurfaceBlendPixelMatrix(t *testing.T) {
	for _, mode := range blendModes {
		for _, bgc := range testColors {
			for _, srcC := range testColors {
				name := mode.String() + "/dst=" + bgc.name + "/src=" + srcC.name
				t.Run(name, func(t *testing.T) {
					s := NewImageSurface(1, 1)
					defer s.Close()
					bg := colorFrom(bgc.r, bgc.g, bgc.b, bgc.a)
					s.Clear(bg)
					if err := s.TrySetBlendMode(mode); err != nil {
						t.Fatalf("TrySetBlendMode: %v", err)
					}

					src := colorFrom(srcC.r, srcC.g, srcC.b, srcC.a)
					srcPremul := premulBytes(src)
					s.blendPremulPixel(0, 0, srcPremul, 255, mode)

					want := referenceBlend(mode, refFromColor(src), refFromColor(bg))
					got := pixelAt(s.Image(), 0, 0)
					assertPixelNear(t, got, want, mode.String())
				})
			}
		}
	}
}

// --- Overlay asymmetry: it is hard-light with source/backdrop swapped -------

func TestImageSurfaceOverlayAsymmetric(t *testing.T) {
	dark := colorFrom(64, 64, 64, 255)
	light := colorFrom(192, 192, 192, 255)

	// Overlay(dark, light) multiplies (backdrop > 0.5 -> screen branch).
	s1 := NewImageSurface(1, 1)
	defer s1.Close()
	s1.Clear(light)
	_ = s1.TrySetBlendMode(BlendModeOverlay)
	s1.blendPremulPixel(0, 0, premulBytes(dark), 255, BlendModeOverlay)
	over := pixelAt(s1.Image(), 0, 0)

	// Overlay(light, dark) screens only where the backdrop is dark.
	s2 := NewImageSurface(1, 1)
	defer s2.Close()
	s2.Clear(dark)
	_ = s2.TrySetBlendMode(BlendModeOverlay)
	s2.blendPremulPixel(0, 0, premulBytes(light), 255, BlendModeOverlay)
	under := pixelAt(s2.Image(), 0, 0)

	if over == under {
		t.Fatalf("overlay should be asymmetric, both directions gave %+v", over)
	}
}

// --- 2. Save/Restore, opacity ------------------------------------------------

func TestImageSurfaceSaveRestore(t *testing.T) {
	s := NewImageSurface(4, 4)
	defer s.Close()

	if s.BlendMode() != BlendModeSourceOver {
		t.Fatalf("default blend mode = %v", s.BlendMode())
	}
	if s.Opacity() != 1 {
		t.Fatalf("default opacity = %v", s.Opacity())
	}

	s.Save()
	if err := s.TrySetBlendMode(BlendModeMultiply); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOpacity(0.25); err != nil {
		t.Fatal(err)
	}
	if s.StateDepth() != 1 {
		t.Fatalf("StateDepth = %d", s.StateDepth())
	}

	s.Restore()
	if s.BlendMode() != BlendModeSourceOver {
		t.Errorf("blend mode after Restore = %v, want SourceOver", s.BlendMode())
	}
	if s.Opacity() != 1 {
		t.Errorf("opacity after Restore = %v, want 1", s.Opacity())
	}
	if s.StateDepth() != 0 {
		t.Errorf("StateDepth after Restore = %d", s.StateDepth())
	}
}

func TestImageSurfaceRestoreEmptyIsNoOp(t *testing.T) {
	s := NewImageSurface(2, 2)
	defer s.Close()
	_ = s.TrySetBlendMode(BlendModeScreen)

	s.Restore() // unbalanced Restore must not panic or change state

	if s.BlendMode() != BlendModeScreen {
		t.Errorf("blend mode = %v, want Screen", s.BlendMode())
	}
	if s.StateDepth() != 0 {
		t.Errorf("StateDepth = %d, want 0", s.StateDepth())
	}
}

func TestImageSurfaceSaveRestoreNested(t *testing.T) {
	s := NewImageSurface(2, 2)
	defer s.Close()

	s.Save()
	_ = s.TrySetBlendMode(BlendModeMultiply)
	s.Save()
	_ = s.TrySetBlendMode(BlendModeScreen)
	_ = s.SetOpacity(0.5)

	s.Restore()
	if s.BlendMode() != BlendModeMultiply {
		t.Errorf("after one Restore: blend = %v", s.BlendMode())
	}
	if s.Opacity() != 1 {
		t.Errorf("after one Restore: opacity = %v", s.Opacity())
	}
	s.Restore()
	if s.BlendMode() != BlendModeSourceOver {
		t.Errorf("after two Restores: blend = %v", s.BlendMode())
	}
}

func TestImageSurfaceOpacityScalesDrawing(t *testing.T) {
	for _, mode := range blendModes {
		t.Run(mode.String(), func(t *testing.T) {
			bg := colorFrom(0, 255, 0, 255)
			src := colorFrom(255, 0, 0, 255)

			s := NewImageSurface(4, 4)
			defer s.Close()
			s.Clear(bg)
			if err := s.TrySetBlendMode(mode); err != nil {
				t.Fatal(err)
			}
			if err := s.SetOpacity(0.5); err != nil {
				t.Fatal(err)
			}
			path := NewPath()
			path.Rectangle(0, 0, 4, 4)
			s.Fill(path, FillStyle{Color: src})

			got := pixelAt(s.Snapshot(), 2, 2)
			want := referenceCoveredPixel(mode, src, 1, 0.5, premulBytes(bg))
			assertPixelNear(t, got, want, "opacity "+mode.String())
		})
	}
}

func TestImageSurfaceOpacityZeroIsNoOp(t *testing.T) {
	s := NewImageSurface(4, 4)
	defer s.Close()
	s.Clear(colorFrom(10, 20, 30, 255))
	before := pixelAt(s.Snapshot(), 1, 1)

	_ = s.SetOpacity(0)
	path := NewPath()
	path.Circle(2, 2, 2)
	s.Fill(path, FillStyle{Color: colorFrom(255, 255, 255, 255)})
	s.Stroke(path, StrokeStyle{Color: colorFrom(255, 0, 0, 255), Width: 2})
	s.DrawImage(solidImage(4, 4, color.White), Pt(0, 0), nil)

	if after := pixelAt(s.Snapshot(), 1, 1); after != before {
		t.Errorf("opacity 0 changed pixel: %+v -> %+v", before, after)
	}
}

func TestImageSurfaceOpacityClamping(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{"negative", -1, 0},
		{"above-one", 4, 1},
		{"nan-defaults-to-one", math.NaN(), 1},
		{"plus-inf", math.Inf(1), 1},
		{"minus-inf", math.Inf(-1), 0},
		{"normal", 0.25, 0.25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewImageSurface(1, 1)
			defer s.Close()
			if err := s.SetOpacity(tt.in); err != nil {
				t.Fatalf("SetOpacity: %v", err)
			}
			if s.Opacity() != tt.want {
				t.Errorf("Opacity() = %v, want %v", s.Opacity(), tt.want)
			}
		})
	}
}

// --- 2. Unknown modes -------------------------------------------------------

func TestImageSurfaceUnknownBlendMode(t *testing.T) {
	unknown := []BlendMode{BlendMode(6), BlendMode(42), 255}

	for _, mode := range unknown {
		t.Run(mode.String(), func(t *testing.T) {
			s := NewImageSurface(4, 4)
			defer s.Close()
			s.Clear(colorFrom(1, 2, 3, 255))
			_ = s.TrySetBlendMode(BlendModeScreen)

			err := s.TrySetBlendMode(mode)
			if err == nil {
				t.Fatal("expected an error for an unknown blend mode")
			}
			if !errors.Is(err, ErrUnknownBlendMode) {
				t.Errorf("errors.Is(err, ErrUnknownBlendMode) = false; err = %v", err)
			}
			if !errors.Is(err, ErrUnsupportedBlendMode) {
				t.Errorf("errors.Is(err, ErrUnsupportedBlendMode) = false; err = %v", err)
			}
			var modeErr *UnknownBlendModeError
			if !errors.As(err, &modeErr) {
				t.Fatalf("errors.As did not produce *UnknownBlendModeError; err = %v", err)
			}
			if modeErr.Mode != mode {
				t.Errorf("error mode = %v, want %v", modeErr.Mode, mode)
			}

			// State and pixels are unchanged.
			if s.BlendMode() != BlendModeScreen {
				t.Errorf("blend mode changed to %v", s.BlendMode())
			}
			if !errors.Is(s.LastError(), ErrUnknownBlendMode) {
				t.Errorf("LastError = %v", s.LastError())
			}
			if p := pixelAt(s.Snapshot(), 1, 1); p != (premulRGBA{R: 1, G: 2, B: 3, A: 255}) {
				t.Errorf("pixels changed: %+v", p)
			}
		})
	}
}

func TestImageSurfaceSetBlendModeRecordsError(t *testing.T) {
	s := NewImageSurface(2, 2)
	defer s.Close()

	_ = s.TrySetBlendMode(BlendModeMultiply)
	if s.LastError() != nil {
		t.Fatalf("LastError after valid set = %v", s.LastError())
	}

	// The BlendableSurface method has no return value; diagnostics go to
	// LastError.
	s.SetBlendMode(BlendMode(99))
	if !errors.Is(s.LastError(), ErrUnknownBlendMode) {
		t.Errorf("LastError = %v, want ErrUnknownBlendMode", s.LastError())
	}
	if s.BlendMode() != BlendModeMultiply {
		t.Errorf("blend mode = %v, want Multiply", s.BlendMode())
	}

	// A subsequent successful set clears the recorded error.
	s.SetBlendMode(BlendModeCopy)
	if s.LastError() != nil {
		t.Errorf("LastError after valid set = %v, want nil", s.LastError())
	}
}

// --- 3. Closed surface ------------------------------------------------------

func TestImageSurfaceClosedState(t *testing.T) {
	s := NewImageSurface(4, 4)
	s.Clear(color.White)

	path := NewPath()
	path.Rectangle(0, 0, 4, 4)
	img := solidImage(2, 2, color.Black)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Drawing after Close must be a safe no-op.
	s.Clear(color.Black)
	s.Fill(path, FillStyle{Color: color.Black})
	s.Stroke(path, StrokeStyle{Color: color.Black, Width: 1})
	s.DrawImage(img, Pt(0, 0), nil)
	s.Save()
	s.Restore()

	if s.Snapshot() != nil {
		t.Error("Snapshot after Close should be nil")
	}
	if s.Image() != nil {
		t.Error("Image after Close should be nil")
	}
	if err := s.Flush(); err != nil {
		t.Errorf("Flush after Close = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}

	// State methods report the closed surface instead of silently succeeding.
	if err := s.TrySetBlendMode(BlendModeMultiply); !errors.Is(err, ErrSurfaceClosed) {
		t.Errorf("TrySetBlendMode after Close = %v, want ErrSurfaceClosed", err)
	}
	if err := s.SetOpacity(0.5); !errors.Is(err, ErrSurfaceClosed) {
		t.Errorf("SetOpacity after Close = %v, want ErrSurfaceClosed", err)
	}
}

// --- 3. NaN / Inf colors ----------------------------------------------------

// nonFiniteColor returns an aggressive color.Color whose RGBA values are
// outside the contract, as a color derived from NaN or Inf would be.
type nonFiniteColor struct{ v uint32 }

func (c nonFiniteColor) RGBA() (r, g, b, a uint32) {
	return c.v, c.v, c.v, c.v
}

func TestImageSurfaceNonFiniteColors(t *testing.T) {
	cases := []struct {
		name string
		c    color.Color
	}{
		{"max-uint32", nonFiniteColor{math.MaxUint32}},
		{"max-uint32-minus-one", nonFiniteColor{math.MaxUint32 - 1}},
		{"saturated-min", nonFiniteColor{1 << 20}},
		{"zero-alpha-bits", nonFiniteColor{0}},
		{"nan-float", color.RGBA64{R: 0, G: 0, B: 0, A: 0}},
	}

	for _, mode := range blendModes {
		for _, tc := range cases {
			t.Run(mode.String()+"/"+tc.name, func(t *testing.T) {
				s := NewImageSurface(4, 4)
				defer s.Close()
				s.Clear(colorFrom(40, 80, 120, 200))
				if err := s.TrySetBlendMode(mode); err != nil {
					t.Fatal(err)
				}

				path := NewPath()
				path.Rectangle(0, 0, 4, 4)
				s.Fill(path, FillStyle{Color: tc.c})
				s.Stroke(path, StrokeStyle{Color: tc.c, Width: 1})
				s.DrawImage(solidImage(2, 2, tc.c), Pt(1, 1), &DrawImageOptions{Alpha: math.NaN()})
				s.Clear(tc.c)

				// Whatever the color reduces to, every channel must stay inside
				// its legal range and premultiplied pixels must satisfy c <= a.
				snap := s.Snapshot()
				for y := 0; y < snap.Bounds().Dy(); y++ {
					for x := 0; x < snap.Bounds().Dx(); x++ {
						p := pixelAt(snap, x, y)
						if p.R > p.A || p.G > p.A || p.B > p.A {
							t.Fatalf("premultiplied invariant broken at (%d,%d): %+v", x, y, p)
						}
					}
				}
			})
		}
	}
}

func TestImageSurfaceNaNPathIsIgnored(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer s.Close()
	s.Clear(colorFrom(10, 20, 30, 255))
	before := pixelAt(s.Snapshot(), 4, 4)

	nan := math.NaN()
	paths := []struct {
		name string
		path *Path
	}{
		{"nan-move", func() *Path { p := NewPath(); p.MoveTo(nan, nan); p.LineTo(8, 8); p.Close(); return p }()},
		{"nan-cubic", func() *Path { p := NewPath(); p.MoveTo(0, 0); p.CubicTo(nan, 0, 0, nan, 8, 8); return p }()},
		{"nan-line-end", func() *Path { p := NewPath(); p.MoveTo(0, 0); p.LineTo(8, nan); return p }()},
		{"inf-rect", func() *Path { p := NewPath(); p.Rectangle(0, 0, math.Inf(1), math.Inf(1)); return p }()},
	}

	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			s.Fill(tc.path, FillStyle{Color: colorFrom(255, 0, 0, 255)})
			s.Stroke(tc.path, StrokeStyle{Color: colorFrom(255, 0, 0, 255), Width: 2})
			after := pixelAt(s.Snapshot(), 4, 4)
			if after != before {
				t.Errorf("non-finite path changed pixel: %+v -> %+v", before, after)
			}
		})
	}
}

// --- 3. Huge regions --------------------------------------------------------

func TestImageSurfaceHugePathDoesNotPanic(t *testing.T) {
	// Full-cover rectangles spanning orders of magnitude past the rasterizer's
	// fixed-point range, including float32 overflow territory.
	extents := []float64{
		float64(1 << 30),
		float64(1 << 14),
		-float64(1 << 30), // negative extent is degenerate but must be safe
		float64(1 << 40),
		3e38,
	}

	for _, big := range extents {
		t.Run("extent", func(t *testing.T) {
			s := NewImageSurface(16, 16)
			defer s.Close()
			s.Clear(colorFrom(5, 5, 5, 255))

			path := NewPath()
			path.Rectangle(-big, -big, 2*big, 2*big)
			s.Fill(path, FillStyle{Color: color.White})
			s.Stroke(path, StrokeStyle{Color: color.White, Width: 2})

			if big < 0 {
				// A negative width is an empty rectangle; nothing is drawn.
				return
			}
			// The whole surface must be covered, and the result must be exactly
			// white.
			snap := s.Snapshot()
			for y := 0; y < 16; y++ {
				for x := 0; x < 16; x++ {
					if p := pixelAt(snap, x, y); p != (premulRGBA{R: 255, G: 255, B: 255, A: 255}) {
						t.Fatalf("extent %g: pixel (%d,%d) = %+v, want opaque white", big, x, y, p)
					}
				}
			}
		})
	}
}

// TestImageSurfaceOffSurfaceHugeShapes checks that shapes placed far outside
// the surface leave it untouched, and shapes straddling the far edge only
// affect the overlap.
func TestImageSurfaceOffSurfaceHugeShapes(t *testing.T) {
	const size = 8

	tests := []struct {
		name          string
		rect          [4]float64
		wantAnyDrawn  bool
		wantAllFilled bool
	}{
		{"far-right", [4]float64{1 << 20, 0, 1 << 20, 8}, false, false},
		{"far-below", [4]float64{0, 1 << 20, 8, 1 << 20}, false, false},
		{"huge-below-right", [4]float64{1e9, 1e9, 1e9, 1e9}, false, false},
		{"ends-at-origin", [4]float64{-1 << 20, -1 << 20, 1 << 20, 1 << 20}, false, false},
		{"covers-everything", [4]float64{-1 << 20, -1 << 20, 2 << 20, 2 << 20}, true, true},
		{"straddles-left", [4]float64{-1e9, 0, 1e9 + size/2, size}, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewImageSurface(size, size)
			defer s.Close()
			s.Clear(colorFrom(0, 0, 0, 255))

			path := NewPath()
			path.Rectangle(tt.rect[0], tt.rect[1], tt.rect[2], tt.rect[3])
			s.Fill(path, FillStyle{Color: color.White})

			snap := s.Snapshot()
			white := 0
			for y := 0; y < size; y++ {
				for x := 0; x < size; x++ {
					if pixelAt(snap, x, y) == (premulRGBA{R: 255, G: 255, B: 255, A: 255}) {
						white++
					}
				}
			}
			if tt.wantAllFilled && white != size*size {
				t.Errorf("white pixels = %d, want %d", white, size*size)
			}
			if tt.wantAnyDrawn && white == 0 {
				t.Error("expected some pixels drawn, got none")
			}
			if !tt.wantAnyDrawn && white != 0 {
				t.Errorf("expected nothing drawn, got %d white pixels", white)
			}
		})
	}
}

func TestImageSurfaceHugeDrawImage(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer s.Close()
	s.Clear(colorFrom(0, 0, 0, 255))

	// A 1×1 image positioned at the far corner and beyond must only touch the
	// in-bounds intersection.
	img := solidImage(1, 1, colorFrom(255, 0, 0, 255))
	s.DrawImage(img, Pt(7, 7), nil)
	s.DrawImage(img, Pt(-100, -100), nil)
	s.DrawImage(img, Pt(math.MaxInt32, math.MaxInt32), nil)
	s.DrawImage(img, Pt(float64(math.MaxInt32)*4, 0), nil)

	snap := s.Snapshot()
	if p := pixelAt(snap, 7, 7); p != (premulRGBA{R: 255, A: 255}) {
		t.Errorf("(7,7) = %+v, want red", p)
	}
	if p := pixelAt(snap, 0, 0); p != (premulRGBA{A: 255}) {
		t.Errorf("(0,0) = %+v, want black", p)
	}
}

// --- 2. Bounds, out-of-bounds clipping -------------------------------------

// TestImageSurfaceDrawImageSourceOrigin verifies that an image whose Bounds do
// not start at the origin is mapped correctly: the top-left of its rectangle
// lands at the requested position, not its (0,0) pixel.
func TestImageSurfaceDrawImageSourceOrigin(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer s.Close()
	s.Clear(colorFrom(0, 0, 0, 255))

	img := image.NewNRGBA(image.Rect(10, 20, 14, 24))
	for y := 20; y < 24; y++ {
		for x := 10; x < 14; x++ {
			img.SetNRGBA(x, y, colorFrom(255, 0, 0, 255))
		}
	}
	img.SetNRGBA(11, 21, colorFrom(0, 255, 0, 255))

	s.DrawImage(img, Pt(2, 2), &DrawImageOptions{SrcRect: &img.Rect, Alpha: 1, Filter: FilterNearest})

	snap := s.Snapshot()
	if p := pixelAt(snap, 2, 2); p != (premulRGBA{R: 255, A: 255}) {
		t.Errorf("(2,2) = %+v, want red", p)
	}
	// Source (11,21) is 1,1 inside the source rect, so it lands at (3,3).
	if p := pixelAt(snap, 3, 3); p != (premulRGBA{G: 255, A: 255}) {
		t.Errorf("(3,3) = %+v, want green", p)
	}
	if p := pixelAt(snap, 6, 6); p != (premulRGBA{A: 255}) {
		t.Errorf("(6,6) = %+v, want untouched black", p)
	}
}

func TestImageSurfaceBoundsAndClipping(t *testing.T) {
	s := NewImageSurfaceFromImage(image.NewRGBA(image.Rect(10, 20, 20, 30)))
	defer s.Close()

	if s.Width() != 10 || s.Height() != 10 {
		t.Fatalf("size = %dx%d, want 10x10", s.Width(), s.Height())
	}

	s.Clear(colorFrom(0, 0, 0, 255))
	path := NewPath()
	path.Rectangle(0, 0, 5, 5)
	s.Fill(path, FillStyle{Color: color.White})

	snap := s.Snapshot()
	// Snapshot is in surface coordinates, independent of the backing image's
	// origin.
	if p := pixelAt(snap, 2, 2); p != (premulRGBA{R: 255, G: 255, B: 255, A: 255}) {
		t.Errorf("inside = %+v, want white", p)
	}
	if p := pixelAt(snap, 7, 7); p != (premulRGBA{A: 255}) {
		t.Errorf("outside = %+v, want black", p)
	}

	// A path entirely outside the surface touches nothing.
	path2 := NewPath()
	path2.Rectangle(100, 100, 10, 10)
	s.Fill(path2, FillStyle{Color: colorFrom(255, 0, 0, 255)})
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			p := pixelAt(s.Snapshot(), x, y)
			if p.R == 255 && p.G == 0 && p.B == 0 {
				t.Fatalf("out-of-bounds fill leaked into (%d,%d)", x, y)
			}
		}
	}
}

// --- 4. Anti-aliased coverage against the reference -------------------------

// TestImageSurfaceFillCoverageMatchesReference fills a rectangle with a
// fractional edge so the boundary pixels get partial coverage, and checks that
// every pixel matches the reference. The rasterizer's coverage is not exposed,
// so coverage is recovered from a fully opaque source-over fill of the same
// geometry: SourceOver at opacity 1 is C = coverage*S + (1-coverage)*D, which
// solves directly for coverage when S is opaque white over opaque black.
func TestImageSurfaceFillCoverageMatchesReference(t *testing.T) {
	const size = 16
	path := NewPath()
	path.Rectangle(3.25, 3.25, 9.5, 9.5) // fractional edges -> AA pixels

	// Determine coverage once with the readable source-over-of-white-over-black
	// configuration.
	covS := NewImageSurface(size, size)
	defer covS.Close()
	covS.Clear(colorFrom(0, 0, 0, 255))
	covS.Fill(path, FillStyle{Color: colorFrom(255, 255, 255, 255)})
	cov := covS.Snapshot()

	bg := colorFrom(20, 40, 200, 255)
	src := colorFrom(240, 30, 60, 160)

	for _, mode := range blendModes {
		t.Run(mode.String(), func(t *testing.T) {
			s := NewImageSurface(size, size)
			defer s.Close()
			s.Clear(bg)
			if err := s.TrySetBlendMode(mode); err != nil {
				t.Fatal(err)
			}
			if err := s.SetOpacity(0.8); err != nil {
				t.Fatal(err)
			}
			s.Fill(path, FillStyle{Color: src})

			snap := s.Snapshot()
			for y := 0; y < size; y++ {
				for x := 0; x < size; x++ {
					// White over opaque black leaves R = coverage*255, so the
					// red channel recovers the rasterizer's coverage.
					coverage := float64(pixelAt(cov, x, y).R) / 255
					want := referenceCoveredPixel(mode, src, coverage, 0.8, premulBytes(bg))
					got := pixelAt(snap, x, y)
					assertPixelNear(t, got, want,
						mode.String())
				}
			}
		})
	}
}

// --- 4. Image edge fixture against the reference ----------------------------

func TestImageSurfaceDrawImageEdges(t *testing.T) {
	for _, mode := range blendModes {
		t.Run(mode.String(), func(t *testing.T) {
			const size = 8
			s := NewImageSurface(size, size)
			defer s.Close()
			// Gradient backdrop so a misplaced blit shows up.
			for y := 0; y < size; y++ {
				for x := 0; x < size; x++ {
					s.img.SetRGBA(x, y, color.RGBA{
						R: uint8(16 * x), G: uint8(16 * y), B: 0x40, A: 0xff,
					})
				}
			}
			if err := s.TrySetBlendMode(mode); err != nil {
				t.Fatal(err)
			}

			// 3×3 source with a distinct alpha in each row. NRGBA keeps the
			// fixture's straight channels exact despite the low alpha.
			src := image.NewNRGBA(image.Rect(0, 0, 3, 3))
			for y := 0; y < 3; y++ {
				for x := 0; x < 3; x++ {
					src.SetNRGBA(x, y, color.NRGBA{
						R: 255, G: uint8(64 * x), B: 0, A: uint8(60 + 60*y),
					})
				}
			}

			// Positioned so it straddles the right and bottom edges.
			at := Pt(6, 6)
			alpha := 0.75
			s.DrawImage(src, at, &DrawImageOptions{SrcRect: &src.Rect, Alpha: alpha, Filter: FilterNearest})

			snap := s.Snapshot()
			for dy := 0; dy < size; dy++ {
				for dx := 0; dx < size; dx++ {
					got := pixelAt(snap, dx, dy)
					srcX := dx - int(at.X)
					srcY := dy - int(at.Y)
					var want premulRGBA
					if srcX >= 0 && srcX < 3 && srcY >= 0 && srcY < 3 {
						sc := src.NRGBAAt(srcX, srcY)
						want = referenceBlend(mode,
							scaleRef(refFromColor(sc), alpha),
							refFromColor(backdropAt(dx, dy)))
					} else {
						want = refToPremulBytes(refFromColor(backdropAt(dx, dy)))
					}
					assertPixelNear(t, got, want, "image edge")
				}
			}
		})
	}
}

// backdropAt reproduces the gradient backdrop used by TestImageSurfaceDrawImageEdges.
func backdropAt(x, y int) color.RGBA {
	return color.RGBA{R: uint8(16 * x), G: uint8(16 * y), B: 0x40, A: 0xff}
}

// scaleRef multiplies a straight color's alpha by f.
func scaleRef(c refColor, f float64) refColor {
	c.A *= f
	return c
}

// --- 3. Repeated configuration is stable -----------------------------------

func TestImageSurfaceRepeatedConfiguration(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer s.Close()

	// Repeating the same calls must converge, not accumulate.
	for i := 0; i < 5; i++ {
		if err := s.TrySetBlendMode(BlendModeMultiply); err != nil {
			t.Fatal(err)
		}
		if err := s.SetOpacity(0.5); err != nil {
			t.Fatal(err)
		}
	}
	if s.BlendMode() != BlendModeMultiply || s.Opacity() != 0.5 {
		t.Fatalf("state = %v/%v", s.BlendMode(), s.Opacity())
	}

	s.Clear(colorFrom(0, 0, 255, 255))
	path := NewPath()
	path.Rectangle(0, 0, 8, 8)
	src := colorFrom(255, 0, 0, 128)

	for i := 0; i < 3; i++ {
		s.Fill(path, FillStyle{Color: src})
	}

	// Three identical multiply fills must match three reference applications.
	want := premulBytes(colorFrom(0, 0, 255, 255))
	for i := 0; i < 3; i++ {
		want = referenceBlend(BlendModeMultiply,
			scaleRef(refFromColor(src), 0.5), refFromPremul(want))
	}
	assertPixelNear(t, pixelAt(s.Snapshot(), 4, 4), want, "repeated fill")
}

// TestImageSurfaceStateNotPollutedByBadInput verifies that rejected state
// changes, non-finite paths and huge regions do not leave the surface in a
// state that changes the next normal drawing operation.
func TestImageSurfaceStateNotPollutedByBadInput(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer s.Close()
	bg := colorFrom(0, 0, 255, 255)
	s.Clear(bg)
	_ = s.TrySetBlendMode(BlendModeMultiply)
	_ = s.SetOpacity(0.5)

	clean := NewImageSurface(8, 8)
	defer clean.Close()
	clean.Clear(bg)
	_ = clean.TrySetBlendMode(BlendModeMultiply)
	_ = clean.SetOpacity(0.5)

	// Provoke every failure and boundary case.
	_ = s.TrySetBlendMode(BlendMode(200))
	bad := NewPath()
	bad.MoveTo(math.NaN(), math.NaN())
	bad.LineTo(8, 8)
	s.Fill(bad, FillStyle{Color: color.White})
	huge := NewPath()
	huge.Rectangle(-1e9, -1e9, 2e9, 2e9)
	s.Fill(huge, FillStyle{Color: color.White})

	// The next normal operation must match a surface that never saw bad input.
	path := NewPath()
	path.Rectangle(2, 2, 4, 4)
	src := colorFrom(255, 0, 0, 200)
	s.Fill(path, FillStyle{Color: src})
	clean.Fill(path, FillStyle{Color: src})

	snap, want := s.Snapshot(), clean.Snapshot()
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if pixelAt(snap, x, y) != pixelAt(want, x, y) {
				t.Fatalf("pixel (%d,%d) differs after bad input: got %+v, want %+v",
					x, y, pixelAt(snap, x, y), pixelAt(want, x, y))
			}
		}
	}
}

// --- Stroke shares the blend path ------------------------------------------

func TestImageSurfaceStrokeBlendModes(t *testing.T) {
	bg := colorFrom(255, 255, 0, 255)
	src := colorFrom(0, 0, 255, 255)

	for _, mode := range blendModes {
		t.Run(mode.String(), func(t *testing.T) {
			s := NewImageSurface(32, 32)
			defer s.Close()
			s.Clear(bg)
			if err := s.TrySetBlendMode(mode); err != nil {
				t.Fatal(err)
			}
			path := NewPath()
			path.MoveTo(2, 16)
			path.LineTo(30, 16)
			s.Stroke(path, StrokeStyle{Color: src, Width: 8})

			got := pixelAt(s.Snapshot(), 16, 16)
			want := referenceBlend(mode, refFromColor(src), refFromColor(bg))
			assertPixelNear(t, got, want, "stroke "+mode.String())
		})
	}
}

// --- Capabilities -----------------------------------------------------------

func TestImageSurfaceCapabilitiesBlendModes(t *testing.T) {
	s := NewImageSurface(4, 4)
	defer s.Close()

	caps := s.Capabilities()
	if !caps.SupportsBlendModes {
		t.Error("ImageSurface should report SupportsBlendModes = true")
	}
	if !caps.SupportsAntialias {
		t.Error("ImageSurface should report SupportsAntialias = true")
	}

	// The concrete type must satisfy the optional interface the capability
	// advertises.
	var blendable BlendableSurface = s
	if blendable.BlendMode() != BlendModeSourceOver {
		t.Errorf("BlendMode() = %v", blendable.BlendMode())
	}
	blendable.SetBlendMode(BlendModeScreen)
	if blendable.BlendMode() != BlendModeScreen {
		t.Errorf("BlendMode() after set = %v", blendable.BlendMode())
	}
}

func TestBlendModeString(t *testing.T) {
	tests := []struct {
		mode BlendMode
		want string
	}{
		{BlendModeSourceOver, "SourceOver"},
		{BlendModeMultiply, "Multiply"},
		{BlendModeScreen, "Screen"},
		{BlendModeOverlay, "Overlay"},
		{BlendModeClear, "Clear"},
		{BlendModeCopy, "Copy"},
		{BlendMode(200), "Unknown"},
	}
	for _, tt := range tests {
		if got := tt.mode.String(); got != tt.want {
			t.Errorf("BlendMode(%d).String() = %q, want %q", uint8(tt.mode), got, tt.want)
		}
		if tt.mode.IsValid() != (tt.want != "Unknown") {
			t.Errorf("BlendMode(%d).IsValid() mismatch", uint8(tt.mode))
		}
	}
}

// --- Empty path / nil inputs -----------------------------------------------

func TestImageSurfaceEmptyAndNilInputs(t *testing.T) {
	s := NewImageSurface(4, 4)
	defer s.Close()
	s.Clear(colorFrom(9, 9, 9, 255))
	before := pixelAt(s.Snapshot(), 0, 0)

	s.Fill(nil, FillStyle{Color: color.White})
	s.Stroke(nil, StrokeStyle{Color: color.White, Width: 1})
	s.Fill(NewPath(), FillStyle{Color: color.White})
	s.Stroke(NewPath(), StrokeStyle{Color: color.White, Width: 1})
	s.Fill(NewPath(), DefaultFillStyle())
	s.DrawImage(nil, Pt(0, 0), nil)

	// A path containing only a MoveTo has no geometry.
	onlyMove := NewPath()
	onlyMove.MoveTo(2, 2)
	s.Fill(onlyMove, FillStyle{Color: color.White})
	s.Stroke(onlyMove, StrokeStyle{Color: color.White, Width: 2})

	// Zero-width stroke produces no outline.
	line := NewPath()
	line.MoveTo(0, 0)
	line.LineTo(4, 4)
	s.Stroke(line, StrokeStyle{Color: color.White, Width: 0})

	if after := pixelAt(s.Snapshot(), 0, 0); after != before {
		t.Errorf("empty input changed pixel: %+v -> %+v", before, after)
	}
}

// TestImageSurfaceNilColor verifies nil colors are handled without panicking.
// Fill/Stroke keep their documented fallback of opaque black, while Clear
// treats nil as transparent and erases the surface.
func TestImageSurfaceNilColor(t *testing.T) {
	s := NewImageSurface(2, 2)
	defer s.Close()
	s.Clear(colorFrom(7, 7, 7, 255))

	path := NewPath()
	path.Rectangle(0, 0, 2, 2)
	s.Fill(path, FillStyle{Color: nil})
	if p := pixelAt(s.Snapshot(), 0, 0); p != (premulRGBA{A: 255}) {
		t.Errorf("nil color fill = %+v, want opaque black fallback", p)
	}

	s.Clear(nil)
	if p := pixelAt(s.Snapshot(), 0, 0); p != (premulRGBA{}) {
		t.Errorf("Clear(nil) = %+v, want transparent", p)
	}
}

func TestImageSurfaceClearIgnoringBlendState(t *testing.T) {
	s := NewImageSurface(2, 2)
	defer s.Close()
	_ = s.TrySetBlendMode(BlendModeMultiply)
	_ = s.SetOpacity(0)

	// Clear resets the surface regardless of blend mode and opacity.
	s.Clear(colorFrom(1, 2, 3, 255))

	p := pixelAt(s.Snapshot(), 0, 0)
	if p != (premulRGBA{R: 1, G: 2, B: 3, A: 255}) {
		t.Errorf("Clear = %+v, want (1,2,3,255)", p)
	}
}
