// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"
)

// goldenPixelCase defines one deterministic rendering scenario exercised
// against the public ImageSurface API. The same cases are used to capture
// per-pixel golden output from the old implementation and to verify the
// refactored pixel-view implementation byte-for-byte.
type goldenPixelCase struct {
	name string
	// build runs the scenario against a fresh surface.
	build func(s *ImageSurface)
	// make creates the surface for this case.
	make func() *ImageSurface
	// snapshot true means the golden also covers Snapshot() bytes.
	snapshot bool
}

// goldenMeta is the on-disk description of a captured golden blob.
type goldenMeta struct {
	Name string `json:"name"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	MinX int    `json:"min_x"`
	MinY int    `json:"min_y"`
	Snap bool   `json:"snapshot"`
}

const goldenDir = "testdata/golden"

func goldenPixelCases() []goldenPixelCase {
	return []goldenPixelCase{
		{
			name:     "clear_opaque",
			make:     func() *ImageSurface { return NewImageSurface(24, 20) },
			snapshot: true,
			build: func(s *ImageSurface) {
				s.Clear(color.RGBA{R: 200, G: 100, B: 50, A: 255})
			},
		},
		{
			name:     "clear_translucent",
			make:     func() *ImageSurface { return NewImageSurface(16, 16) },
			snapshot: true,
			build: func(s *ImageSurface) {
				s.Clear(color.RGBA{R: 10, G: 20, B: 30, A: 128})
			},
		},
		{
			name:     "fill_rect_overlap",
			make:     func() *ImageSurface { return NewImageSurface(32, 24) },
			snapshot: true,
			build: func(s *ImageSurface) {
				s.Clear(color.White)
				p := NewPath()
				p.Rectangle(2, 3, 18, 14)
				s.Fill(p, FillStyle{Color: color.RGBA{R: 255, A: 255}})
				// Overlapping translucent fill exercises overlap writes.
				p2 := NewPath()
				p2.Rectangle(12, 8, 18, 14)
				s.Fill(p2, FillStyle{Color: color.RGBA{B: 255, A: 128}})
			},
		},
		{
			name: "fill_circle_curve",
			make: func() *ImageSurface { return NewImageSurface(28, 28) },
			build: func(s *ImageSurface) {
				s.Clear(color.White)
				p := NewPath()
				p.Circle(14, 14, 9)
				s.Fill(p, FillStyle{Color: color.RGBA{G: 255, A: 200}})
			},
		},
		{
			name: "fill_evenodd",
			make: func() *ImageSurface { return NewImageSurface(40, 40) },
			build: func(s *ImageSurface) {
				s.Clear(color.White)
				p := NewPath()
				p.Rectangle(5, 5, 30, 30)
				p.Rectangle(12, 12, 16, 16)
				s.Fill(p, FillStyle{Color: color.RGBA{R: 255, A: 255}, Rule: FillRuleEvenOdd})
			},
		},
		{
			name: "fill_partly_outside",
			make: func() *ImageSurface { return NewImageSurface(20, 20) },
			build: func(s *ImageSurface) {
				s.Clear(color.RGBA{R: 240, G: 240, B: 240, A: 255})
				p := NewPath()
				p.Rectangle(-8, -6, 20, 16)
				s.Fill(p, FillStyle{Color: color.RGBA{R: 30, G: 60, B: 90, A: 255}})
			},
		},
		{
			name: "stroke_line_overlap",
			make: func() *ImageSurface { return NewImageSurface(40, 30) },
			build: func(s *ImageSurface) {
				s.Clear(color.White)
				p := NewPath()
				p.MoveTo(4, 15)
				p.LineTo(36, 15)
				st := StrokeStyle{Color: color.RGBA{R: 255, A: 255}, Width: 4}
				s.Stroke(p, st)
				// Same stroke again: overlapping writes.
				s.Stroke(p, st)
			},
		},
		{
			name: "stroke_quad_curve",
			make: func() *ImageSurface { return NewImageSurface(40, 30) },
			build: func(s *ImageSurface) {
				s.Clear(color.White)
				p := NewPath()
				p.MoveTo(4, 26)
				p.QuadTo(20, -8, 36, 26)
				s.Stroke(p, StrokeStyle{Color: color.RGBA{B: 255, A: 180}, Width: 3})
			},
		},
		{
			name: "stroke_cubic_curve",
			make: func() *ImageSurface { return NewImageSurface(40, 30) },
			build: func(s *ImageSurface) {
				s.Clear(color.White)
				p := NewPath()
				p.MoveTo(4, 15)
				p.CubicTo(12, -10, 28, 40, 36, 15)
				s.Stroke(p, StrokeStyle{Color: color.RGBA{G: 255, A: 255}, Width: 2})
			},
		},
		{
			name:     "drawimage_rgba_transparent_edges",
			make:     func() *ImageSurface { return NewImageSurface(32, 24) },
			snapshot: true,
			build: func(s *ImageSurface) {
				s.Clear(color.White)
				src := image.NewRGBA(image.Rect(0, 0, 8, 6))
				for y := 0; y < 6; y++ {
					for x := 0; x < 8; x++ {
						a := uint8(0)
						if x > 0 && y > 0 && x < 7 && y < 5 {
							a = 255
						} else if x == 0 || y == 0 {
							a = 64 // translucent edge
						}
						src.SetRGBA(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 40), B: 90, A: a})
					}
				}
				s.DrawImage(src, Pt(3, 4), nil)
			},
		},
		{
			name: "drawimage_nrgba",
			make: func() *ImageSurface { return NewImageSurface(24, 20) },
			build: func(s *ImageSurface) {
				s.Clear(color.White)
				src := image.NewNRGBA(image.Rect(0, 0, 6, 5))
				src.SetNRGBA(0, 0, color.NRGBA{R: 255, G: 0, B: 0, A: 128})
				src.SetNRGBA(1, 0, color.NRGBA{R: 0, G: 255, B: 0, A: 255})
				src.SetNRGBA(2, 0, color.NRGBA{R: 0, G: 0, B: 255, A: 0})
				src.SetNRGBA(3, 1, color.NRGBA{R: 200, G: 100, B: 50, A: 200})
				src.SetNRGBA(4, 2, color.NRGBA{R: 12, G: 200, B: 77, A: 64})
				s.DrawImage(src, Pt(2, 1), nil)
			},
		},
		{
			name: "drawimage_alpha_half",
			make: func() *ImageSurface { return NewImageSurface(24, 20) },
			build: func(s *ImageSurface) {
				s.Clear(color.White)
				src := image.NewRGBA(image.Rect(0, 0, 10, 8))
				for y := 0; y < 8; y++ {
					for x := 0; x < 10; x++ {
						src.SetRGBA(x, y, color.RGBA{R: 200, G: 30, B: 170, A: 220})
					}
				}
				s.DrawImage(src, Pt(5, 4), &DrawImageOptions{Alpha: 0.5})
			},
		},
		{
			name: "drawimage_srcrect",
			make: func() *ImageSurface { return NewImageSurface(24, 20) },
			build: func(s *ImageSurface) {
				s.Clear(color.White)
				src := image.NewRGBA(image.Rect(0, 0, 12, 10))
				for y := 0; y < 10; y++ {
					for x := 0; x < 12; x++ {
						src.SetRGBA(x, y, color.RGBA{R: uint8(x * 20), G: uint8(y * 25), B: 40, A: 255})
					}
				}
				r := image.Rect(3, 2, 9, 8)
				s.DrawImage(src, Pt(6, 5), &DrawImageOptions{SrcRect: &r})
			},
		},
		{
			name: "drawimage_out_of_bounds",
			make: func() *ImageSurface { return NewImageSurface(12, 12) },
			build: func(s *ImageSurface) {
				s.Clear(color.RGBA{R: 245, G: 245, B: 245, A: 255})
				src := image.NewRGBA(image.Rect(0, 0, 6, 6))
				for i := range src.Pix {
					src.Pix[i] = 0
				}
				for y := 0; y < 6; y++ {
					for x := 0; x < 6; x++ {
						src.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
					}
				}
				s.DrawImage(src, Pt(-3, -2), nil)
				s.DrawImage(src, Pt(9, 8), nil)
			},
		},
		{
			name: "drawimage_generic_gray",
			make: func() *ImageSurface { return NewImageSurface(16, 16) },
			build: func(s *ImageSurface) {
				s.Clear(color.White)
				src := image.NewGray(image.Rect(0, 0, 5, 4))
				for y := 0; y < 4; y++ {
					for x := 0; x < 5; x++ {
						src.SetGray(x, y, color.Gray{Y: uint8(x*40 + y*10)})
					}
				}
				s.DrawImage(src, Pt(3, 2), &DrawImageOptions{Alpha: 0.75})
			},
		},
		{
			name: "drawimage_empty_and_negative_srcrect",
			make: func() *ImageSurface { return NewImageSurface(12, 12) },
			build: func(s *ImageSurface) {
				s.Clear(color.RGBA{R: 1, G: 2, B: 3, A: 255})
				empty := image.NewRGBA(image.Rect(0, 0, 0, 0))
				s.DrawImage(empty, Pt(0, 0), nil)
				src := image.NewRGBA(image.Rect(0, 0, 4, 4))
				bad := image.Rect(2, 2, 0, 0) // Max < Min: empty iteration
				s.DrawImage(src, Pt(1, 1), &DrawImageOptions{SrcRect: &bad})
			},
		},
		{
			// Non-zero origin Clear: the pre-refactor Fill/Stroke paths
			// panicked on these surfaces and its DrawImage addressed
			// backing coordinates instead of surface-local ones, so the
			// golden covers only Clear (which the old implementation got
			// right). Fill/Stroke/DrawImage correctness on non-zero
			// origins is verified in image_surface_pixelview_test.go.
			name: "fromimage_nonzero_origin",
			make: func() *ImageSurface {
				img := image.NewRGBA(image.Rect(5, 7, 5+24, 7+20))
				return NewImageSurfaceFromImage(img)
			},
			build: func(s *ImageSurface) {
				s.Clear(color.RGBA{R: 230, G: 230, B: 230, A: 255})
			},
		},
		{
			name: "fromimage_nonzero_origin_blank",
			make: func() *ImageSurface {
				img := image.NewRGBA(image.Rect(8, 3, 8+12, 3+9))
				return NewImageSurfaceFromImage(img)
			},
			build: func(s *ImageSurface) {
				s.Clear(color.RGBA{R: 9, G: 8, B: 7, A: 255})
			},
		},
		{
			name: "fromimage_custom_stride",
			make: func() *ImageSurface {
				w, h := 10, 8
				stride := 4*w + 12 // padded stride
				pix := make([]uint8, stride*h)
				img := &image.RGBA{Pix: pix, Stride: stride, Rect: image.Rect(0, 0, w, h)}
				return NewImageSurfaceFromImage(img)
			},
			build: func(s *ImageSurface) {
				s.Clear(color.RGBA{R: 77, G: 88, B: 99, A: 255})
				p := NewPath()
				p.Rectangle(2, 2, 6, 4)
				s.Fill(p, FillStyle{Color: color.RGBA{G: 255, A: 255}})
			},
		},
		{
			name: "negative_size_clamped",
			make: func() *ImageSurface { return NewImageSurface(-5, -3) },
			build: func(s *ImageSurface) {
				s.Clear(color.RGBA{R: 12, G: 34, B: 56, A: 255})
			},
		},
	}
}

// backingBytes extracts row-major RGBA bytes using the image's own bounds,
// so it is correct for surfaces with non-zero origins / custom strides.
func backingBytes(s *ImageSurface) []byte {
	img := s.Image()
	b := img.Bounds()
	out := make([]byte, 0, b.Dx()*b.Dy()*4)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			i := img.PixOffset(x, y)
			out = append(out, img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3])
		}
	}
	return out
}

func snapshotBytes(s *ImageSurface) []byte {
	snap := s.Snapshot()
	if snap == nil {
		return nil
	}
	return append([]byte(nil), snap.Pix...)
}

// TestImageSurfaceGoldenPixels compares every rendering scenario against the
// golden captured from the pre-refactor implementation.
//
// Capture with: SURFACE_GOLDEN_UPDATE=1 go test ./surface/ -run TestImageSurfaceGoldenPixels
func TestImageSurfaceGoldenPixels(t *testing.T) {
	cases := goldenPixelCases()

	var metas []goldenMeta
	update := os.Getenv("SURFACE_GOLDEN_UPDATE") == "1"
	if update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatalf("create golden dir: %v", err)
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.make()
			tc.build(s)

			img := s.Image()
			b := img.Bounds()
			got := backingBytes(s)
			blob := got
			if tc.snapshot {
				blob = append(blob, snapshotBytes(s)...)
			}

			meta := goldenMeta{
				Name: tc.name,
				W:    b.Dx(),
				H:    b.Dy(),
				MinX: b.Min.X,
				MinY: b.Min.Y,
				Snap: tc.snapshot,
			}

			goldenPath := filepath.Join(goldenDir, tc.name+".rgba")
			if update {
				if err := os.WriteFile(goldenPath, blob, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				metas = append(metas, meta)
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s (run with SURFACE_GOLDEN_UPDATE=1 first): %v", goldenPath, err)
			}
			if !bytes.Equal(want, blob) {
				t.Fatalf("pixel mismatch for %s: got %d bytes, want %d bytes", tc.name, len(blob), len(want))
			}
		})
	}

	if update {
		data, err := json.MarshalIndent(metas, "", "  ")
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		if err := os.WriteFile(filepath.Join(goldenDir, "manifest.json"), data, 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
}
