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

// resizeTestColors are the four checkerboard colors used across resize tests.
var resizeTestColors = [4]color.RGBA{
	{255, 0, 0, 255},   // red
	{0, 255, 0, 255},   // green
	{0, 0, 255, 255},   // blue
	{255, 255, 0, 255}, // yellow
}

// drawCheckerboard fills the surface with a checkerboard of the given cell
// size using the public drawing API (Clear + Fill).
func drawCheckerboard(s *ImageSurface, cell int) {
	s.Clear(resizeTestColors[3])
	for y := 0; y*cell < s.Height(); y++ {
		for x := 0; x*cell < s.Width(); x++ {
			c := resizeTestColors[(x+y)%3]
			path := NewPath()
			path.Rectangle(float64(x*cell), float64(y*cell), float64(cell), float64(cell))
			s.Fill(path, FillStyle{Color: c})
		}
	}
}

// expectPixel asserts the pixel at (x, y) equals want.
func expectPixel(t *testing.T, img *image.RGBA, x, y int, want color.RGBA) {
	t.Helper()
	got := img.RGBAAt(x, y)
	if got != want {
		t.Errorf("pixel (%d,%d) = %v, want %v", x, y, got, want)
	}
}

// TestImageSurfaceResizeEnlargeAnchors verifies that enlarging with each
// anchor places the checkerboard at the expected offset and fills newly
// exposed area with the clear color.
func TestImageSurfaceResizeEnlargeAnchors(t *testing.T) {
	const (
		oldW, oldH = 8, 8
		newW, newH = 16, 12
		cell       = 4
	)
	clearC := color.RGBA{10, 20, 30, 255}

	tests := []struct {
		name     string
		anchor   ResizeAnchor
		wantOffX int
		wantOffY int
	}{
		{"TopLeft", AnchorTopLeft, 0, 0},
		{"Top", AnchorTop, 4, 0},
		{"TopRight", AnchorTopRight, 8, 0},
		{"Left", AnchorLeft, 0, 2},
		{"Center", AnchorCenter, 4, 2},
		{"Right", AnchorRight, 8, 2},
		{"BottomLeft", AnchorBottomLeft, 0, 4},
		{"Bottom", AnchorBottom, 4, 4},
		{"BottomRight", AnchorBottomRight, 8, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewImageSurface(oldW, oldH)
			defer s.Close()
			drawCheckerboard(s, cell)

			err := s.ResizeWithOptions(newW, newH, &ResizeOptions{
				Anchor:     tt.anchor,
				ClearColor: clearC,
			})
			if err != nil {
				t.Fatalf("ResizeWithOptions() error = %v", err)
			}

			if s.Width() != newW || s.Height() != newH {
				t.Fatalf("size = %dx%d, want %dx%d", s.Width(), s.Height(), newW, newH)
			}

			img := s.Snapshot()

			// Checkerboard cell (0,0) is red and must appear at the anchor offset.
			expectPixel(t, img, tt.wantOffX+1, tt.wantOffY+1, resizeTestColors[0])
			// Cell (1,0) is green.
			expectPixel(t, img, tt.wantOffX+cell+1, tt.wantOffY+1, resizeTestColors[1])
			// Cell (0,1) is green, cell (1,1) is blue.
			expectPixel(t, img, tt.wantOffX+1, tt.wantOffY+cell+1, resizeTestColors[1])
			expectPixel(t, img, tt.wantOffX+cell+1, tt.wantOffY+cell+1, resizeTestColors[2])

			// Corners of the new area must be the clear color (unless covered).
			corners := [][2]int{{0, 0}, {newW - 1, 0}, {0, newH - 1}, {newW - 1, newH - 1}}
			for _, c := range corners {
				inOld := c[0] >= tt.wantOffX && c[0] < tt.wantOffX+oldW &&
					c[1] >= tt.wantOffY && c[1] < tt.wantOffY+oldH
				if !inOld {
					expectPixel(t, img, c[0], c[1], clearC)
				}
			}
		})
	}
}

// TestImageSurfaceResizeShrink verifies that shrinking clips content
// according to the anchor.
func TestImageSurfaceResizeShrink(t *testing.T) {
	const cell = 4

	s := NewImageSurface(16, 16)
	defer s.Close()
	drawCheckerboard(s, cell)

	// Center-anchored shrink to 8x8: offset = (8-16)/2 = -4.
	if err := s.ResizeWithOptions(8, 8, &ResizeOptions{Anchor: AnchorCenter}); err != nil {
		t.Fatalf("ResizeWithOptions() error = %v", err)
	}

	img := s.Snapshot()
	// New pixel (0,0) corresponds to old (4,4) = cell (1,1) = blue.
	expectPixel(t, img, 0, 0, resizeTestColors[2])
	// New pixel (1,1) still inside cell (1,1).
	expectPixel(t, img, 1, 1, resizeTestColors[2])
	// New pixel (4,4) corresponds to old (8,8) = cell (2,2) = (2+2)%3=1 green.
	expectPixel(t, img, 5, 5, resizeTestColors[1])
}

// TestImageSurfaceResizeNegativeOffset verifies explicit negative offsets
// clip content from the left/top.
func TestImageSurfaceResizeNegativeOffset(t *testing.T) {
	const cell = 4

	s := NewImageSurface(8, 8)
	defer s.Close()
	drawCheckerboard(s, cell)

	err := s.ResizeWithOptions(8, 8, &ResizeOptions{
		Anchor:  AnchorTopLeft,
		OffsetX: -cell,
		OffsetY: -cell,
	})
	if err != nil {
		t.Fatalf("ResizeWithOptions() error = %v", err)
	}

	img := s.Snapshot()
	// New (0,0) shows old (4,4) = cell (1,1) = blue.
	expectPixel(t, img, 0, 0, resizeTestColors[2])
	// New (5,5) shows old (9,9) which was outside the old 8x8 surface;
	// that area is now transparent (no clear color given).
	expectPixel(t, img, 7, 7, color.RGBA{0, 0, 0, 0})
}

// TestImageSurfaceResizePositiveOffset verifies content shifted by a
// positive offset lands exactly and exposed area uses the clear color.
func TestImageSurfaceResizePositiveOffset(t *testing.T) {
	clearC := color.RGBA{200, 100, 50, 255}

	s := NewImageSurface(4, 4)
	defer s.Close()
	s.Clear(color.RGBA{1, 2, 3, 255})

	err := s.ResizeWithOptions(8, 8, &ResizeOptions{
		OffsetX:    3,
		OffsetY:    2,
		ClearColor: clearC,
	})
	if err != nil {
		t.Fatalf("ResizeWithOptions() error = %v", err)
	}

	img := s.Snapshot()
	expectPixel(t, img, 3, 2, color.RGBA{1, 2, 3, 255})
	expectPixel(t, img, 6, 5, color.RGBA{1, 2, 3, 255})
	expectPixel(t, img, 2, 2, clearC)
	expectPixel(t, img, 3, 1, clearC)
	expectPixel(t, img, 0, 0, clearC)
	expectPixel(t, img, 7, 7, clearC)
}

// TestImageSurfaceResizeNonZeroOrigin verifies resizing a surface backed by
// an image with a non-zero origin: content maps correctly and the new
// buffer has origin (0,0).
func TestImageSurfaceResizeNonZeroOrigin(t *testing.T) {
	src := image.NewRGBA(image.Rect(5, 7, 13, 15)) // 8x8 at origin (5,7)
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			src.SetRGBA(5+x, 7+y, color.RGBA{uint8(x * 30), uint8(y * 30), 0, 255})
		}
	}

	s := NewImageSurfaceFromImage(src)
	defer s.Close()

	if err := s.Resize(16, 16); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	img := s.Image()
	if b := img.Bounds(); b.Min != (image.Point{}) {
		t.Errorf("new bounds Min = %v, want (0,0)", b.Min)
	}

	snap := s.Snapshot()
	expectPixel(t, snap, 0, 0, color.RGBA{0, 0, 0, 255})
	expectPixel(t, snap, 2, 3, color.RGBA{60, 90, 0, 255})
	expectPixel(t, snap, 7, 7, color.RGBA{210, 210, 0, 255})
	// Beyond old content: transparent.
	expectPixel(t, snap, 8, 8, color.RGBA{0, 0, 0, 0})
}

// TestImageSurfaceResizeAlphaPreserved verifies semi-transparent pixels are
// copied byte-for-byte without darkening or premultiplication artifacts.
func TestImageSurfaceResizeAlphaPreserved(t *testing.T) {
	s := NewImageSurface(4, 4)
	defer s.Close()

	semi := []color.RGBA{
		{100, 50, 200, 128},
		{255, 255, 255, 1},
		{10, 20, 30, 254},
		{0, 0, 0, 0},
	}
	img := s.Image()
	for i, c := range semi {
		img.SetRGBA(i, 0, c)
	}

	// Enlarge with an opaque clear color; copied pixels must be untouched.
	err := s.ResizeWithOptions(8, 8, &ResizeOptions{
		Anchor:     AnchorCenter,
		ClearColor: color.RGBA{255, 0, 255, 255},
	})
	if err != nil {
		t.Fatalf("ResizeWithOptions() error = %v", err)
	}

	snap := s.Snapshot()
	for i, want := range semi {
		expectPixel(t, snap, 2+i, 2, want)
	}

	// Resize repeatedly; alpha bytes must survive every round trip.
	// Re-seed the semi-transparent pixels at the top-left so the
	// top-left-anchored resizes keep them in bounds.
	for i, c := range semi {
		s.Image().SetRGBA(i, 0, c)
	}
	for i := 0; i < 5; i++ {
		if err := s.Resize(4+i, 4+i); err != nil {
			t.Fatalf("Resize() iteration %d error = %v", i, err)
		}
	}
	snap = s.Snapshot()
	for i, want := range semi {
		expectPixel(t, snap, i, 0, want)
	}
}

// TestImageSurfaceResizeRepeatedAnchors verifies a sequence of anchored
// resizes never misaligns a marker pixel.
func TestImageSurfaceResizeRepeatedAnchors(t *testing.T) {
	s := NewImageSurface(4, 4)
	defer s.Close()
	s.Clear(color.RGBA{9, 8, 7, 255})

	marker := color.RGBA{250, 251, 252, 255}
	s.Image().SetRGBA(1, 1, marker)

	// Track the marker's logical position through each resize.
	mx, my := 1, 1
	steps := []struct {
		w, h   int
		anchor ResizeAnchor
	}{
		{8, 8, AnchorTopLeft},
		{16, 16, AnchorBottomRight},
		{10, 6, AnchorCenter},
		{32, 32, AnchorTop},
		{24, 20, AnchorTopLeft},
		{20, 12, AnchorRight},
	}

	prevW, prevH := 4, 4
	for i, st := range steps {
		dx, dy := anchorOffset(st.anchor, prevW, prevH, st.w, st.h)
		mx += dx
		my += dy
		if err := s.ResizeWithOptions(st.w, st.h, &ResizeOptions{Anchor: st.anchor}); err != nil {
			t.Fatalf("step %d: ResizeWithOptions() error = %v", i, err)
		}
		prevW, prevH = st.w, st.h

		if mx < 0 || mx >= st.w || my < 0 || my >= st.h {
			t.Fatalf("step %d: marker clipped out of bounds at (%d,%d)", i, mx, my)
		}
		expectPixel(t, s.Snapshot(), mx, my, marker)
	}
}

// TestImageSurfaceResizeSameSizeNoOp verifies same-size resize preserves
// content and does not reallocate.
func TestImageSurfaceResizeSameSizeNoOp(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer s.Close()
	s.Clear(color.RGBA{42, 43, 44, 255})

	before := s.Image()
	if err := s.Resize(8, 8); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if s.Image() != before {
		t.Error("same-size resize should not reallocate the buffer")
	}
	expectPixel(t, s.Snapshot(), 4, 4, color.RGBA{42, 43, 44, 255})
}

// TestImageSurfaceResizeInvalidDimensions verifies zero/negative dimensions
// return an error and leave the surface untouched.
func TestImageSurfaceResizeInvalidDimensions(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer s.Close()
	s.Clear(color.RGBA{77, 88, 99, 255})

	for _, d := range [][2]int{{0, 8}, {8, 0}, {0, 0}, {-1, 8}, {8, -5}} {
		err := s.Resize(d[0], d[1])
		if !errors.Is(err, ErrInvalidDimensions) {
			t.Errorf("Resize(%d, %d) error = %v, want ErrInvalidDimensions", d[0], d[1], err)
		}
	}

	if s.Width() != 8 || s.Height() != 8 {
		t.Errorf("size changed to %dx%d after failed resize", s.Width(), s.Height())
	}
	expectPixel(t, s.Snapshot(), 4, 4, color.RGBA{77, 88, 99, 255})
}

// TestImageSurfaceResizeTooLarge verifies oversized dimensions are rejected
// before allocation.
func TestImageSurfaceResizeTooLarge(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer s.Close()

	cases := [][2]int{
		{maxSurfaceDimension + 1, 8},
		{8, maxSurfaceDimension + 1},
		{maxSurfaceDimension, maxSurfaceDimension}, // 1 GiP > maxSurfacePixels
		{1 << 30, 1 << 30},                         // would overflow int32 stride math
	}
	for _, d := range cases {
		err := s.Resize(d[0], d[1])
		if !errors.Is(err, ErrSurfaceTooLarge) {
			t.Errorf("Resize(%d, %d) error = %v, want ErrSurfaceTooLarge", d[0], d[1], err)
		}
	}

	if s.Width() != 8 || s.Height() != 8 {
		t.Errorf("size changed to %dx%d after rejected resize", s.Width(), s.Height())
	}
}

// TestImageSurfaceResizeClosed verifies resizing a closed surface returns
// ErrSurfaceClosed and other operations remain safe no-ops.
func TestImageSurfaceResizeClosed(t *testing.T) {
	s := NewImageSurface(8, 8)
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if err := s.Resize(16, 16); !errors.Is(err, ErrSurfaceClosed) {
		t.Errorf("Resize() error = %v, want ErrSurfaceClosed", err)
	}
	if err := s.ResizeWithOptions(16, 16, &ResizeOptions{Anchor: AnchorCenter}); !errors.Is(err, ErrSurfaceClosed) {
		t.Errorf("ResizeWithOptions() error = %v, want ErrSurfaceClosed", err)
	}

	// Other operations must remain safe no-ops after failed resizes.
	s.Clear(color.White)
	s.Fill(NewPath(), DefaultFillStyle())
	if snap := s.Snapshot(); snap != nil {
		t.Error("Snapshot() on closed surface should return nil")
	}
}

// TestImageSurfaceResizeConcurrent verifies concurrent Resize/draw/snapshot
// operations are serialized and never panic or corrupt dimensions.
func TestImageSurfaceResizeConcurrent(t *testing.T) {
	s := NewImageSurface(16, 16)
	defer s.Close()

	path := NewPath()
	path.Rectangle(2, 2, 8, 8)

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				switch g % 4 {
				case 0:
					_ = s.Resize(16+i%24, 16+i%24)
				case 1:
					s.Fill(path, FillStyle{Color: color.RGBA{255, 0, 0, 255}})
				case 2:
					s.Clear(color.RGBA{0, 0, 0, 255})
				case 3:
					if snap := s.Snapshot(); snap != nil {
						if b := snap.Bounds(); b.Dx() <= 0 || b.Dy() <= 0 {
							t.Errorf("invalid snapshot bounds %v", b)
						}
					}
				}
			}
		}(g)
	}
	wg.Wait()

	// Surface must still be fully functional.
	if err := s.Resize(32, 32); err != nil {
		t.Fatalf("Resize() after concurrent use error = %v", err)
	}
	s.Clear(color.RGBA{1, 2, 3, 255})
	expectPixel(t, s.Snapshot(), 16, 16, color.RGBA{1, 2, 3, 255})
}

// TestImageSurfaceResizeCapabilities verifies Capabilities agrees with the
// actual resize behavior.
func TestImageSurfaceResizeCapabilities(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer s.Close()

	caps := s.Capabilities()
	if !caps.SupportsResize {
		t.Error("Capabilities.SupportsResize = false, want true")
	}
	if caps.MaxWidth != maxSurfaceDimension || caps.MaxHeight != maxSurfaceDimension {
		t.Errorf("Max dimensions = %dx%d, want %d",
			caps.MaxWidth, caps.MaxHeight, maxSurfaceDimension)
	}

	// A size within the reported limits must succeed.
	if err := s.Resize(caps.MaxWidth/2, caps.MaxHeight/4); err != nil {
		t.Errorf("Resize within reported limits failed: %v", err)
	}
	// A size beyond the reported limits must fail.
	if err := s.Resize(caps.MaxWidth+1, 8); err == nil {
		t.Error("Resize beyond MaxWidth should fail")
	}
}

// TestImageSurfaceResizeStride verifies the new buffer uses a tight
// stride = 4*width and reports correct bounds.
func TestImageSurfaceResizeStride(t *testing.T) {
	s := NewImageSurface(6, 6)
	defer s.Close()

	for _, sz := range [][2]int{{7, 5}, {13, 3}, {2, 9}} {
		if err := s.Resize(sz[0], sz[1]); err != nil {
			t.Fatalf("Resize(%d, %d) error = %v", sz[0], sz[1], err)
		}
		img := s.Image()
		if img.Stride != 4*sz[0] {
			t.Errorf("stride = %d, want %d for width %d", img.Stride, 4*sz[0], sz[0])
		}
		if b := img.Bounds(); b != image.Rect(0, 0, sz[0], sz[1]) {
			t.Errorf("bounds = %v, want (0,0)-(%d,%d)", b, sz[0], sz[1])
		}
		if len(img.Pix) != img.Stride*sz[1] {
			t.Errorf("pix len = %d, want %d", len(img.Pix), img.Stride*sz[1])
		}
	}
}

// TestImageSurfaceResizeSnapshotIndependence verifies snapshots taken before
// a resize are unaffected by it, and drawing after resize does not leak
// into old snapshots.
func TestImageSurfaceResizeSnapshotIndependence(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer s.Close()
	s.Clear(color.RGBA{10, 10, 10, 255})

	snapBefore := s.Snapshot()

	if err := s.Resize(16, 16); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	s.Clear(color.RGBA{200, 200, 200, 255})

	expectPixel(t, snapBefore, 4, 4, color.RGBA{10, 10, 10, 255})
	if b := snapBefore.Bounds(); b != image.Rect(0, 0, 8, 8) {
		t.Errorf("snapshot bounds = %v, want (0,0)-(8,8)", b)
	}
}

// TestImageSurfaceResizeFromImageDetaches verifies that a surface created
// from an existing image stops writing to that image after a resize.
func TestImageSurfaceResizeFromImageDetaches(t *testing.T) {
	orig := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range orig.Pix {
		orig.Pix[i] = 255 // opaque white
	}

	s := NewImageSurfaceFromImage(orig)
	defer s.Close()

	if err := s.Resize(16, 16); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	// The old content (white) must have been carried over.
	expectPixel(t, s.Snapshot(), 4, 4, color.RGBA{255, 255, 255, 255})

	// Drawing after resize must not touch the original image.
	s.Clear(color.RGBA{0, 0, 0, 255})
	for i := range orig.Pix {
		if orig.Pix[i] != 255 {
			t.Fatal("original image modified after resize detached")
		}
	}
}

// TestImageSurfaceResizeDrawAfterResize verifies the existing drawing API
// keeps working on the resized surface.
func TestImageSurfaceResizeDrawAfterResize(t *testing.T) {
	s := NewImageSurface(16, 16)
	defer s.Close()
	s.Clear(color.White)

	if err := s.Resize(32, 24); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	// Fill in the newly exposed region.
	path := NewPath()
	path.Rectangle(20, 12, 10, 10)
	s.Fill(path, FillStyle{Color: color.RGBA{255, 0, 0, 255}})

	// Stroke across the old/new boundary.
	line := NewPath()
	line.MoveTo(0, 20)
	line.LineTo(31, 20)
	s.Stroke(line, StrokeStyle{Color: color.RGBA{0, 128, 0, 255}, Width: 2})

	// DrawImage into the new area.
	srcImg := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range srcImg.Pix {
		srcImg.Pix[i] = 255
	}
	s.DrawImage(srcImg, Pt(28, 0), nil)

	img := s.Snapshot()
	if c := img.RGBAAt(25, 16); c.R < 200 {
		t.Errorf("filled rect pixel = %v, want red", c)
	}
	// Sample the stroke away from the red rectangle (x 20..29).
	if c := img.RGBAAt(5, 20); c.G < 80 {
		t.Errorf("stroked line pixel = %v, want green", c)
	}
	if c := img.RGBAAt(30, 2); c.R != 255 || c.G != 255 || c.B != 255 {
		t.Errorf("drawn image pixel = %v, want white", c)
	}
}

// TestImageSurfaceResizeFineDetail verifies that thin stroke details
// (text-glyph-like features) survive a resize without shifting.
func TestImageSurfaceResizeFineDetail(t *testing.T) {
	s := NewImageSurface(32, 32)
	defer s.Close()
	s.Clear(color.White)

	// Draw thin vertical strokes resembling glyph stems, 1px wide.
	for x := 4; x <= 28; x += 6 {
		path := NewPath()
		path.MoveTo(float64(x), 4)
		path.LineTo(float64(x), 28)
		s.Stroke(path, StrokeStyle{Color: color.RGBA{0, 0, 0, 255}, Width: 1})
	}

	before := s.Snapshot()

	if err := s.ResizeWithOptions(48, 40, &ResizeOptions{Anchor: AnchorCenter}); err != nil {
		t.Fatalf("ResizeWithOptions() error = %v", err)
	}
	// Offset = ((48-32)/2, (40-32)/2) = (8, 4).
	after := s.Snapshot()

	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			want := before.RGBAAt(x, y)
			if got := after.RGBAAt(x+8, y+4); got != want {
				t.Fatalf("pixel (%d,%d) shifted: before=%v after=%v", x, y, want, got)
			}
		}
	}
}

// TestImageSurfaceResizeTransparentLayer verifies that a layer of
// semi-transparent content composited before the resize looks identical
// after an anchored move (no double-blending or darkening).
func TestImageSurfaceResizeTransparentLayer(t *testing.T) {
	s := NewImageSurface(16, 16)
	defer s.Close()
	s.Clear(color.RGBA{255, 255, 255, 255})

	// Semi-transparent red overlay via repeated alpha fill.
	path := NewPath()
	path.Rectangle(4, 4, 8, 8)
	s.Fill(path, FillStyle{Color: color.RGBA{255, 0, 0, 128}})

	before := s.Snapshot()
	midBefore := before.RGBAAt(8, 8)
	if midBefore.A != 255 {
		t.Fatalf("composited pixel alpha = %d, want 255", midBefore.A)
	}

	if err := s.ResizeWithOptions(24, 24, &ResizeOptions{Anchor: AnchorBottomRight}); err != nil {
		t.Fatalf("ResizeWithOptions() error = %v", err)
	}

	after := s.Snapshot()
	// Content moved by (8, 8); the blended pixel must be byte-identical.
	if got := after.RGBAAt(16, 16); got != midBefore {
		t.Errorf("transparent layer pixel = %v, want %v (darkened or shifted)", got, midBefore)
	}
	// Entire old region must match byte-for-byte.
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			if got, want := after.RGBAAt(x+8, y+8), before.RGBAAt(x, y); got != want {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got, want)
			}
		}
	}
}

// TestImageSurfaceResizeResourceRelease verifies the old buffer is dropped
// and the surface remains closable after resizes.
func TestImageSurfaceResizeResourceRelease(t *testing.T) {
	s := NewImageSurface(8, 8)

	old := s.Image()
	if err := s.Resize(64, 64); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if s.Image() == old {
		t.Error("buffer not reallocated on resize")
	}

	// The stale reference still holds the old (transparent) contents and
	// must not be corrupted by new drawing.
	s.Clear(color.RGBA{255, 0, 0, 255})
	expectPixel(t, old, 4, 4, color.RGBA{0, 0, 0, 0})

	if err := s.Close(); err != nil {
		t.Errorf("Close() after resize error = %v", err)
	}
}

// TestImageSurfaceResizableInterface verifies ImageSurface satisfies the
// ResizableSurface optional interface.
func TestImageSurfaceResizableInterface(t *testing.T) {
	var s Surface = NewImageSurface(8, 8)
	defer s.Close()

	rs, ok := s.(ResizableSurface)
	if !ok {
		t.Fatal("ImageSurface does not implement ResizableSurface")
	}
	if err := rs.Resize(16, 16); err != nil {
		t.Errorf("ResizableSurface.Resize() error = %v", err)
	}
	if rs.Width() != 16 || rs.Height() != 16 {
		t.Errorf("size = %dx%d, want 16x16", rs.Width(), rs.Height())
	}
}
