package recording

// This file implements the reading side of the recording stream protocol.
//
// StreamReader consumes frames incrementally from an io.Reader. It validates
// each header and payload checksum, resolves resources by ID as they arrive,
// and can either deliver commands one at a time via Next (constant memory) or
// assemble a complete Recording via ReadAll (memory proportional to the
// recording). The end frame's whole-stream checksum catches reordered,
// dropped, or spliced frames.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/gogpu/gg/text"
)

// StreamReader incrementally decodes a recording stream.
//
// Use Next to consume commands as they arrive, or ReadAll to build an
// in-memory Recording. Resource frames are always consumed transparently: a
// call to Next returns the next command, having read and registered any
// resources (and skipped canvas/end bookkeeping) that precede it.
//
// A StreamReader is not safe for concurrent use.
type StreamReader struct {
	r   io.Reader
	cfg StreamConfig
	bo  binary.ByteOrder

	chunk  uint64 // frames read
	cmdNum uint64 // commands read
	offset int64  // bytes read
	runCRC uint32 // running CRC over every payload

	canvasSeen bool
	ended      bool
	width      int
	height     int

	res    *resourceStore
	pool   *ResourcePool
	hdrBuf [frameHeaderSize]byte

	// retained commands when RetainCommands is set or ReadAll is used.
	retain bool
	cmds   []Command

	// counts declared by the end frame, validated against what was read.
	endCounts endFrameCounts

	err error
}

// endFrameCounts is the resource/command tally carried by the end frame.
type endFrameCounts struct {
	paths, brushes, images, fonts, fontData, commands uint64
	crc                                               uint32
}

// NewStreamReader creates a StreamReader that consumes the stream in r.
// A nil cfg selects the defaults.
func NewStreamReader(r io.Reader, cfg *StreamConfig) *StreamReader {
	c := cfg.normalized()
	sr := &StreamReader{
		r:      r,
		cfg:    c,
		bo:     binary.LittleEndian,
		res:    newResourceStore(),
		pool:   NewResourcePool(),
		retain: c.RetainCommands,
	}
	if r == nil {
		sr.err = fmt.Errorf("%w: nil reader", ErrInvalidData)
	}
	sr.runCRC = seedCRC(StreamVersion)
	return sr
}

// errAt builds a StreamError for the current position.
func (sr *StreamReader) errAt(kind string, command uint64, err error) error {
	return &StreamError{
		Op:      "read",
		Chunk:   sr.chunk + 1,
		Command: command,
		Offset:  sr.offset,
		Kind:    kind,
		Err:     err,
	}
}

// checkCancel returns ErrCanceled if the configured cancel channel is closed.
func (sr *StreamReader) checkCancel() error {
	if sr.cfg.Cancel == nil {
		return nil
	}
	select {
	case <-sr.cfg.Cancel:
		return ErrCanceled
	default:
		return nil
	}
}

// readFull fills p entirely or reports the underlying error.
func (sr *StreamReader) readFull(p []byte) error {
	for len(p) > 0 {
		if err := sr.checkCancel(); err != nil {
			return err
		}
		n, err := sr.r.Read(p)
		if n > 0 {
			sr.offset += int64(n)
			p = p[n:]
		}
		if err != nil {
			if err == io.EOF && len(p) > 0 {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

// readFrameHeader reads and validates the next frame header, returning its
// kind, payload length, checksum, and byte order.
func (sr *StreamReader) readFrameHeader() (frameKind, uint32, uint32, binary.ByteOrder, error) {
	n, err := io.ReadFull(sr.r, sr.hdrBuf[:])
	sr.offset += int64(n)
	if err != nil {
		if errors.Is(err, io.EOF) && n == 0 {
			return 0, 0, 0, nil, io.EOF
		}
		if errors.Is(err, io.EOF) {
			return 0, 0, 0, nil, io.ErrUnexpectedEOF
		}
		return 0, 0, 0, nil, err
	}

	h := sr.hdrBuf[:]
	if string(h[0:4]) != streamMagic {
		return 0, 0, 0, nil, fmt.Errorf("%w: bad magic %q", ErrInvalidHeader, h[0:4])
	}
	// The endianness tag is a single byte and readable before the byte order
	// is known; validate it before interpreting the multi-byte fields.
	bo, ok := byteOrder(h[6])
	if !ok {
		return 0, 0, 0, nil, fmt.Errorf("%w: endianness 0x%02x", ErrInvalidHeader, h[6])
	}
	if version := bo.Uint16(h[4:6]); version != StreamVersion {
		return 0, 0, 0, nil, fmt.Errorf("%w: version %d, want %d", ErrInvalidHeader, version, StreamVersion)
	}
	kind := frameKind(h[7])
	switch kind {
	case frameCanvas, frameResource, frameCommand, frameEnd:
	default:
		return 0, 0, 0, nil, fmt.Errorf("%w: frame kind 0x%02x", ErrUnknownTag, h[7])
	}
	length := bo.Uint32(h[8:12])
	if length > sr.cfg.MaxFrameSize {
		return 0, 0, 0, nil,
			fmt.Errorf("%w: frame length %d > %d", ErrLengthTooLarge, length, sr.cfg.MaxFrameSize)
	}
	sum := bo.Uint32(h[12:16])
	return kind, length, sum, bo, nil
}

// readPayload reads a frame payload of the given length and verifies its CRC.
func (sr *StreamReader) readPayload(length uint32, sum uint32, kind frameKind) ([]byte, error) {
	if length == 0 {
		if sum != 0 && sum != streamCRC(nil) {
			return nil, fmt.Errorf("%w: %s payload checksum", ErrChecksum, kind)
		}
		return nil, nil
	}
	buf := make([]byte, length)
	if err := sr.readFull(buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: %s frame", ErrTruncated, kind)
		}
		return nil, err
	}
	if got := streamCRC(buf); got != sum {
		return nil, fmt.Errorf("%w: %s frame (got %08x want %08x)", ErrChecksum, kind, got, sum)
	}
	return buf, nil
}

// ensureCanvas validates that the first frame was a canvas frame.
func (sr *StreamReader) ensureCanvas(kind frameKind) error {
	if sr.canvasSeen {
		return nil
	}
	if kind != frameCanvas {
		return fmt.Errorf("%w: first frame is %s, want canvas", ErrInvalidHeader, kind)
	}
	return nil
}

// Next reads frames until it produces the next command. It returns
// io.EOF after a well-formed end frame. Resources encountered before the
// command are registered; canvas and end frames are handled inline.
func (sr *StreamReader) Next() (Command, error) {
	if sr.err != nil {
		return nil, sr.err
	}
	if sr.ended {
		return nil, io.EOF
	}
	for {
		if err := sr.checkCancel(); err != nil {
			sr.err = sr.errAt("", sr.cmdNum+1, err)
			return nil, sr.err
		}
		kind, length, sum, bo, err := sr.readFrameHeader()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// EOF at a frame boundary before an end frame.
				if !sr.ended {
					sr.err = sr.errAt("", sr.cmdNum+1, ErrUnfinishedStream)
					return nil, sr.err
				}
				return nil, io.EOF
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				sr.err = sr.errAt("", sr.cmdNum+1, ErrTruncated)
				return nil, sr.err
			}
			sr.err = sr.errAt("", sr.cmdNum+1, err)
			return nil, sr.err
		}
		sr.bo = bo

		if err := sr.ensureCanvas(kind); err != nil {
			sr.err = sr.errAt(kind.String(), sr.cmdNum+1, err)
			return nil, sr.err
		}

		payload, err := sr.readPayload(length, sum, kind)
		if err != nil {
			sr.err = sr.errAt(kind.String(), sr.cmdNum+1, err)
			return nil, sr.err
		}
		sr.chunk++
		// The running checksum covers every frame before the end frame; the
		// end frame carries that checksum rather than contributing to it.
		if kind != frameEnd {
			sr.runCRC = updateCRC(sr.runCRC, payload)
		}

		switch kind {
		case frameCanvas:
			if err := sr.handleCanvas(payload); err != nil {
				return nil, sr.fail(kind, 0, err)
			}
		case frameResource:
			if err := sr.handleResource(payload); err != nil {
				return nil, sr.fail(kind, 0, err)
			}
		case frameCommand:
			cmd, err := sr.handleCommand(payload)
			if err != nil {
				return nil, sr.fail(kind, sr.cmdNum+1, err)
			}
			return cmd, nil
		case frameEnd:
			if err := sr.handleEnd(payload); err != nil {
				return nil, sr.fail(kind, 0, err)
			}
			return nil, io.EOF
		}
	}
}

// fail records and returns a stream error.
func (sr *StreamReader) fail(kind frameKind, command uint64, err error) error {
	if err == nil {
		return nil
	}
	sr.err = sr.errAt(kind.String(), command, err)
	return sr.err
}

// handleCanvas decodes the canvas frame.
func (sr *StreamReader) handleCanvas(payload []byte) error {
	if sr.canvasSeen {
		return fmt.Errorf("%w: duplicate canvas frame", ErrInvalidData)
	}
	d := decoder{b: payload, bo: sr.bo, res: sr.res}
	w := d.f64()
	h := d.f64()
	if err := d.done(); err != nil {
		return err
	}
	if w != math.Trunc(w) || h != math.Trunc(h) {
		return fmt.Errorf("%w: non-integer canvas %.6gx%.6g", ErrInvalidData, w, h)
	}
	if w < 0 || h < 0 || w > math.MaxInt32 || h > math.MaxInt32 {
		return fmt.Errorf("%w: canvas %.6gx%.6g", ErrInvalidData, w, h)
	}
	sr.width = int(w)
	sr.height = int(h)
	sr.canvasSeen = true
	return nil
}

// handleResource decodes and registers one resource.
func (sr *StreamReader) handleResource(payload []byte) error {
	d := decoder{b: payload, bo: sr.bo, res: sr.res}
	kind := resourceKind(d.u8())
	rawID := d.uvarint()
	if d.err != nil {
		return d.err
	}
	if rawID >= uint64(sr.cfg.MaxResources) {
		return fmt.Errorf("%w: %s id %d", ErrLengthTooLarge, kind, rawID)
	}
	id := uint32(rawID)
	if kindIndex(kind) < 0 {
		return fmt.Errorf("%w: resource kind 0x%02x", ErrUnknownTag, uint8(kind))
	}

	switch kind {
	case resPath:
		path := d.pathBody()
		if err := d.done(); err != nil {
			return err
		}
		if _, dup := sr.res.paths[id]; dup {
			return fmt.Errorf("%w: path %d", ErrDuplicateResource, id)
		}
		sr.res.paths[id] = path
		sr.res.pathRefs[id] = sr.pool.appendPathAt(path)
	case resBrush:
		brush := d.brushBody()
		if err := d.done(); err != nil {
			return err
		}
		if _, dup := sr.res.brushes[id]; dup {
			return fmt.Errorf("%w: brush %d", ErrDuplicateResource, id)
		}
		sr.res.brushes[id] = brush
		sr.res.brushRefs[id] = sr.pool.appendBrushAt(brush)
	case resImage:
		img := d.imageBody()
		if err := d.done(); err != nil {
			return err
		}
		if _, dup := sr.res.images[id]; dup {
			return fmt.Errorf("%w: image %d", ErrDuplicateResource, id)
		}
		sr.res.images[id] = img
		sr.res.imageRefs[id] = sr.pool.appendImageAt(img)
	case resFontData:
		raw := d.raw()
		if err := d.done(); err != nil {
			return err
		}
		if _, dup := sr.res.fontData[id]; dup {
			return fmt.Errorf("%w: font data %d", ErrDuplicateResource, id)
		}
		src, err := text.NewFontSource(raw)
		if err != nil {
			return fmt.Errorf("%w: font data %d: %v", ErrInvalidData, id, err)
		}
		sr.res.fontData[id] = raw
		sr.res.fontSrc[id] = src
	case resFont:
		face, _, err := d.faceBody()
		if err != nil {
			return err
		}
		if err := d.done(); err != nil {
			return err
		}
		if _, dup := sr.res.fonts[id]; dup {
			return fmt.Errorf("%w: font %d", ErrDuplicateResource, id)
		}
		sr.res.fonts[id] = face
		sr.res.fontRefs[id] = sr.pool.appendFontAt(face)
	}
	return nil
}

// handleCommand decodes one command and optionally retains it.
func (sr *StreamReader) handleCommand(payload []byte) (Command, error) {
	d := decoder{b: payload, bo: sr.bo, res: sr.res}
	cmd, err := decodeCommand(&d)
	if err != nil {
		return nil, err
	}
	if err := d.done(); err != nil {
		return nil, err
	}
	sr.cmdNum++
	if sr.retain {
		sr.cmds = append(sr.cmds, cmd)
	}
	return cmd, nil
}

// handleEnd validates the end frame and the whole-stream checksum.
func (sr *StreamReader) handleEnd(payload []byte) error {
	d := decoder{b: payload, bo: sr.bo, res: sr.res}
	if marker := d.uvarint(); marker != endMarkerTag {
		return fmt.Errorf("%w: end marker 0x%x", ErrUnknownTag, marker)
	}
	var c endFrameCounts
	c.paths = d.uvarint()
	c.brushes = d.uvarint()
	c.images = d.uvarint()
	c.fonts = d.uvarint()
	c.fontData = d.uvarint()
	c.commands = d.uvarint()
	c.crc = d.u32()
	if err := d.done(); err != nil {
		return err
	}
	// Check declared counts against what actually arrived.
	if got := uint64(len(sr.res.paths)); got != c.paths {
		return fmt.Errorf("%w: end frame declares %d paths, read %d", ErrInvalidData, c.paths, got)
	}
	if got := uint64(len(sr.res.brushes)); got != c.brushes {
		return fmt.Errorf("%w: end frame declares %d brushes, read %d", ErrInvalidData, c.brushes, got)
	}
	if got := uint64(len(sr.res.images)); got != c.images {
		return fmt.Errorf("%w: end frame declares %d images, read %d", ErrInvalidData, c.images, got)
	}
	if got := uint64(len(sr.res.fonts)); got != c.fonts {
		return fmt.Errorf("%w: end frame declares %d fonts, read %d", ErrInvalidData, c.fonts, got)
	}
	if got := uint64(len(sr.res.fontData)); got != c.fontData {
		return fmt.Errorf("%w: end frame declares %d font data, read %d", ErrInvalidData, c.fontData, got)
	}
	if sr.cmdNum != c.commands {
		return fmt.Errorf("%w: end frame declares %d commands, read %d", ErrInvalidData, c.commands, sr.cmdNum)
	}
	if sr.runCRC != c.crc {
		return fmt.Errorf("%w: stream checksum %08x != %08x", ErrChecksum, sr.runCRC, c.crc)
	}
	sr.ended = true
	sr.endCounts = c
	return nil
}

// CanvasSize returns the decoded canvas dimensions. It is valid after the
// first Next call, whether or not it returned a command.
func (sr *StreamReader) CanvasSize() (width, height int) {
	return sr.width, sr.height
}

// CommandsRead returns the number of command frames read so far.
func (sr *StreamReader) CommandsRead() uint64 { return sr.cmdNum }

// RetainedCommands returns the number of commands the reader is holding in
// memory. It is zero unless RetainCommands is enabled (implicitly by ReadAll,
// which retains after the first command); an incremental Next/Playback loop
// leaves it at zero.
func (sr *StreamReader) RetainedCommands() int { return len(sr.cmds) }

// Totals reports the resource and command counts recorded in the end frame.
// The values are zero until the end frame is consumed (see Done). They match
// what the producer wrote, so a consumer can reconcile partial processing.
func (sr *StreamReader) Totals() StreamTotals {
	return StreamTotals{
		Paths:    sr.endCounts.paths,
		Brushes:  sr.endCounts.brushes,
		Images:   sr.endCounts.images,
		Fonts:    sr.endCounts.fonts,
		FontData: sr.endCounts.fontData,
		Commands: sr.endCounts.commands,
		Checksum: sr.endCounts.crc,
	}
}

// StreamTotals mirrors the counts carried by an end frame.
type StreamTotals struct {
	Paths    uint64
	Brushes  uint64
	Images   uint64
	Fonts    uint64
	FontData uint64
	Commands uint64
	Checksum uint32
}

// Resources returns the resource pool assembled from the stream. Paths,
// brushes, images, and fonts are appended in stream order, so their pool
// indices match the stream IDs.
func (sr *StreamReader) Resources() *ResourcePool { return sr.pool }

// Err returns the first error encountered, if any.
func (sr *StreamReader) Err() error { return sr.err }

// Done reports whether a well-formed end frame was consumed.
func (sr *StreamReader) Done() bool { return sr.ended }

// ReadAll consumes the entire stream and returns an in-memory Recording.
// It fails with ErrPartiallyConsumed if commands were already delivered by
// Next and command retention is disabled.
func (sr *StreamReader) ReadAll() (*Recording, error) {
	if sr.err != nil {
		return nil, sr.err
	}
	if sr.cmdNum > 0 && !sr.retain {
		return nil, &StreamError{Op: "read", Chunk: sr.chunk + 1, Command: sr.cmdNum, Err: ErrPartiallyConsumed}
	}
	sr.retain = true
	for {
		_, err := sr.Next()
		if errors.Is(err, io.EOF) && sr.ended {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return &Recording{
		width:     sr.width,
		height:    sr.height,
		commands:  sr.cmds,
		resources: sr.pool,
	}, nil
}
