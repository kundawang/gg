// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"errors"
	"image/color"
	"sync"
	"testing"
)

// TestImageSurfaceConcurrentUseAcrossSurfaces exercises the documented
// concurrency contract: an ImageSurface is not safe for concurrent use, but
// independent surfaces never share mutable state, so many goroutines each
// drawing on their own surface must be race-free.
//
// Run with -race to make this test meaningful.
func TestImageSurfaceConcurrentUseAcrossSurfaces(t *testing.T) {
	const workers = 8

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()

			s := NewImageSurface(32, 32)
			defer s.Close()

			// Reading the package-level blend mode values concurrently is
			// safe; only the surface's own state is per-goroutine.
			modes := []BlendMode{BlendModeSourceOver, BlendModeMultiply, BlendModeScreen, BlendModeOverlay, BlendModeClear, BlendModeCopy}
			path := NewPath()
			path.Rectangle(2, 2, 20, 20)

			for k := 0; k < 50; k++ {
				mode := modes[(i+k)%len(modes)]
				_ = s.TrySetBlendMode(mode)
				_ = s.SetOpacity(float64(k%5) / 4)
				s.Save()
				s.Fill(path, FillStyle{Color: color.NRGBA{R: uint8(k), G: 32, B: 64, A: 200}})
				s.Stroke(path, StrokeStyle{Color: color.NRGBA{R: 64, G: uint8(k), B: 0, A: 255}, Width: 2})
				s.Restore()
			}

			if s.Snapshot() == nil {
				t.Errorf("worker %d: Snapshot returned nil", i)
			}
		}(i)
	}
	wg.Wait()
}

// TestImageSurfaceConcurrentReadOnlyStateCheck verifies that the pure getters
// (BlendMode, Opacity, StateDepth, Capabilities, Width, Height) can be called
// concurrently with each other on a surface whose state is not being mutated,
// which callers rely on for read-only diagnostics.
func TestImageSurfaceConcurrentReadOnlyStateCheck(t *testing.T) {
	s := NewImageSurface(16, 16)
	defer s.Close()
	_ = s.TrySetBlendMode(BlendModeScreen)
	_ = s.SetOpacity(0.5)
	s.Save()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 100; k++ {
				if s.BlendMode() != BlendModeScreen {
					t.Errorf("BlendMode = %v", s.BlendMode())
					return
				}
				if s.Opacity() != 0.5 {
					t.Errorf("Opacity = %v", s.Opacity())
					return
				}
				if s.StateDepth() != 1 {
					t.Errorf("StateDepth = %d", s.StateDepth())
					return
				}
				if !s.Capabilities().SupportsBlendModes {
					t.Error("SupportsBlendModes = false")
					return
				}
				if s.Width() != 16 || s.Height() != 16 {
					t.Error("dimensions changed")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestImageSurfaceErrorsAreStable checks that repeated failing state calls keep
// returning the same diagnosable error without corrupting state.
func TestImageSurfaceErrorsAreStable(t *testing.T) {
	s := NewImageSurface(4, 4)
	defer s.Close()
	_ = s.TrySetBlendMode(BlendModeOverlay)

	for i := 0; i < 3; i++ {
		err := s.TrySetBlendMode(BlendMode(200))
		if !errors.Is(err, ErrUnknownBlendMode) {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if s.BlendMode() != BlendModeOverlay {
			t.Fatalf("iteration %d: blend mode = %v", i, s.BlendMode())
		}
	}
	if s.StateDepth() != 0 {
		t.Errorf("failed sets leaked state: depth = %d", s.StateDepth())
	}
}
