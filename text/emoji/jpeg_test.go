package emoji

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"strconv"
	"strings"
	"testing"
)

// --- JPEG fixture builders -------------------------------------------------

// jpegComponent describes one component in a hand-built JPEG fixture.
type jpegComponent struct {
	h, v int // horizontal/vertical sampling factors
}

// buildMinimalJPEG builds a valid baseline JPEG with the given dimensions
// and per-component sampling factors. All coefficients are zero, so the
// decoded image is a uniform mid-gray. Go's jpeg encoder can only produce
// 4:2:0 color and grayscale output, so fixtures for other sampling ratios
// (4:4:4, 4:2:2, 4:1:1, ...) are constructed directly.
func buildMinimalJPEG(t *testing.T, w, h int, comps []jpegComponent) []byte {
	t.Helper()

	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xD8}) // SOI

	// DQT: table 0, 8-bit precision, all ones.
	buf.Write([]byte{0xFF, 0xDB, 0x00, 67, 0x00})
	buf.Write(bytes.Repeat([]byte{1}, 64))

	// SOF0.
	ncomp := len(comps)
	sofLen := 8 + 3*ncomp
	buf.Write([]byte{0xFF, 0xC0, byte(sofLen >> 8), byte(sofLen)})
	buf.Write([]byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(ncomp)})
	for i, c := range comps {
		buf.Write([]byte{byte(i + 1), byte(c.h<<4 | c.v), 0})
	}

	// DHT: trivial tables — DC symbol 0 (zero diff) and AC symbol 0 (EOB),
	// each a single 1-bit code "0". All components share table 0.
	bits := make([]byte, 16)
	bits[0] = 1                                   // one code of length 1
	buf.Write([]byte{0xFF, 0xC4, 0x00, 20, 0x00}) // DC table 0
	buf.Write(bits)
	buf.WriteByte(0x00)
	buf.Write([]byte{0xFF, 0xC4, 0x00, 20, 0x10}) // AC table 0
	buf.Write(bits)
	buf.WriteByte(0x00)

	// SOS.
	sosLen := 6 + 2*ncomp
	buf.Write([]byte{0xFF, 0xDA, byte(sosLen >> 8), byte(sosLen), byte(ncomp)})
	for i := range comps {
		buf.Write([]byte{byte(i + 1), 0x00})
	}
	buf.Write([]byte{0, 63, 0})

	// Scan data: every block is DC diff 0 ("0") + EOB ("0") = 2 zero bits.
	hmax, vmax := 1, 1
	blocksPerMCU := 0
	for _, c := range comps {
		if c.h > hmax {
			hmax = c.h
		}
		if c.v > vmax {
			vmax = c.v
		}
		blocksPerMCU += c.h * c.v
	}
	mcusX := (w + 8*hmax - 1) / (8 * hmax)
	mcusY := (h + 8*vmax - 1) / (8 * vmax)
	totalBits := mcusX * mcusY * blocksPerMCU * 2
	buf.Write(make([]byte, totalBits/8))
	if rem := totalBits % 8; rem > 0 {
		buf.WriteByte(byte((1 << (8 - rem)) - 1)) // pad with 1-bits
	}

	buf.Write([]byte{0xFF, 0xD9}) // EOI
	return buf.Bytes()
}

// encodeStdJPEG encodes img with the standard library encoder (4:2:0).
func encodeStdJPEG(t *testing.T, img image.Image, quality int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// gradientImage builds a non-uniform test image.
func gradientImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8((x * 255) / max(w-1, 1)),
				G: uint8((y * 255) / max(h-1, 1)),
				B: uint8(((x + y) * 255) / max(w+h-2, 1)),
				A: 255,
			})
		}
	}
	return img
}

// injectEXIFOrientation inserts an APP1 Exif segment carrying the given
// orientation tag directly after the SOI marker.
func injectEXIFOrientation(t *testing.T, jpegData []byte, orientation uint16) []byte {
	t.Helper()
	if len(jpegData) < 2 || jpegData[0] != 0xFF || jpegData[1] != 0xD8 {
		t.Fatal("fixture is not a JPEG")
	}

	var app1 bytes.Buffer
	app1.WriteString("Exif\x00\x00")
	app1.Write([]byte{'I', 'I', 42, 0})      // little-endian TIFF, magic 42
	app1.Write([]byte{8, 0, 0, 0})           // IFD0 at offset 8
	app1.Write([]byte{1, 0})                 // 1 entry
	app1.Write([]byte{0x12, 0x01})           // tag 0x0112 (Orientation)
	app1.Write([]byte{3, 0})                 // type SHORT
	app1.Write([]byte{1, 0, 0, 0})           // count 1
	app1.Write([]byte{byte(orientation), 0}) // value
	app1.Write([]byte{0, 0})                 // value padding
	app1.Write([]byte{0, 0, 0, 0})           // next IFD offset

	segLen := app1.Len() + 2
	out := make([]byte, 0, len(jpegData)+2+segLen)
	out = append(out, jpegData[:2]...) // SOI
	out = append(out, 0xFF, 0xE1, byte(segLen>>8), byte(segLen))
	out = append(out, app1.Bytes()...)
	out = append(out, jpegData[2:]...)
	return out
}

// nrgbaAt is a small helper for readable pixel assertions.
func nrgbaAt(img image.Image, x, y int) color.NRGBA {
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

// --- Decode: formats and sampling ratios -----------------------------------

func TestDecodeJPEG_SamplingRatios(t *testing.T) {
	cases := []struct {
		name  string
		comps []jpegComponent
	}{
		{"grayscale", []jpegComponent{{1, 1}}},
		{"4:4:4", []jpegComponent{{1, 1}, {1, 1}, {1, 1}}},
		{"4:2:2", []jpegComponent{{2, 1}, {1, 1}, {1, 1}}},
		{"4:2:0", []jpegComponent{{2, 2}, {1, 1}, {1, 1}}},
		{"4:1:1", []jpegComponent{{4, 1}, {1, 1}, {1, 1}}},
		{"4:4:0", []jpegComponent{{1, 2}, {1, 1}, {1, 1}}},
	}
	sizes := []struct{ w, h int }{{16, 16}, {24, 8}, {21, 13}, {1, 1}}

	for _, tc := range cases {
		for _, sz := range sizes {
			t.Run(tc.name+"/"+strconv.Itoa(sz.w)+"x"+strconv.Itoa(sz.h), func(t *testing.T) {
				data := buildMinimalJPEG(t, sz.w, sz.h, tc.comps)

				// Reference: standard decoder must accept the fixture.
				ref, err := jpeg.Decode(bytes.NewReader(data))
				if err != nil {
					t.Fatalf("std jpeg.Decode rejected fixture: %v", err)
				}

				glyph := &BitmapGlyph{GlyphID: 7, Format: FormatJPEG, Data: data, PPEM: 32}
				img, err := glyph.Decode()
				if err != nil {
					t.Fatalf("Decode() error = %v", err)
				}

				// Bounds must match the coded size exactly — padded MCU
				// blocks must not leak outside the glyph bounds.
				if got := img.Bounds(); got != image.Rect(0, 0, sz.w, sz.h) {
					t.Fatalf("Bounds() = %v, want (0,0)-(%d,%d)", got, sz.w, sz.h)
				}

				// Pixels must match the standard decoder, with opaque alpha.
				for y := 0; y < sz.h; y++ {
					for x := 0; x < sz.w; x++ {
						want := nrgbaAt(ref, x, y)
						got := nrgbaAt(img, x, y)
						if got != want {
							t.Fatalf("pixel (%d,%d) = %v, want %v (std jpeg)", x, y, got, want)
						}
						if got.A != 255 {
							t.Fatalf("pixel (%d,%d) alpha = %d, want 255", x, y, got.A)
						}
					}
				}
			})
		}
	}
}

func TestDecodeJPEG_MatchesStdDecoder(t *testing.T) {
	// Non-uniform content through the standard encoder (4:2:0, quality 90).
	src := gradientImage(37, 23)
	data := encodeStdJPEG(t, src, 90)

	glyph := &BitmapGlyph{GlyphID: 3, Format: FormatJPEG, Data: data, PPEM: 16}
	img, err := glyph.Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	ref, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("std jpeg.Decode: %v", err)
	}

	if img.Bounds() != ref.Bounds() && img.Bounds() != image.Rect(0, 0, 37, 23) {
		t.Fatalf("Bounds() = %v, want (0,0)-(37,23)", img.Bounds())
	}
	for y := 0; y < 23; y++ {
		for x := 0; x < 37; x++ {
			got, want := nrgbaAt(img, x, y), nrgbaAt(ref, x, y)
			if got != want {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got, want)
			}
		}
	}
}

func TestDecodeJPEG_OpaqueOverTransparentBackground(t *testing.T) {
	// JPEG has no alpha: compositing the decoded glyph over a transparent
	// background must yield fully opaque pixels (same RGBA semantics as an
	// opaque PNG).
	data := encodeStdJPEG(t, gradientImage(16, 16), 95)
	glyph := &BitmapGlyph{GlyphID: 1, Format: FormatJPEG, Data: data, PPEM: 16}
	img, err := glyph.Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	dst := image.NewNRGBA(image.Rect(0, 0, 16, 16)) // all-transparent
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			dst.SetNRGBA(x, y, nrgbaAt(img, x, y)) // source-over with a=255
		}
	}
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			if a := nrgbaAt(dst, x, y).A; a != 255 {
				t.Fatalf("composited pixel (%d,%d) alpha = %d, want 255", x, y, a)
			}
		}
	}
}

// --- EXIF orientation --------------------------------------------------------

func TestDecodeJPEG_EXIFOrientation(t *testing.T) {
	// 4x2 image with a distinct color per pixel quadrant landmark.
	src := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255}) // top-left: red
	src.SetNRGBA(3, 0, color.NRGBA{G: 255, A: 255}) // top-right: green
	src.SetNRGBA(0, 1, color.NRGBA{B: 255, A: 255}) // bottom-left: blue
	src.SetNRGBA(3, 1, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	base := encodeStdJPEG(t, src, 100)

	// Decode the reference pixels once (lossy, so compare against the
	// std-decoded baseline rather than the original).
	refImg, err := jpeg.Decode(bytes.NewReader(base))
	if err != nil {
		t.Fatalf("std jpeg.Decode: %v", err)
	}
	ref := make([][]color.NRGBA, 2)
	for y := 0; y < 2; y++ {
		ref[y] = make([]color.NRGBA, 4)
		for x := 0; x < 4; x++ {
			ref[y][x] = nrgbaAt(refImg, x, y)
		}
	}

	// mapFn gives, for each orientation, the source pixel expected at each
	// destination coordinate, plus the expected display dimensions.
	cases := []struct {
		orientation uint16
		dw, dh      int
		srcXY       func(dx, dy int) (int, int)
	}{
		{1, 4, 2, func(dx, dy int) (int, int) { return dx, dy }},
		{2, 4, 2, func(dx, dy int) (int, int) { return 3 - dx, dy }},
		{3, 4, 2, func(dx, dy int) (int, int) { return 3 - dx, 1 - dy }},
		{4, 4, 2, func(dx, dy int) (int, int) { return dx, 1 - dy }},
		{5, 2, 4, func(dx, dy int) (int, int) { return dy, dx }},
		{6, 2, 4, func(dx, dy int) (int, int) { return dy, 1 - dx }},
		{7, 2, 4, func(dx, dy int) (int, int) { return 3 - dy, 1 - dx }},
		{8, 2, 4, func(dx, dy int) (int, int) { return 3 - dy, dx }},
	}

	for _, tc := range cases {
		t.Run("orientation_"+strconv.Itoa(int(tc.orientation)), func(t *testing.T) {
			data := injectEXIFOrientation(t, base, tc.orientation)
			glyph := &BitmapGlyph{GlyphID: 9, Format: FormatJPEG, Data: data, PPEM: 16}
			img, err := glyph.Decode()
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if got := img.Bounds(); got != image.Rect(0, 0, tc.dw, tc.dh) {
				t.Fatalf("Bounds() = %v, want (0,0)-(%d,%d)", got, tc.dw, tc.dh)
			}
			for dy := 0; dy < tc.dh; dy++ {
				for dx := 0; dx < tc.dw; dx++ {
					sx, sy := tc.srcXY(dx, dy)
					if got, want := nrgbaAt(img, dx, dy), ref[sy][sx]; got != want {
						t.Errorf("pixel (%d,%d) = %v, want src(%d,%d) = %v",
							dx, dy, got, sx, sy, want)
					}
				}
			}

			// Dimension metadata must agree with the decoded image.
			w, h, err := jpegBitmapDimensions(data)
			if err != nil {
				t.Fatalf("jpegBitmapDimensions() error = %v", err)
			}
			if w != tc.dw || h != tc.dh {
				t.Errorf("jpegBitmapDimensions() = %dx%d, want %dx%d", w, h, tc.dw, tc.dh)
			}
		})
	}
}

// --- Error handling and resource limits ------------------------------------

func TestDecodeJPEG_Errors(t *testing.T) {
	valid := buildMinimalJPEG(t, 16, 16, []jpegComponent{{2, 2}, {1, 1}, {1, 1}})

	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, ErrInvalidJPEGData},
		{"one_byte", []byte{0xFF}, ErrInvalidJPEGData},
		{"not_jpeg", []byte{0x89, 0x50, 0x4E, 0x47}, ErrInvalidJPEGData},
		{"soi_only", []byte{0xFF, 0xD8}, ErrInvalidJPEGData},
		{"truncated_header", valid[:8], ErrInvalidJPEGData},
		{"truncated_scan", valid[:len(valid)-4], ErrInvalidJPEGData},
		{"garbage_after_soi", append([]byte{0xFF, 0xD8}, bytes.Repeat([]byte{0xAB}, 64)...), ErrInvalidJPEGData},
		{"oversized_dimensions", buildMinimalJPEG(t, 30000, 30000, []jpegComponent{{1, 1}}), ErrBitmapTooLarge},
		{"oversized_one_axis", buildMinimalJPEG(t, 5000, 16, []jpegComponent{{1, 1}}), ErrBitmapTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			glyph := &BitmapGlyph{GlyphID: 42, Format: FormatJPEG, Data: tc.data, PPEM: 16}
			_, err := glyph.Decode()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Decode() error = %v, want errors.Is %v", err, tc.want)
			}
			// Error must carry glyph context.
			if !strings.Contains(err.Error(), "glyph 42") {
				t.Errorf("error %q lacks glyph context", err)
			}
		})
	}
}

func TestDecodeJPEG_DuplicateFrame(t *testing.T) {
	// Some fonts concatenate a duplicate JPEG frame; the first frame is
	// authoritative and trailing data is ignored.
	first := encodeStdJPEG(t, gradientImage(8, 8), 90)
	second := encodeStdJPEG(t, gradientImage(32, 32), 90)
	data := append(append([]byte{}, first...), second...)

	glyph := &BitmapGlyph{GlyphID: 5, Format: FormatJPEG, Data: data, PPEM: 16}
	img, err := glyph.Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if got := img.Bounds(); got != image.Rect(0, 0, 8, 8) {
		t.Errorf("Bounds() = %v, want first frame (0,0)-(8,8)", got)
	}
}

func TestDecodeJPEG_MutationsNoPanic(t *testing.T) {
	valid := encodeStdJPEG(t, gradientImage(16, 16), 90)

	// Truncations at every length and single-byte corruptions must never
	// panic; errors are fine.
	for i := 0; i < len(valid); i++ {
		glyph := &BitmapGlyph{GlyphID: 1, Format: FormatJPEG, Data: valid[:i]}
		_, _ = glyph.Decode()
	}
	for i := 0; i < len(valid); i += 7 {
		mutated := append([]byte{}, valid...)
		mutated[i] ^= 0xFF
		glyph := &BitmapGlyph{GlyphID: 1, Format: FormatJPEG, Data: mutated}
		_, _ = glyph.Decode()
	}
}

func TestDecodeJPEG_InvalidPPEMStillDecodes(t *testing.T) {
	// PPEM is draw-time metadata; a zero/invalid ppem must not affect
	// decoding (the draw path guards the division separately).
	data := buildMinimalJPEG(t, 8, 8, []jpegComponent{{1, 1}})
	for _, ppem := range []uint16{0, 1, 0xFFFF} {
		glyph := &BitmapGlyph{GlyphID: 2, Format: FormatJPEG, Data: data, PPEM: ppem}
		img, err := glyph.Decode()
		if err != nil {
			t.Fatalf("Decode(ppem=%d) error = %v", ppem, err)
		}
		if got := img.Bounds(); got != image.Rect(0, 0, 8, 8) {
			t.Errorf("Decode(ppem=%d) Bounds() = %v, want (0,0)-(8,8)", ppem, got)
		}
	}
}

// --- sbix integration --------------------------------------------------------

// buildSBIXTable builds a minimal sbix table with one strike containing the
// given glyph payloads (nil = empty glyph slot).
func buildSBIXTable(t *testing.T, ppem uint16, glyphs [][]byte) []byte {
	t.Helper()

	numGlyphs := len(glyphs)
	strikeOffset := 8 + 4 // header + 1 strike offset

	var buf bytes.Buffer
	// Header: version 1, flags 0, numStrikes 1.
	buf.Write([]byte{0, 1, 0, 0, 0, 0, 0, 1})
	buf.Write([]byte{0, 0, 0, byte(strikeOffset)})
	// Strike header: ppem, ppi 72.
	buf.Write([]byte{byte(ppem >> 8), byte(ppem), 0, 72})

	// Glyph data offsets (relative to strike start).
	offsets := make([]uint32, numGlyphs+1)
	pos := uint32(4 + (numGlyphs+1)*4)
	for i, g := range glyphs {
		offsets[i] = pos
		if g != nil {
			pos += uint32(len(g))
		}
	}
	offsets[numGlyphs] = pos
	for _, o := range offsets {
		buf.Write([]byte{byte(o >> 24), byte(o >> 16), byte(o >> 8), byte(o)})
	}
	for _, g := range glyphs {
		buf.Write(g)
	}
	return buf.Bytes()
}

// sbixGlyphData builds one sbix glyph record: originX, originY, tag, data.
func sbixGlyphData(originX, originY int16, tag string, data []byte) []byte {
	out := make([]byte, 0, 8+len(data))
	out = append(out,
		byte(uint16(originX)>>8), byte(uint16(originX)),
		byte(uint16(originY)>>8), byte(uint16(originY)),
		tag[0], tag[1], tag[2], tag[3],
	)
	return append(out, data...)
}

func TestSBIX_GetGlyph_JPEG(t *testing.T) {
	jpegData := buildMinimalJPEG(t, 24, 16, []jpegComponent{{2, 2}, {1, 1}, {1, 1}})
	glyph0 := sbixGlyphData(-3, 20, "jpg ", jpegData)

	table := buildSBIXTable(t, 32, [][]byte{glyph0, nil})
	parser, err := NewSBIXParser(table, 2)
	if err != nil {
		t.Fatalf("NewSBIXParser() error = %v", err)
	}

	bm, err := parser.GetGlyph(0, 0)
	if err != nil {
		t.Fatalf("GetGlyph() error = %v", err)
	}
	if bm.Format != FormatJPEG {
		t.Errorf("Format = %v, want JPEG", bm.Format)
	}
	if bm.OriginX != -3 || bm.OriginY != 20 {
		t.Errorf("origin = (%v, %v), want (-3, 20)", bm.OriginX, bm.OriginY)
	}
	if bm.PPEM != 32 {
		t.Errorf("PPEM = %d, want 32", bm.PPEM)
	}
	// Dimensions must come from the JPEG SOF, not remain zero.
	if bm.Width != 24 || bm.Height != 16 {
		t.Errorf("dimensions = %dx%d, want 24x16", bm.Width, bm.Height)
	}

	img, err := bm.Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if got := img.Bounds(); got != image.Rect(0, 0, 24, 16) {
		t.Errorf("decoded Bounds() = %v, want (0,0)-(24,16)", got)
	}

	// Empty glyph slot must report "not in bitmap".
	if _, err := parser.GetGlyph(1, 0); !errors.Is(err, ErrGlyphNotInBitmap) {
		t.Errorf("GetGlyph(empty) error = %v, want ErrGlyphNotInBitmap", err)
	}
}

func TestSBIX_GetGlyph_JPEG_EXIFDimensions(t *testing.T) {
	// A 4x2 JPEG with orientation 6 (rotate 90 CW) reports 2x4 metadata.
	base := encodeStdJPEG(t, gradientImage(4, 2), 90)
	jpegData := injectEXIFOrientation(t, base, 6)
	glyph0 := sbixGlyphData(0, 0, "jpg ", jpegData)

	table := buildSBIXTable(t, 16, [][]byte{glyph0})
	parser, err := NewSBIXParser(table, 1)
	if err != nil {
		t.Fatalf("NewSBIXParser() error = %v", err)
	}
	bm, err := parser.GetGlyph(0, 0)
	if err != nil {
		t.Fatalf("GetGlyph() error = %v", err)
	}
	if bm.Width != 2 || bm.Height != 4 {
		t.Errorf("dimensions = %dx%d, want 2x4 (EXIF axes swapped)", bm.Width, bm.Height)
	}
	img, err := bm.Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if got := img.Bounds(); got != image.Rect(0, 0, 2, 4) {
		t.Errorf("decoded Bounds() = %v, want (0,0)-(2,4)", got)
	}
}
