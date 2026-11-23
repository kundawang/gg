package raster

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/recording"
)

func TestRecordingRasterClipSetClip(t *testing.T) {
	rec := recording.NewRecorder(100, 100)
	rec.Translate(10, 10)
	rec.SetRGB(1, 0, 0)
	rec.DrawRectangle(0, 0, 20, 20)
	rec.Clip()
	rec.FillRectangle(0, 0, 100, 100)

	img := playbackRaster(t, rec.FinishRecording())
	assertOpaqueRed(t, img, 15, 15)
	assertTransparent(t, img, 35, 35)
}

func TestRecordingRasterClipIntersectsSuccessiveClips(t *testing.T) {
	rec := recording.NewRecorder(100, 100)
	clipRecorderRect(rec, 10, 10, 80, 80)
	clipRecorderRect(rec, 40, 40, 30, 30)
	rec.SetRGB(1, 0, 0)
	rec.FillRectangle(0, 0, 100, 100)

	img := playbackRaster(t, rec.FinishRecording())
	assertOpaqueRed(t, img, 50, 50)
	assertTransparent(t, img, 20, 20)
	assertTransparent(t, img, 80, 80)
}

func TestRecordingRasterClipClearClip(t *testing.T) {
	rec := recording.NewRecorder(100, 100)
	clipRecorderRect(rec, 20, 20, 60, 60)
	rec.SetRGB(1, 0, 0)
	rec.FillRectangle(0, 0, 100, 100)
	rec.ResetClip()
	rec.SetRGB(0, 0, 1)
	rec.FillRectangle(0, 0, 20, 100)

	img := playbackRaster(t, rec.FinishRecording())
	assertOpaqueBlue(t, img, 10, 50)
	assertOpaqueRed(t, img, 50, 50)
	assertTransparent(t, img, 90, 50)
}

func TestRecordingRasterClipClearClipRestoredByRestore(t *testing.T) {
	rec := recording.NewRecorder(100, 100)
	clipRecorderRect(rec, 10, 10, 80, 80)
	rec.Save()
	rec.ResetClip()
	rec.Restore()
	rec.SetRGB(1, 0, 0)
	rec.FillRectangle(0, 0, 100, 100)

	img := playbackRaster(t, rec.FinishRecording())
	assertOpaqueRed(t, img, 50, 50)
	assertTransparent(t, img, 5, 5)
}

func TestRecordingRasterClipSaveRestore(t *testing.T) {
	rec := recording.NewRecorder(100, 100)
	clipRecorderRect(rec, 10, 10, 80, 80)
	rec.Save()
	clipRecorderRect(rec, 30, 30, 40, 40)
	rec.Restore()
	rec.SetRGB(1, 0, 0)
	rec.FillRectangle(0, 0, 20, 100)

	img := playbackRaster(t, rec.FinishRecording())
	assertOpaqueRed(t, img, 15, 50)
	assertTransparent(t, img, 50, 50)
	assertTransparent(t, img, 5, 5)
}

func TestRecordingRasterClipHonorsFillRule(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rule       recording.FillRule
		centerDraw bool
	}{
		{name: "non-zero", rule: recording.FillRuleNonZero, centerDraw: true},
		{name: "even-odd", rule: recording.FillRuleEvenOdd, centerDraw: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := recording.NewRecorder(100, 100)
			rec.SetFillRule(tc.rule)
			rec.DrawRectangle(10, 10, 80, 80)
			rec.DrawRectangle(30, 30, 40, 40)
			rec.Clip()
			rec.SetRGB(1, 0, 0)
			rec.FillRectangle(0, 0, 100, 100)

			img := playbackRaster(t, rec.FinishRecording())
			assertOpaqueRed(t, img, 15, 50)
			if tc.centerDraw {
				assertOpaqueRed(t, img, 50, 50)
			} else {
				assertTransparent(t, img, 50, 50)
			}
			assertTransparent(t, img, 5, 5)
		})
	}
}

func TestRecordingRasterClipRoundRectTransformParity(t *testing.T) {
	tests := []struct {
		name              string
		transformContext  func(*gg.Context)
		transformRecorder func(*recording.Recorder)
		points            []image.Point
	}{
		{
			name:              "translation",
			transformContext:  func(dc *gg.Context) { dc.Translate(15, 8) },
			transformRecorder: func(rec *recording.Recorder) { rec.Translate(15, 8) },
			points:            []image.Point{{X: 55, Y: 43}, {X: 25, Y: 35}},
		},
		{
			name: "scale",
			transformContext: func(dc *gg.Context) {
				dc.Translate(5, 5)
				dc.Scale(1.5, 0.75)
			},
			transformRecorder: func(rec *recording.Recorder) {
				rec.Translate(5, 5)
				rec.Scale(1.5, 0.75)
			},
			points: []image.Point{{X: 42, Y: 21}, {X: 60, Y: 50}},
		},
		{
			name: "rotation",
			transformContext: func(dc *gg.Context) {
				dc.Translate(60, 5)
				dc.Rotate(math.Pi / 6)
			},
			transformRecorder: func(rec *recording.Recorder) {
				rec.Translate(60, 5)
				rec.Rotate(math.Pi / 6)
			},
			points: []image.Point{{X: 74, Y: 33}, {X: 60, Y: 50}},
		},
		{
			name: "shear",
			transformContext: func(dc *gg.Context) {
				dc.Translate(5, 5)
				dc.Shear(0.6, 0.2)
			},
			transformRecorder: func(rec *recording.Recorder) {
				rec.Translate(5, 5)
				rec.Shear(0.6, 0.2)
			},
			points: []image.Point{{X: 52, Y: 30}, {X: 25, Y: 25}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			direct := gg.NewContext(120, 120)
			tc.transformContext(direct)
			direct.ClipRoundRect(20, 20, 40, 30, 12)
			direct.Identity()
			direct.SetRGB(1, 0, 0)
			direct.DrawRectangle(0, 0, 120, 120)
			if err := direct.Fill(); err != nil {
				t.Fatalf("direct fill failed: %v", err)
			}

			rec := recording.NewRecorder(120, 120)
			tc.transformRecorder(rec)
			rec.ClipRoundRect(20, 20, 40, 30, 12)
			rec.Identity()
			rec.SetRGB(1, 0, 0)
			rec.FillRectangle(0, 0, 120, 120)
			recorded := playbackRaster(t, rec.FinishRecording())

			for _, point := range tc.points {
				want := color.NRGBAModel.Convert(direct.Image().At(point.X, point.Y)).(color.NRGBA).A
				got := color.NRGBAModel.Convert(recorded.At(point.X, point.Y)).(color.NRGBA).A
				if got != want {
					t.Errorf("alpha at %v = %#02x, want direct alpha %#02x", point, got, want)
				}
			}
		})
	}
}

func TestRecordingRasterClipDisjointSubpaths(t *testing.T) {
	for _, rule := range []recording.FillRule{recording.FillRuleNonZero, recording.FillRuleEvenOdd} {
		name := "non-zero"
		if rule == recording.FillRuleEvenOdd {
			name = "even-odd"
		}
		t.Run(name, func(t *testing.T) {
			rec := recording.NewRecorder(100, 100)
			rec.SetFillRule(rule)
			rec.DrawRectangle(10, 10, 20, 20)
			rec.DrawRectangle(70, 70, 20, 20)
			rec.Clip()
			rec.SetRGB(1, 0, 0)
			rec.FillRectangle(0, 0, 100, 100)

			img := playbackRaster(t, rec.FinishRecording())
			assertOpaqueRed(t, img, 20, 20)
			assertOpaqueRed(t, img, 80, 80)
			assertTransparent(t, img, 50, 50)
		})
	}
}

// pathOnlyBackend hides the optional recording.RRectClipBackend interface of
// the raster backend, forcing Playback to use the generic transformed-path
// fallback so its geometry stays covered by the parity tests too.
type pathOnlyBackend struct {
	recording.Backend
}

func newPathOnlyBackend() recording.Backend {
	return &pathOnlyBackend{Backend: NewBackend()}
}

// Image delegates to the embedded raster backend; the wrapper deliberately
// adds no other methods so RRectClipBackend stays hidden.
func (b *pathOnlyBackend) Image() image.Image {
	return b.Backend.(*Backend).Image()
}

// alphaBounds returns the bounding box of all pixels with non-zero alpha.
func alphaBounds(img image.Image) image.Rectangle {
	b := img.Bounds()
	minX, minY := b.Max.X, b.Max.Y
	maxX, maxY := b.Min.X-1, b.Min.Y-1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			nrgba := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if nrgba.A == 0 {
				continue
			}
			if x < minX {
				minX = x
			}
			if y < minY {
				minY = y
			}
			if x > maxX {
				maxX = x
			}
			if y > maxY {
				maxY = y
			}
		}
	}
	if maxX < minX {
		return image.Rectangle{}
	}
	return image.Rect(minX, minY, maxX+1, maxY+1)
}

// maxAlphaDiff reports the largest per-pixel alpha difference and the number
// of differing pixels between two equally sized images.
func maxAlphaDiff(a, b image.Image) (int, int) {
	bounds := a.Bounds()
	maxDiff, mismatched := 0, 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			ca := color.NRGBAModel.Convert(a.At(x, y)).(color.NRGBA)
			cb := color.NRGBAModel.Convert(b.At(x, y)).(color.NRGBA)
			d := int(ca.A) - int(cb.A)
			if d < 0 {
				d = -d
			}
			if d > maxDiff {
				maxDiff = d
			}
			if d != 0 {
				mismatched++
			}
		}
	}
	return maxDiff, mismatched
}

// grossAlphaMismatch counts pixels where one image is fully transparent while
// the other is mostly opaque — a filled-in transparent region or a missed
// interior pixel, as opposed to an anti-aliasing difference on the boundary.
func grossAlphaMismatch(a, b image.Image) int {
	const opaqueThreshold = 0x80
	bounds := a.Bounds()
	gross := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			ca := color.NRGBAModel.Convert(a.At(x, y)).(color.NRGBA).A
			cb := color.NRGBAModel.Convert(b.At(x, y)).(color.NRGBA).A
			if (ca == 0 && cb >= opaqueThreshold) || (cb == 0 && ca >= opaqueThreshold) {
				gross++
			}
		}
	}
	return gross
}

// assertClipParity applies the assertion tier appropriate to the playback
// path:
//   - the native backend calls the same gg.Context code as the reference draw,
//     so every pixel alpha must match exactly;
//   - the generic transformed-path fallback rasterizes through the internal
//     clip mask rasterizer, whose edge anti-aliasing differs slightly from the
//     main rasterizer. It must still produce identical coverage Bounds with no
//     filled-in or missed interior pixels (only boundary AA may differ).
func assertClipParity(t *testing.T, want, got image.Image, native bool) {
	t.Helper()
	if native {
		if d, n := maxAlphaDiff(want, got); d != 0 || n != 0 {
			t.Errorf("alpha mismatch: maxDiff=%d differingPixels=%d", d, n)
		}
	} else if gross := grossAlphaMismatch(want, got); gross != 0 {
		t.Errorf("fallback clip has %d gross (fill/leak) mismatches", gross)
	}
	if gb, wb := alphaBounds(got), alphaBounds(want); !gb.Eq(wb) {
		t.Errorf("alpha Bounds = %v, want %v", gb, wb)
	}
}

// directRoundRectClip renders the canonical reference: a direct gg.Context
// with the transform applied, a rounded-rect clip, then an identity-space
// opaque fill of the whole canvas.
func directRoundRectClip(size int, m gg.Matrix, x, y, w, h, radius float64) image.Image {
	direct := gg.NewContext(size, size)
	direct.SetTransform(m)
	direct.ClipRoundRect(x, y, w, h, radius)
	direct.Identity()
	direct.SetRGB(1, 0, 0)
	direct.DrawRectangle(0, 0, float64(size), float64(size))
	if err := direct.Fill(); err != nil {
		panic(err)
	}
	return direct.Image()
}

// recordedRoundRectClip renders the same scene through Recorder + the given
// backend factory.
func recordedRoundRectClip(t *testing.T, size int, m gg.Matrix, x, y, w, h, radius float64,
	newBackend func() recording.Backend) image.Image {
	t.Helper()
	rec := recording.NewRecorder(size, size)
	rec.SetTransform(recording.Matrix{A: m.A, B: m.B, C: m.C, D: m.D, E: m.E, F: m.F})
	rec.ClipRoundRect(x, y, w, h, radius)
	rec.Identity()
	rec.SetRGB(1, 0, 0)
	rec.FillRectangle(0, 0, float64(size), float64(size))
	backend := newBackend()
	if err := rec.FinishRecording().Playback(backend); err != nil {
		t.Fatalf("playback failed: %v", err)
	}
	return backend.(interface{ Image() image.Image }).Image()
}

// TestRecordingRasterClipRoundRectPixelParity compares the full rendered alpha
// channel of direct draws and recorded playback across scale, rotation, shear,
// negative scale and non-zero origin, using both the native rounded-rect clip
// path and the generic transformed-path fallback. Geometry and Bounds must be
// byte-identical, not merely approximately close.
func TestRecordingRasterClipRoundRectPixelParity(t *testing.T) {
	const size = 120
	transforms := []struct {
		name string
		m    gg.Matrix
	}{
		{"translation", gg.Matrix{A: 1, E: 1, C: 15, F: 8}},
		{"uniform-scale", gg.Matrix{A: 1.5, E: 1.5, C: 5, F: 5}},
		{"nonuniform-scale", gg.Matrix{A: 1.5, E: 0.75, C: 5, F: 5}},
		{"rotation-30", gg.Matrix{
			A: math.Cos(math.Pi / 6), B: -math.Sin(math.Pi / 6),
			D: math.Sin(math.Pi / 6), E: math.Cos(math.Pi / 6),
			C: 60, F: 5,
		}},
		{"rotation-45", gg.Matrix{
			A: math.Sqrt2 / 2, B: -math.Sqrt2 / 2,
			D: math.Sqrt2 / 2, E: math.Sqrt2 / 2,
			C: 60, F: 60,
		}},
		{"shear", gg.Matrix{A: 1, B: 0.6, D: 0.2, E: 1, C: 5, F: 5}},
		{"extreme-shear", gg.Matrix{A: 1, B: 4.0, D: -2.5, E: 1, C: 10, F: 60}},
		{"negative-scale", gg.Matrix{A: -1.5, E: -0.8, C: 90, F: 90}},
		{"reflection-x", gg.Matrix{A: -1, E: 1, C: 95, F: 5}},
		{"chained-transform", gg.Matrix{
			A: 1.2, B: 0.35, C: 22,
			D: -0.25, E: 0.9, F: 14,
		}},
	}
	backends := []struct {
		name    string
		factory func() recording.Backend
	}{
		{"native", func() recording.Backend { return NewBackend() }},
		{"path-fallback", newPathOnlyBackend},
	}

	for _, tc := range transforms {
		// Radius smaller than half the min dimension exercises curved
		// corners; a second case exercises the radius clamp.
		for _, radius := range []float64{12, 60} {
			direct := directRoundRectClip(size, tc.m, 20, 20, 40, 30, radius)
			for _, be := range backends {
				name := fmt.Sprintf("%s/r%g/%s", tc.name, radius, be.name)
				t.Run(name, func(t *testing.T) {
					got := recordedRoundRectClip(t, size, tc.m, 20, 20, 40, 30, radius, be.factory)
					assertClipParity(t, direct, got, be.name == "native")
				})
			}
		}
	}
}

// TestRecordingRasterClipRoundRectSaveRestoreAndIntersect covers nested
// Save/Restore and multiple intersecting clips: the transform must never leak
// into subsequent commands, and restored clips must match a direct context.
func TestRecordingRasterClipRoundRectSaveRestoreAndIntersect(t *testing.T) {
	const size = 120
	buildDirect := func() image.Image {
		dc := gg.NewContext(size, size)
		dc.Translate(10, 10)
		dc.ClipRoundRect(5, 5, 90, 90, 20)
		dc.Push()
		dc.Rotate(math.Pi / 7)
		dc.Scale(1.2, 0.9)
		dc.ClipRoundRect(15, 15, 55, 45, 10)
		dc.Pop()
		// After Pop the transform is back to translation only; the nested
		// clip must remain in effect and the transform must not accumulate.
		dc.Identity()
		dc.SetRGB(0, 0.5, 1)
		dc.DrawRectangle(0, 0, size, size)
		if err := dc.Fill(); err != nil {
			panic(err)
		}
		return dc.Image()
	}
	buildRecorded := func(t *testing.T, factory func() recording.Backend) image.Image {
		t.Helper()
		rec := recording.NewRecorder(size, size)
		rec.Translate(10, 10)
		rec.ClipRoundRect(5, 5, 90, 90, 20)
		rec.Save()
		rec.Rotate(math.Pi / 7)
		rec.Scale(1.2, 0.9)
		rec.ClipRoundRect(15, 15, 55, 45, 10)
		rec.Restore()
		rec.Identity()
		rec.SetRGB(0, 0.5, 1)
		rec.FillRectangle(0, 0, size, size)
		backend := factory()
		if err := rec.FinishRecording().Playback(backend); err != nil {
			t.Fatalf("playback failed: %v", err)
		}
		return backend.(interface{ Image() image.Image }).Image()
	}

	want := buildDirect()
	for _, be := range []struct {
		name    string
		factory func() recording.Backend
	}{
		{"native", func() recording.Backend { return NewBackend() }},
		{"path-fallback", newPathOnlyBackend},
	} {
		t.Run(be.name, func(t *testing.T) {
			got := buildRecorded(t, be.factory)
			assertClipParity(t, want, got, be.name == "native")
		})
	}
}

// TestRecordingRasterClipRoundRectTransformDoesNotLeak verifies that playback
// restores the recording transform after installing a clip, so a later fill
// lands in the same place as a direct context draw.
func TestRecordingRasterClipRoundRectTransformDoesNotLeak(t *testing.T) {
	const size = 100
	m := gg.Matrix{
		A: math.Cos(0.5), B: -math.Sin(0.5),
		D: math.Sin(0.5), E: math.Cos(0.5),
		C: 50, F: 50,
	}

	direct := gg.NewContext(size, size)
	direct.SetTransform(m)
	direct.ClipRoundRect(-20, -15, 40, 30, 8)
	direct.DrawRectangle(-30, -30, 60, 60)
	if err := direct.Fill(); err != nil {
		t.Fatalf("direct fill: %v", err)
	}

	rec := recording.NewRecorder(size, size)
	rec.SetTransform(recording.Matrix{A: m.A, B: m.B, C: m.C, D: m.D, E: m.E, F: m.F})
	rec.ClipRoundRect(-20, -15, 40, 30, 8)
	rec.DrawRectangle(-30, -30, 60, 60)
	rec.Fill()
	backend := NewBackend()
	if err := rec.FinishRecording().Playback(backend); err != nil {
		t.Fatalf("playback: %v", err)
	}
	if d, n := maxAlphaDiff(direct.Image(), backend.Image()); d != 0 || n != 0 {
		t.Errorf("clip leaked transform: maxDiff=%d differingPixels=%d", d, n)
	}
}

// TestRecordingRasterClipRoundRectDegenerate checks empty regions, zero and
// clamped radii, non-finite matrices, extreme shear and ResetClip: playback
// must stay stable (no panic, no out-of-bounds writes, no painted pixels in a
// collapsed clip) and match the direct context result including Bounds.
func TestRecordingRasterClipRoundRectDegenerate(t *testing.T) {
	const size = 80
	cases := []struct {
		name   string
		m      gg.Matrix
		x, y   float64
		w, h   float64
		radius float64
	}{
		{"zero-radius", gg.Matrix{A: 1.4, E: 1.1, C: 4, F: 6}, 10, 10, 40, 30, 0},
		{"negative-radius", gg.Matrix{A: 1.4, E: 1.1, C: 4, F: 6}, 10, 10, 40, 30, -5},
		{"negative-dims", gg.Matrix{A: 1, E: 1, C: 5, F: 5}, 50, 40, -40, -30, 8},
		{"zero-width", gg.Matrix{A: 1, E: 1}, 10, 10, 0, 30, 8},
		{"zero-height", gg.Matrix{A: 1, E: 1}, 10, 10, 40, 0, 8},
		{"collapsed-scale", gg.Matrix{A: 0, E: 1}, 10, 10, 40, 30, 8},
		{"nan-matrix", gg.Matrix{A: math.NaN(), E: 1}, 10, 10, 40, 30, 8},
		{"inf-matrix", gg.Matrix{A: math.Inf(1), E: 1}, 10, 10, 40, 30, 8},
		{"nan-translate", gg.Matrix{A: 1, E: 1, C: math.NaN()}, 10, 10, 40, 30, 8},
		{"extreme-shear", gg.Matrix{A: 1, B: 1e12, D: 0.3, E: 1}, 10, 10, 40, 30, 8},
		{"huge-radius-clamped", gg.Matrix{A: 1, E: 1, C: 4, F: 4}, 10, 10, 30, 20, 1e6},
	}

	for _, tc := range cases {
		for _, be := range []struct {
			name    string
			factory func() recording.Backend
		}{
			{"native", func() recording.Backend { return NewBackend() }},
			{"path-fallback", newPathOnlyBackend},
		} {
			t.Run(tc.name+"/"+be.name, func(t *testing.T) {
				direct := directRoundRectClip(size, tc.m, tc.x, tc.y, tc.w, tc.h, tc.radius)
				got := recordedRoundRectClip(t, size, tc.m, tc.x, tc.y, tc.w, tc.h, tc.radius, be.factory)
				assertClipParity(t, direct, got, be.name == "native")
			})
		}
	}
}

// TestRecordingRasterClipRoundRectResetClip verifies ResetClip restores the
// full drawable surface after a transformed rounded-rect clip.
func TestRecordingRasterClipRoundRectResetClip(t *testing.T) {
	const size = 100
	m := gg.Matrix{
		A: math.Cos(math.Pi / 8), B: -math.Sin(math.Pi / 8),
		D: math.Sin(math.Pi / 8), E: math.Cos(math.Pi / 8),
		C: 50, F: 50,
	}

	direct := gg.NewContext(size, size)
	direct.SetTransform(m)
	direct.ClipRoundRect(-20, -20, 40, 40, 10)
	direct.ResetClip()
	direct.Identity()
	direct.SetRGB(1, 0, 0)
	direct.DrawRectangle(0, 0, size, size)
	if err := direct.Fill(); err != nil {
		t.Fatalf("direct fill: %v", err)
	}

	rec := recording.NewRecorder(size, size)
	rec.SetTransform(recording.Matrix{A: m.A, B: m.B, C: m.C, D: m.D, E: m.E, F: m.F})
	rec.ClipRoundRect(-20, -20, 40, 40, 10)
	rec.ResetClip()
	rec.Identity()
	rec.SetRGB(1, 0, 0)
	rec.FillRectangle(0, 0, size, size)
	backend := NewBackend()
	if err := rec.FinishRecording().Playback(backend); err != nil {
		t.Fatalf("playback: %v", err)
	}
	if d, n := maxAlphaDiff(direct.Image(), backend.Image()); d != 0 || n != 0 {
		t.Errorf("ResetClip mismatch: maxDiff=%d differingPixels=%d", d, n)
	}
	if bounds := alphaBounds(backend.Image()); !bounds.Eq(image.Rect(0, 0, size, size)) {
		t.Errorf("bounds after ResetClip = %v, want full canvas", bounds)
	}
}

func clipRecorderRect(rec *recording.Recorder, x, y, w, h float64) {
	rec.DrawRectangle(x, y, w, h)
	rec.Clip()
}

func playbackRaster(t *testing.T, rec *recording.Recording) image.Image {
	t.Helper()
	backend := NewBackend()
	if err := rec.Playback(backend); err != nil {
		t.Fatalf("recording playback failed: %v", err)
	}
	return backend.Image()
}

func assertOpaqueRed(t *testing.T, img image.Image, x, y int) {
	t.Helper()
	r, g, b, a := img.At(x, y).RGBA()
	if a < 0xc000 || r < 0xc000 || g > 0x1000 || b > 0x1000 {
		t.Errorf("pixel at (%d,%d) = %#04x %#04x %#04x %#04x, want opaque red", x, y, r, g, b, a)
	}
}

func assertOpaqueBlue(t *testing.T, img image.Image, x, y int) {
	t.Helper()
	r, g, b, a := img.At(x, y).RGBA()
	if a < 0xc000 || r > 0x1000 || g > 0x1000 || b < 0xc000 {
		t.Errorf("pixel at (%d,%d) = %#04x %#04x %#04x %#04x, want opaque blue", x, y, r, g, b, a)
	}
}

func assertTransparent(t *testing.T, img image.Image, x, y int) {
	t.Helper()
	rgba := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
	if rgba.A > 0x10 {
		t.Errorf("pixel at (%d,%d) has alpha %#02x, want transparent", x, y, rgba.A)
	}
}
