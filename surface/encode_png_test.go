// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"runtime"
	"testing"
)

// countingWriter counts Write calls and bytes forwarded.
type countingWriter struct {
	w     io.Writer
	calls int
	bytes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.calls++
	c.bytes += len(p)
	return c.w.Write(p)
}

// failAfterWriter allows a fixed number of bytes through, then fails.
type failAfterWriter struct {
	remaining int
	err       error
}

func (f *failAfterWriter) Write(p []byte) (int, error) {
	if f.remaining < len(p) {
		return 0, f.err
	}
	f.remaining -= len(p)
	return len(p), nil
}

// nrgbaAt returns the decoded pixel as non-premultiplied RGBA.
func nrgbaAt(img image.Image, x, y int) color.NRGBA {
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

// unpremultiply mirrors the encoder's premultiplied -> NRGBA conversion.
func unpremultiply(c color.RGBA) color.NRGBA {
	if c.A == 0 {
		return color.NRGBA{}
	}
	if c.A == 255 {
		return color.NRGBA{R: c.R, G: c.G, B: c.B, A: 255}
	}
	ua := uint32(c.A)
	return color.NRGBA{
		R: uint8((uint32(c.R) * 0xffff / ua) >> 8),
		G: uint8((uint32(c.G) * 0xffff / ua) >> 8),
		B: uint8((uint32(c.B) * 0xffff / ua) >> 8),
		A: c.A,
	}
}

// fillSurface sets every pixel of the surface from fn(x, y) in
// surface-local coordinates.
func fillSurface(s *ImageSurface, fn func(x, y int) color.RGBA) {
	img := s.Image()
	b := img.Bounds()
	for y := 0; y < s.Height(); y++ {
		for x := 0; x < s.Width(); x++ {
			off := img.PixOffset(b.Min.X+x, b.Min.Y+y)
			c := fn(x, y)
			img.Pix[off+0] = c.R
			img.Pix[off+1] = c.G
			img.Pix[off+2] = c.B
			img.Pix[off+3] = c.A
		}
	}
}

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("png.Decode: %v", err)
	}
	return img
}

// TestEncodePNGRegionTransparentEdges verifies pixels round-trip through a
// standard PNG decoder, including fully transparent edge pixels.
func TestEncodePNGRegionTransparentEdges(t *testing.T) {
	const w, h = 8, 6
	s := NewImageSurface(w, h)
	defer func() { _ = s.Close() }()

	// Transparent 1px border; interior carries opaque and semi-transparent
	// premultiplied colors.
	px := func(x, y int) color.RGBA {
		if x == 0 || y == 0 || x == w-1 || y == h-1 {
			return color.RGBA{0, 0, 0, 0}
		}
		switch (x + y) % 3 {
		case 0:
			return color.RGBA{255, 0, 0, 255}
		case 1:
			return color.RGBA{0, 128, 255, 255}
		default:
			return color.RGBA{128, 64, 32, 128} // premultiplied
		}
	}
	fillSurface(s, px)

	var buf bytes.Buffer
	if err := s.EncodePNGRegion(context.Background(), &buf, image.Rect(0, 0, w, h), nil); err != nil {
		t.Fatalf("EncodePNGRegion: %v", err)
	}

	got := decodePNG(t, buf.Bytes())
	if b := got.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("decoded size = %v, want %dx%d", b, w, h)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			want := unpremultiply(px(x, y))
			if g := nrgbaAt(got, x, y); g != want {
				t.Errorf("pixel (%d,%d) = %v, want %v", x, y, g, want)
			}
		}
	}

	// Hand-computed spot check: premultiplied {128,64,32,128} must decode
	// as NRGBA{255,127,63,128} (c8*0xffff/a8 >> 8 per channel).
	if g := nrgbaAt(got, 2, 3); g != (color.NRGBA{255, 127, 63, 128}) {
		t.Errorf("premultiplied round-trip = %v, want {255 127 63 128}", g)
	}
}

// TestEncodePNGRegionMatchesStdlib verifies decoded output matches the
// standard library's png.Encode of the same region.
func TestEncodePNGRegionMatchesStdlib(t *testing.T) {
	const w, h = 33, 17
	s := NewImageSurface(w, h)
	defer func() { _ = s.Close() }()
	fillSurface(s, func(x, y int) color.RGBA {
		return color.RGBA{
			R: uint8((x*37 + y*11) % 256), //nolint:gosec // test data
			G: uint8((x*5 + y*53) % 256),  //nolint:gosec // test data
			B: uint8((x*91 + y*7) % 256),  //nolint:gosec // test data
			A: uint8((x*13 + y*29) % 256), //nolint:gosec // test data
		}
	})

	region := image.Rect(3, 2, 30, 15)
	var mine bytes.Buffer
	if err := s.EncodePNGRegion(context.Background(), &mine, region, nil); err != nil {
		t.Fatalf("EncodePNGRegion: %v", err)
	}

	var std bytes.Buffer
	sub := s.Image().SubImage(region)
	if err := png.Encode(&std, sub); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}

	gotImg := decodePNG(t, mine.Bytes())
	stdImg := decodePNG(t, std.Bytes())
	if gotImg.Bounds().Dx() != region.Dx() || gotImg.Bounds().Dy() != region.Dy() {
		t.Fatalf("decoded size = %v, want %v", gotImg.Bounds(), region)
	}
	for y := 0; y < region.Dy(); y++ {
		for x := 0; x < region.Dx(); x++ {
			g, wnt := nrgbaAt(gotImg, x, y), nrgbaAt(stdImg, x, y)
			if g != wnt {
				t.Fatalf("pixel (%d,%d) = %v, stdlib = %v", x, y, g, wnt)
			}
		}
	}
}

// TestEncodePNGRegionNonZeroOrigin verifies correct handling of an
// underlying image with a non-zero origin and a custom (padded) stride.
func TestEncodePNGRegionNonZeroOrigin(t *testing.T) {
	const w, h = 5, 4
	const stride = 4*w + 12 // padded stride
	img := &image.RGBA{
		Pix:    make([]uint8, stride*h),
		Stride: stride,
		Rect:   image.Rect(10, 20, 10+w, 20+h),
	}
	s := NewImageSurfaceFromImage(img)
	defer func() { _ = s.Close() }()

	fillSurface(s, func(x, y int) color.RGBA {
		return color.RGBA{uint8(x * 10), uint8(y * 20), 200, 255} //nolint:gosec // test data
	})

	// Region in surface-local coordinates, not image coordinates.
	region := image.Rect(1, 1, 4, 3)
	var buf bytes.Buffer
	if err := s.EncodePNGRegion(context.Background(), &buf, region, nil); err != nil {
		t.Fatalf("EncodePNGRegion: %v", err)
	}

	got := decodePNG(t, buf.Bytes())
	if b := got.Bounds(); b.Dx() != 3 || b.Dy() != 2 {
		t.Fatalf("decoded size = %v, want 3x2", b)
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			sx, sy := x+1, y+1
			want := color.NRGBA{uint8(sx * 10), uint8(sy * 20), 200, 255} //nolint:gosec // test data
			if g := nrgbaAt(got, x, y); g != want {
				t.Errorf("pixel (%d,%d) = %v, want %v", x, y, g, want)
			}
		}
	}
}

// TestEncodePNGRegionSingleRow verifies a 1-pixel-tall region.
func TestEncodePNGRegionSingleRow(t *testing.T) {
	s := NewImageSurface(4, 1)
	defer func() { _ = s.Close() }()
	fillSurface(s, func(x, _ int) color.RGBA {
		return color.RGBA{uint8(x), 0, 0, 255} //nolint:gosec // test data
	})

	var buf bytes.Buffer
	if err := s.EncodePNGRegion(context.Background(), &buf, image.Rect(0, 0, 4, 1), nil); err != nil {
		t.Fatalf("EncodePNGRegion: %v", err)
	}
	got := decodePNG(t, buf.Bytes())
	if b := got.Bounds(); b.Dx() != 4 || b.Dy() != 1 {
		t.Fatalf("decoded size = %v, want 4x1", b)
	}
	for x := 0; x < 4; x++ {
		want := color.NRGBA{uint8(x), 0, 0, 255} //nolint:gosec // test data
		if g := nrgbaAt(got, x, 0); g != want {
			t.Errorf("pixel (%d,0) = %v, want %v", x, g, want)
		}
	}
}

// TestEncodePNGRegionFullyTransparent verifies a surface of fully
// transparent rows encodes to a valid all-zero PNG.
func TestEncodePNGRegionFullyTransparent(t *testing.T) {
	s := NewImageSurface(16, 16)
	defer func() { _ = s.Close() }()

	var buf bytes.Buffer
	if err := s.EncodePNGRegion(context.Background(), &buf, image.Rect(0, 0, 16, 16), nil); err != nil {
		t.Fatalf("EncodePNGRegion: %v", err)
	}
	got := decodePNG(t, buf.Bytes())
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			if g := nrgbaAt(got, x, y); g != (color.NRGBA{}) {
				t.Fatalf("pixel (%d,%d) = %v, want transparent zero", x, y, g)
			}
		}
	}
}

// TestEncodePNGRegionEmptyRegion verifies empty and negative regions fail
// deterministically.
func TestEncodePNGRegionEmptyRegion(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer func() { _ = s.Close() }()

	for _, r := range []image.Rectangle{
		image.Rect(0, 0, 0, 4), // zero width
		image.Rect(0, 0, 4, 0), // zero height
		image.Rectangle{},      // zero rect
		// Negative sizes: image.Rect normalizes, so construct directly.
		image.Rectangle{Min: image.Pt(4, 4), Max: image.Pt(2, 6)}, // negative width
		image.Rectangle{Min: image.Pt(2, 6), Max: image.Pt(4, 4)}, // negative height
		image.Rectangle{Min: image.Pt(3, 3), Max: image.Pt(3, 3)}, // empty point
	} {
		var buf bytes.Buffer
		err := s.EncodePNGRegion(context.Background(), &buf, r, nil)
		if !errors.Is(err, ErrEmptyRegion) {
			t.Errorf("region %v: err = %v, want ErrEmptyRegion", r, err)
		}
		if buf.Len() != 0 {
			t.Errorf("region %v: wrote %d bytes, want 0", r, buf.Len())
		}
	}
}

// TestEncodePNGRegionOutOfBounds verifies regions outside the surface fail.
func TestEncodePNGRegionOutOfBounds(t *testing.T) {
	s := NewImageSurface(8, 8)
	defer func() { _ = s.Close() }()

	for _, r := range []image.Rectangle{
		image.Rect(-1, 0, 4, 4),  // negative origin
		image.Rect(0, -2, 4, 4),  // negative origin
		image.Rect(0, 0, 9, 8),   // exceeds width
		image.Rect(0, 0, 8, 100), // exceeds height
		image.Rect(7, 7, 20, 20), // corner overflow
	} {
		var buf bytes.Buffer
		err := s.EncodePNGRegion(context.Background(), &buf, r, nil)
		if !errors.Is(err, ErrRegionOutOfBounds) {
			t.Errorf("region %v: err = %v, want ErrRegionOutOfBounds", r, err)
		}
		if buf.Len() != 0 {
			t.Errorf("region %v: wrote %d bytes, want 0", r, buf.Len())
		}
	}
}

// TestEncodePNGRegionNilWriter verifies a nil writer is rejected.
func TestEncodePNGRegionNilWriter(t *testing.T) {
	s := NewImageSurface(4, 4)
	defer func() { _ = s.Close() }()
	if err := s.EncodePNGRegion(context.Background(), nil, image.Rect(0, 0, 4, 4), nil); !errors.Is(err, ErrNilWriter) {
		t.Fatalf("err = %v, want ErrNilWriter", err)
	}
}

// TestEncodePNGRegionClosedSurface verifies encoding from a closed surface
// fails without a panic.
func TestEncodePNGRegionClosedSurface(t *testing.T) {
	s := NewImageSurface(4, 4)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var buf bytes.Buffer
	if err := s.EncodePNGRegion(context.Background(), &buf, image.Rect(0, 0, 4, 4), nil); !errors.Is(err, ErrEncodeSurfaceClosed) {
		t.Fatalf("err = %v, want ErrEncodeSurfaceClosed", err)
	}
}

// TestEncodePNGRegionWriterFails verifies a mid-stream writer failure
// propagates without panic or unbounded buffering.
func TestEncodePNGRegionWriterFails(t *testing.T) {
	s := NewImageSurface(256, 256)
	defer func() { _ = s.Close() }()
	fillSurface(s, func(x, y int) color.RGBA {
		return color.RGBA{uint8(x), uint8(y), 0, 255} //nolint:gosec // test data
	})

	errBoom := errors.New("disk full")
	// Fail well inside the compressed stream.
	fw := &failAfterWriter{remaining: 100, err: errBoom}
	err := s.EncodePNGRegion(context.Background(), fw, image.Rect(0, 0, 256, 256), nil)
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want wrapped %v", err, errBoom)
	}
}

// TestEncodePNGRegionCancel verifies cancellation before and during
// encoding returns the context error.
func TestEncodePNGRegionCancel(t *testing.T) {
	s := NewImageSurface(64, 64)
	defer func() { _ = s.Close() }()
	region := image.Rect(0, 0, 64, 64)

	// Pre-canceled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	if err := s.EncodePNGRegion(ctx, &buf, region, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled: err = %v, want context.Canceled", err)
	}

	// Cancel from the row callback partway through.
	ctx2, cancel2 := context.WithCancel(context.Background())
	rows := 0
	opts := &RegionEncodeOptions{OnRow: func(row int) error {
		rows++
		if row == 10 {
			cancel2()
		}
		return nil
	}}
	buf.Reset()
	err := s.EncodePNGRegion(ctx2, &buf, region, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-stream cancel: err = %v, want context.Canceled", err)
	}
	if rows > 12 {
		t.Fatalf("OnRow called %d times after cancel at row 10, want prompt stop", rows)
	}
}

// TestEncodePNGRegionOnRowBackpressure verifies the row callback is invoked
// once per row in order, and that its error aborts encoding.
func TestEncodePNGRegionOnRowBackpressure(t *testing.T) {
	s := NewImageSurface(8, 10)
	defer func() { _ = s.Close() }()
	region := image.Rect(0, 0, 8, 10)

	// Full pass: one callback per row, in order.
	var seen []int
	opts := &RegionEncodeOptions{OnRow: func(row int) error {
		seen = append(seen, row)
		return nil
	}}
	var buf bytes.Buffer
	if err := s.EncodePNGRegion(context.Background(), &buf, region, opts); err != nil {
		t.Fatalf("EncodePNGRegion: %v", err)
	}
	if len(seen) != 10 {
		t.Fatalf("OnRow called %d times, want 10", len(seen))
	}
	for i, r := range seen {
		if r != i {
			t.Fatalf("OnRow order = %v, want 0..9", seen)
		}
	}
	if _, err := png.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("output not decodable: %v", err)
	}

	// Abort at row 3.
	errStop := errors.New("backpressure stop")
	calls := 0
	opts.OnRow = func(row int) error {
		calls++
		if row == 3 {
			return errStop
		}
		return nil
	}
	buf.Reset()
	if err := s.EncodePNGRegion(context.Background(), &buf, region, opts); !errors.Is(err, errStop) {
		t.Fatalf("err = %v, want %v", err, errStop)
	}
	if calls != 4 {
		t.Fatalf("OnRow called %d times, want 4 (rows 0..3)", calls)
	}
}

// TestEncodePNGRegionWriteCount verifies the writer sees a deterministic,
// bounded number of Write calls.
func TestEncodePNGRegionWriteCount(t *testing.T) {
	s := NewImageSurface(200, 100)
	defer func() { _ = s.Close() }()
	fillSurface(s, func(x, y int) color.RGBA {
		return color.RGBA{uint8(x), uint8(y), 128, 255} //nolint:gosec // test data
	})
	region := image.Rect(0, 0, 200, 100)

	counts := make([]int, 2)
	sizes := make([]int, 2)
	for i := range counts {
		var buf bytes.Buffer
		cw := &countingWriter{w: &buf}
		if err := s.EncodePNGRegion(context.Background(), cw, region, nil); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		counts[i] = cw.calls
		sizes[i] = cw.bytes
	}
	if counts[0] == 0 {
		t.Fatal("no writes recorded")
	}
	if counts[0] != counts[1] || sizes[0] != sizes[1] {
		t.Fatalf("non-deterministic output: calls %v, sizes %v", counts, sizes)
	}
	// 80KB of filtered scanlines must not produce per-row writes:
	// bounded by signature + IHDR + IDAT chunks + IEND.
	if counts[0] > 20 {
		t.Fatalf("write calls = %d, want bounded chunking (<= 20)", counts[0])
	}
}

// TestEncodePNGRegionDeterministic verifies identical output across runs,
// including for a large region.
func TestEncodePNGRegionDeterministic(t *testing.T) {
	const w, h = 2048, 1024
	s := NewImageSurface(w, h)
	defer func() { _ = s.Close() }()
	fillSurface(s, func(x, y int) color.RGBA {
		return color.RGBA{uint8(x * y), uint8(x + y), uint8(x ^ y), 255} //nolint:gosec // test data
	})
	region := image.Rect(0, 0, w, h)

	var a, b bytes.Buffer
	if err := s.EncodePNGRegion(context.Background(), &a, region, nil); err != nil {
		t.Fatalf("first encode: %v", err)
	}
	if err := s.EncodePNGRegion(context.Background(), &b, region, nil); err != nil {
		t.Fatalf("second encode: %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("two encodes of the same region differ")
	}
	got := decodePNG(t, a.Bytes())
	if gb := got.Bounds(); gb.Dx() != w || gb.Dy() != h {
		t.Fatalf("decoded size = %v, want %dx%d", gb, w, h)
	}
}

// TestEncodePNGRegionPeakMemory verifies encoding a large region does not
// buffer the whole image: allocations stay far below a full-canvas copy.
func TestEncodePNGRegionPeakMemory(t *testing.T) {
	const w, h = 2048, 2048 // 16 MiB RGBA source
	s := NewImageSurface(w, h)
	defer func() { _ = s.Close() }()
	fillSurface(s, func(x, y int) color.RGBA {
		return color.RGBA{uint8(x), uint8(y), uint8(x + y), 255} //nolint:gosec // test data
	})

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := s.EncodePNGRegion(context.Background(), io.Discard, image.Rect(0, 0, w, h), nil)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("EncodePNGRegion: %v", err)
	}

	delta := after.TotalAlloc - before.TotalAlloc
	fullCopy := uint64(4 * w * h)
	if delta > fullCopy/4 {
		t.Fatalf("allocated %d bytes encoding a %d-byte region; want streaming (<= %d)",
			delta, fullCopy, fullCopy/4)
	}
}

// TestEncodePNGRegionSourceUnmodified verifies encoding never mutates the
// source pixels.
func TestEncodePNGRegionSourceUnmodified(t *testing.T) {
	s := NewImageSurface(64, 64)
	defer func() { _ = s.Close() }()
	fillSurface(s, func(x, y int) color.RGBA {
		return color.RGBA{uint8(x * 4), uint8(y * 4), uint8((x + y) * 2), uint8(255 - x)} //nolint:gosec // test data
	})

	before := append([]byte(nil), s.Image().Pix...)

	var buf bytes.Buffer
	opts := &RegionEncodeOptions{OnRow: func(int) error { return nil }}
	if err := s.EncodePNGRegion(context.Background(), &buf, image.Rect(0, 0, 64, 64), opts); err != nil {
		t.Fatalf("EncodePNGRegion: %v", err)
	}

	if !bytes.Equal(before, s.Image().Pix) {
		t.Fatal("source pixels modified by encoding")
	}
}

// TestEncodePNGRegionNilContext verifies a nil context is accepted.
func TestEncodePNGRegionNilContext(t *testing.T) {
	s := NewImageSurface(4, 4)
	defer func() { _ = s.Close() }()
	var buf bytes.Buffer
	//nolint:staticcheck // SA1012: intentionally testing nil Context handling
	if err := s.EncodePNGRegion(nil, &buf, image.Rect(0, 0, 4, 4), nil); err != nil {
		t.Fatalf("EncodePNGRegion: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("output not decodable: %v", err)
	}
}
