package emoji

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
)

// JPEG bitmap decoding errors.
var (
	// ErrInvalidJPEGData indicates malformed or undecodable JPEG bitmap data.
	ErrInvalidJPEGData = errors.New("emoji: invalid JPEG bitmap data")

	// ErrBitmapTooLarge indicates the bitmap exceeds resource limits.
	ErrBitmapTooLarge = errors.New("emoji: bitmap exceeds resource limits")
)

// Resource limits for embedded bitmap decoding. Emoji bitmaps are small
// (strikes are typically <= 256 px); these bounds reject decompression
// bombs in untrusted fonts while leaving ample headroom.
const (
	// maxBitmapDataSize bounds the compressed bitmap payload (32 MiB).
	maxBitmapDataSize = 32 << 20

	// maxBitmapDimension bounds the width and height of a decoded bitmap.
	maxBitmapDimension = 4096

	// maxBitmapPixels bounds the total decoded pixel count.
	// At 4 bytes per pixel (NRGBA) this caps allocations at 64 MiB.
	maxBitmapPixels = maxBitmapDimension * maxBitmapDimension
)

// EXIF orientation values (JPEG APP1/Exif, TIFF tag 0x0112).
const (
	exifOrientationNormal         = 1 // no transform
	exifOrientationFlipHorizontal = 2
	exifOrientationRotate180      = 3
	exifOrientationFlipVertical   = 4
	exifOrientationTranspose      = 5 // flip across main diagonal
	exifOrientationRotate90CW     = 6
	exifOrientationTransverse     = 7 // flip across anti-diagonal
	exifOrientationRotate270CW    = 8
)

// decodeJPEGBitmap decodes JPEG bitmap data to an opaque NRGBA image.
//
// JPEG has no alpha channel, so the result is fully opaque (alpha 255),
// matching the RGBA semantics of decoded PNG bitmaps composited over an
// opaque background. EXIF orientation, when present, is applied so the
// returned image is in display orientation; its bounds always start at
// (0,0) and exactly match the (possibly swapped) coded dimensions, so no
// padded MCU blocks leak outside the glyph bounds.
func decodeJPEGBitmap(data []byte) (img *image.NRGBA, err error) {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, fmt.Errorf("%w: missing SOI marker (data: %d bytes)", ErrInvalidJPEGData, len(data))
	}
	if len(data) > maxBitmapDataSize {
		return nil, fmt.Errorf("%w: compressed data %d bytes > %d byte limit",
			ErrBitmapTooLarge, len(data), maxBitmapDataSize)
	}

	// EXIF orientation is read from the raw marker stream because the
	// standard decoder ignores APP1 metadata.
	orientation := exifOrientation(data)

	// Validate dimensions before committing to a full decode.
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: header: %w", ErrInvalidJPEGData, err)
	}
	if err := checkBitmapSize(config.Width, config.Height); err != nil {
		return nil, err
	}

	// The stdlib decoder is robust, but the input is untrusted font data:
	// convert any residual decoder panic into an error.
	defer func() {
		if r := recover(); r != nil {
			img = nil
			err = fmt.Errorf("%w: decoder panic: %v", ErrInvalidJPEGData, r)
		}
	}()

	src, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidJPEGData, err)
	}

	img = toOpaqueNRGBA(src)
	if orientation != exifOrientationNormal {
		img = applyEXIFOrientation(img, orientation)
	}
	return img, nil
}

// jpegBitmapDimensions returns the display dimensions of JPEG bitmap data
// without fully decoding it, applying the EXIF orientation axis swap.
// Used to populate bitmap metadata (Width/Height) from font tables.
func jpegBitmapDimensions(data []byte) (width, height int, err error) {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return 0, 0, fmt.Errorf("%w: missing SOI marker (data: %d bytes)", ErrInvalidJPEGData, len(data))
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: header: %w", ErrInvalidJPEGData, err)
	}
	if err := checkBitmapSize(config.Width, config.Height); err != nil {
		return 0, 0, err
	}
	if swapsAxes(exifOrientation(data)) {
		return config.Height, config.Width, nil
	}
	return config.Width, config.Height, nil
}

// checkBitmapSize enforces resource limits on decoded bitmap dimensions.
func checkBitmapSize(width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("%w: non-positive dimensions %dx%d", ErrInvalidJPEGData, width, height)
	}
	if width > maxBitmapDimension || height > maxBitmapDimension || width*height > maxBitmapPixels {
		return fmt.Errorf("%w: dimensions %dx%d (limit %dx%d, %d px)",
			ErrBitmapTooLarge, width, height, maxBitmapDimension, maxBitmapDimension, maxBitmapPixels)
	}
	return nil
}

// swapsAxes reports whether an EXIF orientation exchanges width and height.
func swapsAxes(orientation int) bool {
	switch orientation {
	case exifOrientationTranspose, exifOrientationRotate90CW,
		exifOrientationTransverse, exifOrientationRotate270CW:
		return true
	}
	return false
}

// toOpaqueNRGBA converts a decoded JPEG image (YCbCr, Gray, or other) to
// NRGBA with a fully opaque alpha channel. The output bounds start at (0,0)
// and match the source size exactly, independent of MCU padding.
func toOpaqueNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))

	switch s := src.(type) {
	case *image.YCbCr:
		// Fast path with direct sample access; COffset handles the
		// chroma subsampling ratio (4:4:4, 4:2:2, 4:2:0, ...).
		for y := 0; y < h; y++ {
			sy := b.Min.Y + y
			for x := 0; x < w; x++ {
				sx := b.Min.X + x
				yi := s.YOffset(sx, sy)
				ci := s.COffset(sx, sy)
				r, g, bl := color.YCbCrToRGB(s.Y[yi], s.Cb[ci], s.Cr[ci])
				off := y*dst.Stride + x*4
				dst.Pix[off] = r
				dst.Pix[off+1] = g
				dst.Pix[off+2] = bl
				dst.Pix[off+3] = 255
			}
		}
	case *image.Gray:
		for y := 0; y < h; y++ {
			sy := b.Min.Y + y
			for x := 0; x < w; x++ {
				v := s.GrayAt(b.Min.X+x, sy).Y
				off := y*dst.Stride + x*4
				dst.Pix[off] = v
				dst.Pix[off+1] = v
				dst.Pix[off+2] = v
				dst.Pix[off+3] = 255
			}
		}
	default:
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				c := color.NRGBAModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
				off := y*dst.Stride + x*4
				dst.Pix[off] = c.R
				dst.Pix[off+1] = c.G
				dst.Pix[off+2] = c.B
				dst.Pix[off+3] = 255
			}
		}
	}
	return dst
}

// applyEXIFOrientation returns a copy of src transformed into display
// orientation. Orientations 5-8 swap width and height.
func applyEXIFOrientation(src *image.NRGBA, orientation int) *image.NRGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()

	var dw, dh int
	var mapXY func(dx, dy int) (sx, sy int)

	switch orientation {
	case exifOrientationFlipHorizontal:
		dw, dh = w, h
		mapXY = func(dx, dy int) (int, int) { return w - 1 - dx, dy }
	case exifOrientationRotate180:
		dw, dh = w, h
		mapXY = func(dx, dy int) (int, int) { return w - 1 - dx, h - 1 - dy }
	case exifOrientationFlipVertical:
		dw, dh = w, h
		mapXY = func(dx, dy int) (int, int) { return dx, h - 1 - dy }
	case exifOrientationTranspose:
		dw, dh = h, w
		mapXY = func(dx, dy int) (int, int) { return dy, dx }
	case exifOrientationRotate90CW:
		dw, dh = h, w
		mapXY = func(dx, dy int) (int, int) { return dy, h - 1 - dx }
	case exifOrientationTransverse:
		dw, dh = h, w
		mapXY = func(dx, dy int) (int, int) { return w - 1 - dy, h - 1 - dx }
	case exifOrientationRotate270CW:
		dw, dh = h, w
		mapXY = func(dx, dy int) (int, int) { return w - 1 - dy, dx }
	default:
		return src
	}

	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		for dx := 0; dx < dw; dx++ {
			sx, sy := mapXY(dx, dy)
			sOff := sy*src.Stride + sx*4
			dOff := dy*dst.Stride + dx*4
			copy(dst.Pix[dOff:dOff+4], src.Pix[sOff:sOff+4])
		}
	}
	return dst
}

// exifOrientation extracts the EXIF orientation (1-8) from raw JPEG data.
// It returns exifOrientationNormal when no valid orientation tag exists.
// The scan is defensive: any malformed structure yields the default.
func exifOrientation(data []byte) int {
	const (
		markerPrefix = 0xFF
		markerAPP1   = 0xE1
		markerSOS    = 0xDA
	)
	if len(data) < 4 || data[0] != markerPrefix || data[1] != 0xD8 {
		return exifOrientationNormal
	}

	pos := 2 // skip SOI
	for pos+4 <= len(data) {
		if data[pos] != markerPrefix {
			return exifOrientationNormal
		}
		marker := data[pos+1]
		pos += 2

		// Standalone markers (RSTn, TEM, SOI, EOI) carry no length.
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD9) {
			continue
		}
		if pos+2 > len(data) {
			return exifOrientationNormal
		}
		segLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		if segLen < 2 || pos+segLen > len(data) {
			return exifOrientationNormal
		}
		if marker == markerAPP1 {
			if orientation, ok := parseEXIFPayload(data[pos+2 : pos+segLen]); ok {
				return orientation
			}
		}
		if marker == markerSOS {
			return exifOrientationNormal // image data starts; stop scanning
		}
		pos += segLen
	}
	return exifOrientationNormal
}

// parseEXIFPayload parses an APP1 payload ("Exif\0\0" + TIFF) and returns
// the orientation tag value if present and valid.
func parseEXIFPayload(payload []byte) (int, bool) {
	const exifHeader = "Exif\x00\x00"
	if len(payload) < len(exifHeader)+8 || string(payload[:len(exifHeader)]) != exifHeader {
		return 0, false
	}
	tiff := payload[len(exifHeader):]

	var order binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		order = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		order = binary.BigEndian
	default:
		return 0, false
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 0, false
	}

	ifd0 := int(order.Uint32(tiff[4:8]))
	if ifd0 < 0 || ifd0+2 > len(tiff) {
		return 0, false
	}
	count := int(order.Uint16(tiff[ifd0 : ifd0+2]))
	entries := tiff[ifd0+2:]
	if count*12 > len(entries) {
		return 0, false
	}

	const (
		tagOrientation = 0x0112
		typeShort      = 3
	)
	for i := 0; i < count; i++ {
		e := entries[i*12:]
		if order.Uint16(e[0:2]) != tagOrientation {
			continue
		}
		if order.Uint16(e[2:4]) != typeShort || order.Uint32(e[4:8]) < 1 {
			return 0, false
		}
		orientation := int(order.Uint16(e[8:10]))
		if orientation >= exifOrientationNormal && orientation <= exifOrientationRotate270CW {
			return orientation, true
		}
		return 0, false
	}
	return 0, false
}
