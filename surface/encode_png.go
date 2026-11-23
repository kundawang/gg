// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package surface

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"io"

	"compress/zlib"
)

// Errors returned by ImageSurface.EncodePNGRegion.
var (
	// ErrNilWriter is returned when the destination writer is nil.
	ErrNilWriter = errors.New("surface: cannot encode PNG region: nil writer")

	// ErrEncodeSurfaceClosed is returned when encoding from a closed surface.
	ErrEncodeSurfaceClosed = errors.New("surface: cannot encode PNG region: surface is closed")

	// ErrEmptyRegion is returned when the region has zero or negative size.
	// PNG does not permit zero-width or zero-height images, so an empty
	// region can never produce a valid stream.
	ErrEmptyRegion = errors.New("surface: cannot encode PNG region: region is empty")

	// ErrRegionOutOfBounds is returned when the region is not fully
	// contained within the surface bounds.
	ErrRegionOutOfBounds = errors.New("surface: cannot encode PNG region: region out of bounds")
)

// pngSignature is the 8-byte PNG file magic.
var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// idatChunkSize bounds the amount of compressed data buffered before an
// IDAT chunk is flushed to the writer. Encoding memory stays O(row+chunk)
// regardless of region size.
const idatChunkSize = 32 * 1024

// RegionEncodeOptions configures ImageSurface.EncodePNGRegion.
type RegionEncodeOptions struct {
	// OnRow, if non-nil, is called after each scanline has been encoded,
	// with the 0-based row index relative to the top of the region.
	//
	// The callback may block to apply backpressure (the encoder waits),
	// or return a non-nil error to abort encoding; that error is returned
	// from EncodePNGRegion and the output stream is left incomplete.
	OnRow func(row int) error
}

// EncodePNGRegion streams a rectangular region of the surface to w as a PNG
// image, without copying the full canvas.
//
// The region is expressed in surface-local coordinates: (0, 0) is the
// top-left corner of the surface and (Width(), Height()) the bottom-right,
// regardless of the bounds of the underlying image. The region must be
// non-empty and fully contained within the surface.
//
// The output is a deterministic, non-interlaced 8-bit RGBA PNG (color type
// 6, filter None) readable by any standard PNG decoder. Premultiplied
// source pixels are converted to the non-premultiplied form PNG requires,
// matching png.Encode semantics for *image.RGBA. The source pixels are
// never modified.
//
// Encoding is incremental: at most one scanline plus a fixed-size chunk
// buffer is held in memory, so peak usage does not scale with region area.
// The context is checked once per row; canceling it aborts encoding and
// returns the context error. A failing writer aborts encoding immediately
// with the writer's error. In both cases the stream written so far is
// incomplete and should be discarded by the caller.
//
// A nil ctx is treated as context.Background().
func (s *ImageSurface) EncodePNGRegion(ctx context.Context, w io.Writer, region image.Rectangle, opts *RegionEncodeOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if w == nil {
		return ErrNilWriter
	}
	if s.closed || s.img == nil {
		return ErrEncodeSurfaceClosed
	}
	if region.Dx() <= 0 || region.Dy() <= 0 {
		return ErrEmptyRegion
	}
	full := image.Rect(0, 0, s.width, s.height)
	if !region.In(full) {
		return fmt.Errorf("%w: %v not within %v", ErrRegionOutOfBounds, region, full)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var onRow func(row int) error
	if opts != nil {
		onRow = opts.OnRow
	}

	width, height := region.Dx(), region.Dy()

	// PNG signature.
	if err := writeFull(w, pngSignature); err != nil {
		return fmt.Errorf("surface: png region encode: writing signature: %w", err)
	}

	// IHDR: 8-bit RGBA (color type 6), deflate, no interlace.
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(width))  //nolint:gosec // G115: width > 0 and bounded by surface
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(height)) //nolint:gosec // G115: height > 0 and bounded by surface
	ihdr[8] = 8                                           // bit depth
	ihdr[9] = 6                                           // color type: truecolor with alpha
	if err := writePNGChunk(w, "IHDR", ihdr[:]); err != nil {
		return err
	}

	// Compressed image data, emitted as bounded IDAT chunks.
	idat := &idatChunkWriter{w: w, buf: make([]byte, 0, idatChunkSize)}
	zw := zlib.NewWriter(idat)

	// Reused scanline buffer: 1 filter byte + 4 bytes per pixel.
	row := make([]byte, 1+4*width)
	imgBounds := s.img.Bounds()
	srcX := imgBounds.Min.X + region.Min.X
	srcY0 := imgBounds.Min.Y + region.Min.Y

	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Read the source scanline (never mutated) and unpremultiply into
		// the row buffer. PixOffset handles non-zero origins and stride.
		off := s.img.PixOffset(srcX, srcY0+y)
		src := s.img.Pix[off : off+4*width : off+4*width]
		row[0] = 0 // filter: None
		unpremultiplyRow(src, row[1:])

		if _, err := zw.Write(row); err != nil {
			return fmt.Errorf("surface: png region encode: row %d: %w", y, err)
		}

		if onRow != nil {
			if err := onRow(y); err != nil {
				return err
			}
		}
	}

	if err := zw.Close(); err != nil {
		return fmt.Errorf("surface: png region encode: finishing stream: %w", err)
	}
	if err := idat.Flush(); err != nil {
		return err
	}
	return writePNGChunk(w, "IEND", nil)
}

// unpremultiplyRow converts one scanline of premultiplied RGBA source
// pixels to the non-premultiplied form PNG requires, matching the image/png
// encoder. src and dst must both have length 4*width; they must not overlap.
func unpremultiplyRow(src, dst []byte) {
	for i := 0; i+3 < len(src); i += 4 {
		a := src[i+3]
		switch a {
		case 0:
			// Fully transparent: premultiplied color channels are zero
			// by definition (matches the image/png encoder).
			dst[i+0] = 0
			dst[i+1] = 0
			dst[i+2] = 0
		case 255:
			dst[i+0] = src[i+0]
			dst[i+1] = src[i+1]
			dst[i+2] = src[i+2]
		default:
			// Premultiplied -> non-premultiplied, matching the
			// image/png encoder: c16 = c8*0x101, then
			// n8 = (c16*0xffff/a16) >> 8 == (c8*0xffff/a8) >> 8.
			ua := uint32(a)
			dst[i+0] = uint8((uint32(src[i+0]) * 0xffff / ua) >> 8) //nolint:gosec // G115: result <= 0xffff>>8
			dst[i+1] = uint8((uint32(src[i+1]) * 0xffff / ua) >> 8) //nolint:gosec // G115: result <= 0xffff>>8
			dst[i+2] = uint8((uint32(src[i+2]) * 0xffff / ua) >> 8) //nolint:gosec // G115: result <= 0xffff>>8
		}
		dst[i+3] = a
	}
}

// idatChunkWriter buffers compressed data and emits it to the underlying
// writer as IDAT chunks of at most idatChunkSize bytes.
type idatChunkWriter struct {
	w   io.Writer
	buf []byte
}

// Write appends p to the chunk buffer, flushing full chunks as needed.
func (c *idatChunkWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if len(c.buf) == idatChunkSize {
			if err := c.Flush(); err != nil {
				return n - len(p), err
			}
		}
		k := min(idatChunkSize-len(c.buf), len(p))
		c.buf = append(c.buf, p[:k]...)
		p = p[k:]
	}
	return n, nil
}

// Flush emits any buffered data as a final IDAT chunk.
func (c *idatChunkWriter) Flush() error {
	if len(c.buf) == 0 {
		return nil
	}
	err := writePNGChunk(c.w, "IDAT", c.buf)
	c.buf = c.buf[:0]
	return err
}

// writePNGChunk writes one PNG chunk: length, type, data, CRC-32.
func writePNGChunk(w io.Writer, typ string, data []byte) error {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(data))) //nolint:gosec // G115: chunk data is bounded
	copy(hdr[4:8], typ)
	if err := writeFull(w, hdr[:]); err != nil {
		return fmt.Errorf("surface: png region encode: writing %s chunk header: %w", typ, err)
	}
	if len(data) > 0 {
		if err := writeFull(w, data); err != nil {
			return fmt.Errorf("surface: png region encode: writing %s chunk data: %w", typ, err)
		}
	}
	crc := crc32.NewIEEE()
	_, _ = crc.Write(hdr[4:8])
	_, _ = crc.Write(data)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], crc.Sum32())
	if err := writeFull(w, sum[:]); err != nil {
		return fmt.Errorf("surface: png region encode: writing %s chunk CRC: %w", typ, err)
	}
	return nil
}

// writeFull writes all of p, treating a short write without an error as
// io.ErrShortWrite.
func writeFull(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return err
}
