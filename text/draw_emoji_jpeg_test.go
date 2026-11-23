package text

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/gogpu/gg/text/emoji"
)

// fakeColorFont implements ColorFont returning a fixed bitmap glyph.
type fakeColorFont struct {
	bitmap *emoji.BitmapGlyph
	err    error
}

func (f *fakeColorFont) HasColorTables() bool       { return true }
func (f *fakeColorFont) GlyphType(uint16) GlyphType { return GlyphTypeBitmap }
func (f *fakeColorFont) COLRGlyph(uint16, int) (*emoji.COLRGlyph, error) {
	return nil, emoji.ErrGlyphNotInCOLR
}

func (f *fakeColorFont) BitmapGlyph(glyphID uint16, ppem uint16) (*emoji.BitmapGlyph, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.bitmap, nil
}

// encodeJPEGForDraw encodes a solid-color JPEG bitmap.
func encodeJPEGForDraw(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestDrawBitmapGlyph_JPEG_ScalingAndOrigin(t *testing.T) {
	// 8x8 red JPEG at strike ppem 16, drawn at ppem 32 => 2x scale => 16x16.
	jpegData := encodeJPEGForDraw(t, 8, 8, color.NRGBA{R: 255, A: 255})
	font := &fakeColorFont{bitmap: &emoji.BitmapGlyph{
		GlyphID: 1,
		Format:  emoji.FormatJPEG,
		Data:    jpegData,
		Width:   8,
		Height:  8,
		OriginX: 2,
		OriginY: 4,
		PPEM:    16,
	}}

	dst := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	drawBitmapGlyph(dst, font, 1, 32, 10, 20)

	// Expected destination rect: x = 10 + 2*2 = 14, y = 20 - 4*2 = 12,
	// size 16x16.
	want := image.Rect(14, 12, 30, 28)

	// Nothing may be written outside the glyph bounds.
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			got := dst.NRGBAAt(x, y)
			if !image.Pt(x, y).In(want) {
				if got != (color.NRGBA{}) {
					t.Fatalf("pixel (%d,%d) outside glyph bounds was written: %v", x, y, got)
				}
				continue
			}
			if got.A != 255 {
				t.Fatalf("pixel (%d,%d) alpha = %d, want 255 (opaque JPEG)", x, y, got.A)
			}
			// JPEG is lossy; allow a small tolerance around pure red.
			if got.R < 200 || got.G > 60 || got.B > 60 {
				t.Fatalf("pixel (%d,%d) = %v, want ~red", x, y, got)
			}
		}
	}
}

func TestDrawBitmapGlyph_JPEG_InvalidPPEM(t *testing.T) {
	jpegData := encodeJPEGForDraw(t, 8, 8, color.NRGBA{G: 255, A: 255})

	// PPEM 0 in the bitmap metadata must not panic or divide by zero.
	font := &fakeColorFont{bitmap: &emoji.BitmapGlyph{
		GlyphID: 1,
		Format:  emoji.FormatJPEG,
		Data:    jpegData,
		Width:   8,
		Height:  8,
		PPEM:    0,
	}}
	dst := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	drawBitmapGlyph(dst, font, 1, 16, 4, 20) // must not panic

	// ppem 0 requested at draw time must not panic either.
	font2 := &fakeColorFont{bitmap: &emoji.BitmapGlyph{
		GlyphID: 1,
		Format:  emoji.FormatJPEG,
		Data:    jpegData,
		Width:   8,
		Height:  8,
		PPEM:    16,
	}}
	drawBitmapGlyph(dst, font2, 1, 0, 4, 20) // must not panic
}

func TestDrawBitmapGlyph_JPEG_CorruptDataSkipped(t *testing.T) {
	font := &fakeColorFont{bitmap: &emoji.BitmapGlyph{
		GlyphID: 1,
		Format:  emoji.FormatJPEG,
		Data:    []byte{0xFF, 0xD8, 0x00, 0x01}, // SOI then garbage
		Width:   8,
		Height:  8,
		PPEM:    16,
	}}
	dst := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	drawBitmapGlyph(dst, font, 1, 16, 0, 16) // must not panic; glyph skipped

	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			if got := dst.NRGBAAt(x, y); got != (color.NRGBA{}) {
				t.Fatalf("corrupt glyph wrote pixel (%d,%d) = %v", x, y, got)
			}
		}
	}
}
