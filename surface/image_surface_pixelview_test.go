// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"errors"
	"image"
	"image/color"
	"sync"
	"testing"
)

// --- Non-zero origin correctness (the original Fill/Stroke panic bug) ---

// TestPixelViewNonZeroOriginFillStroke verifies that Fill and Stroke write
// through the image origin instead of treating (0,0) as the first pixel.
func TestPixelViewNonZeroOriginFillStroke(t *testing.T) {
	const ox, oy, w, h = 5, 7, 24, 20
	img := image.NewRGBA(image.Rect(ox, oy, ox+w, oy+h))
	s := NewImageSurfaceFromImage(img)
	defer func() { _ = s.Close() }()

	if s.Width() != w || s.Height() != h {
		t.Fatalf("size = %dx%d, want %dx%d", s.Width(), s.Height(), w, h)
	}

	s.Clear(color.RGBA{R: 230, G: 230, B: 230, A: 255})

	// Fill a rectangle fully inside the surface and verify exact placement
	// relative to the backing image origin.
	p := NewPath()
	p.Rectangle(2, 3, 10, 8)
	s.Fill(p, FillStyle{Color: color.RGBA{R: 255, A: 255}})

	if c := img.RGBAAt(ox+5, oy+5); c.R != 255 || c.A != 255 {
		t.Errorf("filled pixel at origin-offset = %v, want opaque red", c)
	}
	// A point just outside the filled rectangle must keep the clear color.
	if c := img.RGBAAt(ox+1, oy+2); c.R != 230 || c.G != 230 {
		t.Errorf("outside pixel = %v, want clear color", c)
	}

	// Stroke must also honor the origin.
	sp := NewPath()
	sp.MoveTo(1, 18)
	sp.LineTo(22, 18)
	s.Stroke(sp, StrokeStyle{Color: color.RGBA{B: 255, A: 255}, Width: 3})
	if c := img.RGBAAt(ox+10, oy+18); c.B == 0 {
		t.Error("stroke did not write through the non-zero origin")
	}

	// Snapshot must be zero-origin, tightly packed, and match the backing.
	snap := s.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot returned nil")
	}
	if b := snap.Bounds(); b.Min != (image.Point{}) || b.Dx() != w || b.Dy() != h {
		t.Errorf("snapshot bounds = %v, want zero-origin %dx%d", b, w, h)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if snap.RGBAAt(x, y) != img.RGBAAt(ox+x, oy+y) {
				t.Fatalf("snapshot mismatch at (%d,%d)", x, y)
			}
		}
	}
}

// TestPixelViewNonZeroOriginDrawImage verifies DrawImage destination
// coordinates are surface-local even with a non-zero image origin.
func TestPixelViewNonZeroOriginDrawImage(t *testing.T) {
	const ox, oy = 8, 3
	img := image.NewRGBA(image.Rect(ox, oy, ox+12, oy+9))
	s := NewImageSurfaceFromImage(img)
	defer func() { _ = s.Close() }()

	src := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for i := range src.Pix {
		src.Pix[i] = 0
	}
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			src.SetRGBA(x, y, color.RGBA{R: 12, G: 34, B: 56, A: 255})
		}
	}
	s.DrawImage(src, Pt(2, 1), nil)

	if c := img.RGBAAt(ox+2, oy+1); c.R != 12 || c.G != 34 || c.B != 56 || c.A != 255 {
		t.Errorf("drawn pixel at origin-offset = %v, want (12,34,56,255)", c)
	}
	// One pixel before the drawn region must remain untouched (zero value).
	if c := img.RGBAAt(ox+1, oy+1); c.A != 0 {
		t.Errorf("pixel outside draw = %v, want untouched", c)
	}
}

// TestPixelViewNonZeroOriginNoPanic reproduces the pre-refactor panic:
// Fill on a non-zero-origin surface used to index Pix out of range.
func TestPixelViewNonZeroOriginNoPanic(t *testing.T) {
	img := image.NewRGBA(image.Rect(100, 200, 120, 220))
	s := NewImageSurfaceFromImage(img)
	defer func() { _ = s.Close() }()

	p := NewPath()
	p.Circle(10, 10, 8)
	s.Fill(p, FillStyle{Color: color.RGBA{R: 255, A: 255}}) // must not panic
}

// --- Construction errors surfaced through Flush ---

func TestPixelViewFromNilImage(t *testing.T) {
	s := NewImageSurfaceFromImage(nil)
	defer func() { _ = s.Close() }()

	if err := s.Flush(); !errors.Is(err, errNilImage) {
		t.Errorf("Flush error = %v, want errNilImage", err)
	}
	// All operations must be safe no-ops.
	s.Clear(color.White)
	s.Fill(NewPath(), DefaultFillStyle())
	s.Stroke(NewPath(), DefaultStrokeStyle())
	s.DrawImage(image.NewRGBA(image.Rect(0, 0, 2, 2)), Pt(0, 0), nil)
	if s.Snapshot() != nil {
		t.Error("Snapshot on invalid surface should be nil")
	}
	if s.Image() != nil {
		t.Error("Image on invalid surface should be nil")
	}
	if s.Width() != 0 || s.Height() != 0 {
		t.Errorf("invalid surface size = %dx%d, want 0x0", s.Width(), s.Height())
	}
}

func TestPixelViewFromEmptyImage(t *testing.T) {
	s := NewImageSurfaceFromImage(image.NewRGBA(image.Rect(0, 0, 0, 0)))
	defer func() { _ = s.Close() }()
	if err := s.Flush(); !errors.Is(err, errEmptyImage) {
		t.Errorf("Flush error = %v, want errEmptyImage", err)
	}
}

func TestPixelViewInvalidStride(t *testing.T) {
	// Stride too small for the declared width.
	img := &image.RGBA{
		Pix:    make([]uint8, 4*8),
		Stride: 8, // only 2 pixels per row, but Rect claims 4
		Rect:   image.Rect(0, 0, 4, 2),
	}
	s := NewImageSurfaceFromImage(img)
	defer func() { _ = s.Close() }()
	if err := s.Flush(); !errors.Is(err, errInvalidStride) {
		t.Errorf("Flush error = %v, want errInvalidStride", err)
	}
	s.Clear(color.Black) // must not panic and must not write
	if img.Pix[0] != 0 {
		t.Error("invalid surface wrote to backing storage")
	}
}

func TestPixelViewPixTooSmall(t *testing.T) {
	// Valid stride but Pix cannot hold the declared rectangle.
	img := &image.RGBA{
		Pix:    make([]uint8, 4*4),
		Stride: 16,
		Rect:   image.Rect(0, 0, 4, 4), // needs 64 bytes, has 16
	}
	s := NewImageSurfaceFromImage(img)
	defer func() { _ = s.Close() }()
	if err := s.Flush(); !errors.Is(err, errPixTooSmall) {
		t.Errorf("Flush error = %v, want errPixTooSmall", err)
	}
}

func TestPixelViewPaddedStrideWorks(t *testing.T) {
	w, h := 6, 4
	stride := 4*w + 20
	pix := make([]uint8, stride*h)
	img := &image.RGBA{Pix: pix, Stride: stride, Rect: image.Rect(0, 0, w, h)}
	s := NewImageSurfaceFromImage(img)
	defer func() { _ = s.Close() }()

	if err := s.Flush(); err != nil {
		t.Fatalf("unexpected Flush error: %v", err)
	}
	s.Clear(color.RGBA{R: 7, G: 8, B: 9, A: 255})

	// Padding bytes between rows must never be touched.
	for y := 0; y < h; y++ {
		for p := 4 * w; p < stride; p++ {
			if pix[y*stride+p] != 0 {
				t.Fatalf("padding byte at row %d offset %d was written", y, p)
			}
		}
	}
	// Snapshot must be tightly packed (no padding) and match the pixels.
	snap := s.Snapshot()
	if snap.Stride != 4*w {
		t.Errorf("snapshot stride = %d, want %d", snap.Stride, 4*w)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if c := snap.RGBAAt(x, y); c != (color.RGBA{R: 7, G: 8, B: 9, A: 255}) {
				t.Fatalf("snapshot pixel (%d,%d) = %v", x, y, c)
			}
		}
	}
}

// --- Snapshot isolation ---

func TestPixelViewSnapshotIsIndependent(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer func() { _ = s.Close() }()
	s.Clear(color.RGBA{R: 1, G: 2, B: 3, A: 255})

	snap := s.Snapshot()
	snap.Pix[0] = 99
	s.Clear(color.RGBA{R: 9, G: 9, B: 9, A: 255})

	// The snapshot's own backing must be unaffected by later surface draws.
	if snap.Pix[0] != 99 {
		t.Error("snapshot pixels alias the surface backing storage")
	}
	// And the surface must not see snapshot mutations.
	s2 := s.Snapshot()
	if s2.Pix[0] != 9 {
		t.Error("surface observed mutation through snapshot alias")
	}
}

// --- Aliased source image ---

// TestPixelViewDrawImageAliasedSource draws the surface's own backing image
// back onto itself with an offset. The pre-refactor code read pixels as it
// overwrote them, producing gradient-like corruption; the refactor must
// snapshot the source first and produce a clean translated copy.
func TestPixelViewDrawImageAliasedSource(t *testing.T) {
	s := NewImageSurface(16, 8)
	defer func() { _ = s.Close() }()

	backing := s.Image()
	// Left half: opaque red; right half: transparent.
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			backing.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}

	// Draw the backing image onto itself shifted right by 4 pixels.
	s.DrawImage(backing, Pt(4, 0), nil)

	// Destination columns 4..11 must now be exactly the original left-half
	// pattern (red), not a smeared gradient.
	for y := 0; y < 8; y++ {
		for x := 4; x < 12; x++ {
			c := backing.RGBAAt(x, y)
			if c.R != 255 || c.A != 255 || c.G != 0 || c.B != 0 {
				t.Fatalf("aliased blit corrupted pixel (%d,%d) = %v", x, y, c)
			}
		}
	}
}

// TestPixelViewDrawImageAliasedSubImage uses a sub-image sharing part of the
// backing storage, exercising overlap detection on sub-slices.
func TestPixelViewDrawImageAliasedSubImage(t *testing.T) {
	s := NewImageSurface(12, 10)
	defer func() { _ = s.Close() }()
	s.Clear(color.RGBA{R: 20, G: 30, B: 40, A: 255})

	backing := s.Image()
	sub := backing.SubImage(image.Rect(0, 0, 6, 5)).(*image.RGBA)
	// Must not panic and must produce a stable result; run twice and compare.
	s.DrawImage(sub, Pt(4, 3), nil)
	first := append([]uint8(nil), backing.Pix...)
	s.DrawImage(sub, Pt(4, 3), nil)
	if len(first) != len(backing.Pix) {
		t.Fatal("backing changed size")
	}
	for i := range first {
		if first[i] != backing.Pix[i] {
			t.Fatal("repeated aliased sub-image draws are not deterministic")
		}
	}
}

// --- Concurrency ---

// TestPixelViewConcurrentCalls hammers every public operation from multiple
// goroutines. The internal mutex must serialize access so the race detector
// stays clean and the process never panics.
func TestPixelViewConcurrentCalls(t *testing.T) {
	s := NewImageSurface(40, 30)
	defer func() { _ = s.Close() }()
	s.Clear(color.White)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			src := image.NewNRGBA(image.Rect(0, 0, 5, 5))
			src.SetNRGBA(1, 1, color.NRGBA{R: uint8(seed), G: 100, B: 200, A: 180})
			for i := 0; i < 100; i++ {
				p := NewPath()
				p.Rectangle(2, 2, 20, 15)
				s.Fill(p, FillStyle{Color: color.RGBA{R: 255, A: 128}})
				sp := NewPath()
				sp.MoveTo(0, 10)
				sp.QuadTo(20, 0, 39, 10)
				s.Stroke(sp, StrokeStyle{Color: color.RGBA{B: 255, A: 255}, Width: 2})
				s.DrawImage(src, Pt(float64(seed%7), float64(seed%5)), &DrawImageOptions{Alpha: 0.7})
				s.Clear(color.RGBA{R: 240, G: 240, B: 240, A: 255})
				_ = s.Snapshot()
				_ = s.Width()
				_ = s.Height()
				_ = s.Flush()
			}
		}(g)
	}
	wg.Wait()
}

// TestPixelViewCloseConcurrent ensures Close racing with draws is safe.
func TestPixelViewCloseConcurrent(t *testing.T) {
	s := NewImageSurface(20, 20)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			p := NewPath()
			p.Circle(10, 10, 5)
			s.Fill(p, FillStyle{Color: color.RGBA{R: 255, A: 255}})
		}
	}()
	go func() {
		defer wg.Done()
		_ = s.Close()
	}()
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Errorf("double close after concurrent close: %v", err)
	}
}

// --- Closed-surface semantics ---

func TestPixelViewClosedSurfaceNoOp(t *testing.T) {
	s := NewImageSurface(6, 6)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Everything must remain a safe no-op after Close.
	s.Clear(color.Black)
	s.Fill(NewPath(), DefaultFillStyle())
	s.Stroke(NewPath(), DefaultStrokeStyle())
	s.DrawImage(image.NewRGBA(image.Rect(0, 0, 2, 2)), Pt(0, 0), nil)
	if s.Snapshot() != nil {
		t.Error("Snapshot after Close should be nil")
	}
	if s.Image() != nil {
		t.Error("Image after Close should be nil")
	}
	if err := s.Flush(); err != nil {
		t.Errorf("Flush after Close should be nil, got %v", err)
	}
}

// --- Negative / clamped dimensions ---

func TestPixelViewClampedDimensions(t *testing.T) {
	for _, dims := range [][2]int{{0, 0}, {-1, -5}, {-100, 4}, {4, -100}} {
		s := NewImageSurface(dims[0], dims[1])
		if s.Width() < 1 || s.Height() < 1 {
			t.Errorf("NewImageSurface(%d,%d) = %dx%d, want >= 1", dims[0], dims[1], s.Width(), s.Height())
		}
		if err := s.Flush(); err != nil {
			t.Errorf("Flush on clamped surface: %v", err)
		}
		s.Clear(color.RGBA{R: 1, G: 2, B: 3, A: 255})
		if c := s.Snapshot().RGBAAt(0, 0); c.A != 255 {
			t.Errorf("clear failed on clamped surface %v: %v", dims, c)
		}
		_ = s.Close()
	}
}

// --- Overflow-safe addressing ---

func TestPixelViewOffsetOverflow(t *testing.T) {
	// A rectangle with huge coordinates but no backing storage can never
	// be addressed safely; construction must reject it rather than wrap.
	huge := image.Rectangle{
		Min: image.Point{X: 0, Y: 0},
		Max: image.Point{X: 1 << 30, Y: 1 << 30},
	}
	img := &image.RGBA{
		Pix:    make([]uint8, 16),
		Stride: 16,
		Rect:   huge,
	}
	s := NewImageSurfaceFromImage(img)
	defer func() { _ = s.Close() }()
	if err := s.Flush(); err == nil {
		t.Error("expected construction error for overflow-sized rectangle")
	}
}
