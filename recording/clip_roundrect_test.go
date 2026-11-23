package recording

import (
	"image"
	"math"
	"testing"

	"github.com/gogpu/gg"
)

// recordingClipBackend captures the world-space clip path Playback installs
// while hiding any optional rounded-rect interface, exercising the generic
// transformed-path fallback in transformedClipRoundRect.
type recordingClipBackend struct {
	playbackMockBackend
	clipPaths []*gg.Path
}

func (b *recordingClipBackend) SetClip(path *gg.Path, _ FillRule) {
	b.clipPaths = append(b.clipPaths, path)
}

// nativeClipRecordingBackend implements RRectClipBackend so Playback must
// deliver the user-space rounded-rect call instead of a transformed path.
type nativeClipRecordingBackend struct {
	playbackMockBackend
	nativeCalls [][5]float64
	clipCalls   int
}

func (b *nativeClipRecordingBackend) SetClip(*gg.Path, FillRule) {
	b.clipCalls++
}

func (b *nativeClipRecordingBackend) ClipRoundRect(x, y, w, h, radius float64) {
	b.nativeCalls = append(b.nativeCalls, [5]float64{x, y, w, h, radius})
}

var (
	_ Backend          = (*recordingClipBackend)(nil)
	_ RRectClipBackend = (*nativeClipRecordingBackend)(nil)
)

func playbackOnce(t *testing.T, rec *Recorder) *recordingClipBackend {
	t.Helper()
	backend := &recordingClipBackend{}
	if err := rec.FinishRecording().Playback(backend); err != nil {
		t.Fatalf("playback failed: %v", err)
	}
	return backend
}

// TestTransformedClipRoundRectFallbackGeometry verifies the fallback builds a
// device-space path with the exact transformed bounds of the rounded rect:
// rotation and shear must produce a true quadrilateral rather than an
// axis-aligned bounding box (the old parity bug).
func TestTransformedClipRoundRectFallbackGeometry(t *testing.T) {
	const size = 120
	cases := []struct {
		name string
		m    Matrix
	}{
		{"translation", Translate(15, 8)},
		{"uniform-scale", Translate(5, 5).Multiply(Scale(1.5, 1.5))},
		{"nonuniform-scale", Translate(5, 5).Multiply(Scale(1.5, 0.75))},
		{"rotation", Translate(60, 5).Multiply(Rotate(math.Pi / 6))},
		{"shear", Translate(5, 5).Multiply(Shear(0.6, 0.2))},
		{"negative-scale", Matrix{A: -1.5, E: -0.8, C: 90, F: 90}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := NewRecorder(size, size)
			rec.SetTransform(tc.m)
			rec.ClipRoundRect(20, 20, 40, 30, 12)
			backend := playbackOnce(t, rec)
			if len(backend.clipPaths) != 1 {
				t.Fatalf("got %d clip paths, want 1", len(backend.clipPaths))
			}

			// Reference: transform every user-space rounded-rect control
			// point and take its bounding box, exactly as Context does.
			userPath := gg.BuildPath().RoundRect(20, 20, 40, 30, 12).Build()
			wantPath := userPath.Transform(gg.Matrix{
				A: tc.m.A, B: tc.m.B, C: tc.m.C,
				D: tc.m.D, E: tc.m.E, F: tc.m.F,
			})
			if got, want := backend.clipPaths[0].Bounds(), wantPath.Bounds(); got != want {
				t.Errorf("clip path bounds = %v, want %v", got, want)
			}
		})
	}
}

// TestTransformedClipRoundRectFallbackDegenerate covers empty/degenerate
// geometry and non-finite matrices: Playback must not panic, must not install
// a clip with a huge or non-finite bounding box, and the clip must stay empty.
func TestTransformedClipRoundRectFallbackDegenerate(t *testing.T) {
	const size = 80
	cases := []struct {
		name       string
		m          Matrix
		x, y, w, h float64
		radius     float64
		wantEmpty  bool
	}{
		{"zero-width", Identity(), 10, 10, 0, 30, 8, true},
		{"zero-height", Identity(), 10, 10, 40, 0, 8, true},
		{"collapsed-scale", Matrix{E: 1}, 10, 10, 40, 30, 8, true},
		{"nan", Matrix{A: math.NaN(), E: 1}, 10, 10, 40, 30, 8, true},
		{"inf", Matrix{A: math.Inf(1), E: 1}, 10, 10, 40, 30, 8, true},
		{"nan-translate", Matrix{A: 1, E: 1, C: math.NaN()}, 10, 10, 40, 30, 8, true},
		{"valid", Translate(4, 6), 10, 10, 40, 30, 8, false},
		{"zero-radius-valid", Translate(4, 6), 10, 10, 40, 30, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := NewRecorder(size, size)
			rec.SetTransform(tc.m)
			rec.ClipRoundRect(tc.x, tc.y, tc.w, tc.h, tc.radius)
			backend := playbackOnce(t, rec)
			if len(backend.clipPaths) != 1 {
				t.Fatalf("got %d clip paths, want 1", len(backend.clipPaths))
			}
			bounds := backend.clipPaths[0].Bounds()
			if tc.wantEmpty {
				if !bounds.Empty() {
					t.Errorf("degenerate clip produced non-empty bounds %v", bounds)
				}
				return
			}
			if bounds.Empty() {
				t.Errorf("valid clip produced empty bounds")
			}
			if !rectIsFinite(bounds) {
				t.Errorf("clip bounds %v contain non-finite values", bounds)
			}
		})
	}
}

func rectIsFinite(r image.Rectangle) bool {
	for _, v := range []int{r.Min.X, r.Min.Y, r.Max.X, r.Max.Y} {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false
		}
	}
	return true
}

// TestTransformedClipRoundRectNativeBackendSelected proves a backend that
// implements RRectClipBackend receives the user-space call directly, with the
// recording transform still installed, instead of a transformed SetClip path.
func TestTransformedClipRoundRectNativeBackendSelected(t *testing.T) {
	backend := &nativeClipRecordingBackend{}

	rec := NewRecorder(60, 60)
	rec.Rotate(0.7)
	rec.ClipRoundRect(5, 5, 30, 20, 6)
	if err := rec.FinishRecording().Playback(backend); err != nil {
		t.Fatalf("playback failed: %v", err)
	}
	if len(backend.nativeCalls) != 1 {
		t.Fatalf("native ClipRoundRect calls = %d, want 1", len(backend.nativeCalls))
	}
	if got, want := backend.nativeCalls[0], [5]float64{5, 5, 30, 20, 6}; got != want {
		t.Errorf("native call = %v, want %v", got, want)
	}
	if backend.clipCalls != 0 {
		t.Errorf("SetClip calls = %d, want 0 (native path must be used)", backend.clipCalls)
	}
}
