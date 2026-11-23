package recording

// This file implements the writing side of the recording stream protocol.
//
// StreamWriter serializes canvas metadata, resources, and commands into a
// sequence of self-describing frames and hands them to an io.Writer as they
// are produced, so a long recording never has to be materialized in memory.
// Attach a recorder with StreamTo; the recorder then mirrors every command
// into the stream, and Finish seals the stream with an end frame and a
// whole-stream checksum.

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"io"
	"math"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
)

// StreamWriter serializes a recording into an io.Writer incrementally.
//
// Each resource and command is a separate framed chunk written to the
// underlying writer as soon as it is recorded. The stream begins with a canvas
// frame and ends with an end frame carrying resource/command counts and a
// checksum over the whole stream. Resources are referenced by stable IDs:
// paths, brushes, and images use their resource-pool index; fonts use IDs
// assigned as faces are first seen.
//
// A StreamWriter is not safe for concurrent use.
type StreamWriter struct {
	w   io.Writer
	cfg StreamConfig
	bo  binary.ByteOrder
	enc encoder

	chunk      uint64 // frames fully written
	commandNum uint64 // command frames written
	offset     int64  // bytes handed to w
	streamCRC  uint32 // running CRC over every payload

	// nextID is the next stream ID to assign per kind, indexed by kind index.
	// IDs are monotonic for the whole stream even when Recorder.Reset restarts
	// the in-memory pool indices.
	nextID [5]uint32

	// emittedIndex mirrors the resource-pool indices already written for the
	// current pool epoch. They restart at zero on Reset.
	emittedIndex [3]uint32

	// fontDataIDs dedupes raw font blobs by backing FontSource.
	fontDataIDs map[*text.FontSource]uint32
	// faceIDs dedupes font resources by face identity.
	faceIDs map[text.Face]uint32

	rec      *Recorder
	finished bool
	err      error
}

// resource slot indices in StreamWriter.nextID.
const (
	slotPath = iota
	slotBrush
	slotImage
	slotFont
	slotFontData
)

// pool index cursor slots in StreamWriter.emittedIndex, aligned with
// encoder.idOf slots (path, brush, image).
const (
	cursorPath = iota
	cursorBrush
	cursorImage
)

// NewStreamWriter creates a StreamWriter that writes to w.
// A nil cfg selects the defaults. The canvas frame is written when the writer
// is attached to a recorder via Recorder.StreamTo or Recording.StreamTo.
func NewStreamWriter(w io.Writer, cfg *StreamConfig) *StreamWriter {
	c := cfg.normalized()
	bo := binary.ByteOrder(binary.LittleEndian)
	if c.BigEndian {
		bo = binary.BigEndian
	}
	sw := &StreamWriter{
		w:           w,
		cfg:         c,
		bo:          bo,
		enc:         encoder{bo: bo, faceIDs: make(map[text.Face]uint32)},
		fontDataIDs: make(map[*text.FontSource]uint32),
		faceIDs:     make(map[text.Face]uint32),
	}
	for i := range sw.enc.idOf {
		sw.enc.idOf[i] = make(map[uint32]uint32)
	}
	if w == nil {
		sw.err = fmt.Errorf("%w: nil writer", ErrInvalidData)
	}
	sw.streamCRC = seedCRC(StreamVersion)
	return sw
}

// seedCRC seeds the running checksum so streams of different versions differ.
func seedCRC(version uint16) uint32 {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], version)
	return crc32.Update(0, crcTable, b[:])
}

// updateCRC extends a running CRC-32C.
func updateCRC(crc uint32, p []byte) uint32 { return crc32.Update(crc, crcTable, p) }

// checkCancel returns ErrCanceled if the configured cancel channel is closed.
func (w *StreamWriter) checkCancel() error {
	if w.cfg.Cancel == nil {
		return nil
	}
	select {
	case <-w.cfg.Cancel:
		return ErrCanceled
	default:
		return nil
	}
}

// attachSize binds the writer to fixed canvas dimensions without a live
// recorder. It is used by Recording.StreamTo, which writes a finished
// recording rather than one being recorded.
func (w *StreamWriter) attachSize(width, height int) error {
	if w.err != nil {
		return w.err
	}
	if w.finished {
		return ErrStreamFinished
	}
	if w.rec != nil || w.chunk != 0 {
		return ErrAlreadyStreaming
	}
	if err := w.WriteCanvas(width, height); err != nil {
		return err
	}
	w.rec = &Recorder{width: width, height: height}
	return nil
}

// errAt builds a StreamError for the current position.
func (w *StreamWriter) errAt(kind string, command uint64, err error) error {
	return &StreamError{
		Op:      "write",
		Chunk:   w.chunk + 1,
		Command: command,
		Offset:  w.offset,
		Kind:    kind,
		Err:     err,
	}
}

// writeFull writes all of p, honoring cancellation and retrying short writes.
func (w *StreamWriter) writeFull(p []byte) error {
	for len(p) > 0 {
		if err := w.checkCancel(); err != nil {
			return err
		}
		n, err := w.w.Write(p)
		if n > 0 {
			w.offset += int64(n)
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// writeFrame writes one frame: a 16-byte header followed by the payload.
func (w *StreamWriter) writeFrame(kind frameKind, command uint64, payload []byte) error {
	if err := w.checkCancel(); err != nil {
		w.err = w.errAt(kind.String(), command, err)
		return w.err
	}
	if uint64(len(payload)) > uint64(w.cfg.MaxFrameSize) {
		w.err = w.errAt(kind.String(), command,
			fmt.Errorf("%w: %d bytes > %d", ErrLengthTooLarge, len(payload), w.cfg.MaxFrameSize))
		return w.err
	}
	sum := streamCRC(payload)

	frame := make([]byte, frameHeaderSize+len(payload))
	hdr := frame[:frameHeaderSize]
	copy(hdr[0:4], streamMagic)
	w.bo.PutUint16(hdr[4:6], StreamVersion)
	hdr[6] = endianTag(w.bo)
	hdr[7] = byte(kind)
	w.bo.PutUint32(hdr[8:12], uint32(len(payload)))
	w.bo.PutUint32(hdr[12:16], sum)
	copy(frame[frameHeaderSize:], payload)

	if err := w.writeFull(frame); err != nil {
		w.err = w.errAt(kind.String(), command, err)
		return w.err
	}
	w.chunk++
	w.streamCRC = updateCRC(w.streamCRC, payload)
	return nil
}

// WriteCanvas writes the opening canvas frame. It is called automatically by
// StreamTo; calling it directly is only necessary for hand-built streams.
func (w *StreamWriter) WriteCanvas(width, height int) error {
	if w.err != nil {
		return w.err
	}
	if w.finished {
		return w.errAt("canvas", 0, ErrStreamFinished)
	}
	if w.chunk != 0 {
		return w.errAt("canvas", 0, fmt.Errorf("%w: canvas frame must be first", ErrInvalidData))
	}
	if width < 0 || height < 0 || int64(width) > math.MaxInt32 || int64(height) > math.MaxInt32 {
		return w.errAt("canvas", 0, fmt.Errorf("%w: canvas %dx%d", ErrInvalidData, width, height))
	}
	w.enc.reset()
	w.enc.f64(float64(width))
	w.enc.f64(float64(height))
	return w.writeFrame(frameCanvas, 0, w.enc.buf)
}

// attach binds the writer to a recorder, writing the canvas frame on first use.
func (w *StreamWriter) attach(rec *Recorder) error {
	if w.err != nil {
		return w.err
	}
	if w.finished {
		return ErrStreamFinished
	}
	if w.rec != nil && w.rec != rec {
		return ErrAlreadyStreaming
	}
	if w.rec == nil {
		if err := w.WriteCanvas(rec.width, rec.height); err != nil {
			return err
		}
		w.rec = rec
	}
	return nil
}

// writeResourceFrame emits a resource frame with the given kind and ID.
func (w *StreamWriter) writeResourceFrame(kind resourceKind, id uint32, encode func(*encoder)) error {
	if w.finished {
		w.err = w.errAt(kind.String(), 0, ErrStreamFinished)
		return w.err
	}
	w.enc.reset()
	w.enc.u8(uint8(kind))
	w.enc.uvarint(uint64(id))
	encode(&w.enc)
	if w.enc.err != nil {
		return w.errAt(kind.String(), 0, w.enc.err)
	}
	return w.writeFrame(frameResource, 0, w.enc.buf)
}

// emitPath writes the path at pool index idx if not already emitted in the
// current pool epoch, assigning it a fresh monotonic stream ID.
func (w *StreamWriter) emitPath(idx uint32, path *gg.Path) error {
	if w.emittedIndex[cursorPath] > idx {
		return nil // already emitted in this epoch
	}
	if w.emittedIndex[cursorPath] != idx {
		return w.errAt("path", 0,
			fmt.Errorf("%w: path index %d, expected %d", ErrInvalidData, idx, w.emittedIndex[cursorPath]))
	}
	id := w.nextID[slotPath]
	if err := w.checkLimit(resPath, id); err != nil {
		return err
	}
	if err := w.writeResourceFrame(resPath, id, func(e *encoder) { e.path(path) }); err != nil {
		return err
	}
	w.nextID[slotPath]++
	w.emittedIndex[cursorPath]++
	w.enc.idOf[idPath][idx] = id
	return nil
}

// emitBrush writes the brush at pool index idx.
func (w *StreamWriter) emitBrush(idx uint32, brush Brush) error {
	if w.emittedIndex[cursorBrush] > idx {
		return nil
	}
	if w.emittedIndex[cursorBrush] != idx {
		return w.errAt("brush", 0,
			fmt.Errorf("%w: brush index %d, expected %d", ErrInvalidData, idx, w.emittedIndex[cursorBrush]))
	}
	id := w.nextID[slotBrush]
	if err := w.checkLimit(resBrush, id); err != nil {
		return err
	}
	if err := w.writeResourceFrame(resBrush, id, func(e *encoder) { e.brush(brush) }); err != nil {
		return err
	}
	w.nextID[slotBrush]++
	w.emittedIndex[cursorBrush]++
	w.enc.idOf[idBrush][idx] = id
	return nil
}

// emitImage writes the image at pool index idx.
func (w *StreamWriter) emitImage(idx uint32, img image.Image) error {
	if w.emittedIndex[cursorImage] > idx {
		return nil
	}
	if w.emittedIndex[cursorImage] != idx {
		return w.errAt("image", 0,
			fmt.Errorf("%w: image index %d, expected %d", ErrInvalidData, idx, w.emittedIndex[cursorImage]))
	}
	id := w.nextID[slotImage]
	if err := w.checkLimit(resImage, id); err != nil {
		return err
	}
	if err := w.writeResourceFrame(resImage, id, func(e *encoder) { e.image(img) }); err != nil {
		return err
	}
	w.nextID[slotImage]++
	w.emittedIndex[cursorImage]++
	w.enc.idOf[idImage][idx] = id
	return nil
}

// checkLimit rejects a resource ID at or above the configured limit.
func (w *StreamWriter) checkLimit(kind resourceKind, id uint32) error {
	if id >= w.cfg.MaxResources {
		return w.errAt(kind.String(), 0,
			fmt.Errorf("%w: id %d >= %d", ErrLengthTooLarge, id, w.cfg.MaxResources))
	}
	return nil
}

// syncResources emits every resource the pool has that the stream has not
// seen in the current epoch, in dependency order: images before brushes
// (pattern brushes reference images), then paths and fonts. Called before
// each command.
func (w *StreamWriter) syncResources(pool *ResourcePool) error {
	if w.finished {
		w.err = w.errAt("resource", w.commandNum+1, ErrStreamFinished)
		return w.err
	}
	for int(w.emittedIndex[cursorImage]) < len(pool.images) {
		idx := w.emittedIndex[cursorImage]
		if err := w.emitImage(idx, pool.images[idx]); err != nil {
			return err
		}
	}
	for int(w.emittedIndex[cursorBrush]) < len(pool.brushes) {
		idx := w.emittedIndex[cursorBrush]
		if err := w.emitBrush(idx, pool.brushes[idx]); err != nil {
			return err
		}
	}
	for int(w.emittedIndex[cursorPath]) < len(pool.paths) {
		idx := w.emittedIndex[cursorPath]
		if err := w.emitPath(idx, pool.paths[idx]); err != nil {
			return err
		}
	}
	for _, face := range pool.fonts {
		if err := w.emitFont(face); err != nil {
			return err
		}
	}
	return nil
}

// emitFont writes the font-data and font resources for a face if not already
// emitted. Faces that share a FontSource share font data. Faces are keyed by
// identity rather than pool index because text commands reference the face
// directly (the pool may not contain it).
func (w *StreamWriter) emitFont(face text.Face) error {
	if face == nil {
		return nil
	}
	if _, ok := w.faceIDs[face]; ok {
		return nil
	}
	raw, err := fontData(face)
	if err != nil {
		return w.errAt("font", 0, err)
	}
	src := face.Source()

	dataID, ok := w.fontDataIDs[src]
	if !ok {
		dataID = w.nextID[slotFontData]
		if err := w.checkLimit(resFontData, dataID); err != nil {
			return err
		}
		if err := w.writeResourceFrame(resFontData, dataID, func(e *encoder) { e.bytes(raw) }); err != nil {
			return err
		}
		w.nextID[slotFontData]++
		w.fontDataIDs[src] = dataID
	}

	id := w.nextID[slotFont]
	if err := w.checkLimit(resFont, id); err != nil {
		return err
	}
	if err := w.writeResourceFrame(resFont, id, func(e *encoder) { e.face(face, dataID) }); err != nil {
		return err
	}
	w.nextID[slotFont]++
	w.faceIDs[face] = id
	w.enc.faceIDs[face] = id
	return nil
}

// emitCommand writes one command frame, registering any font it references.
func (w *StreamWriter) emitCommand(cmd Command) error {
	if w.err != nil {
		return w.err
	}
	if w.finished {
		w.err = w.errAt("command", w.commandNum+1, ErrStreamFinished)
		return w.err
	}
	// Text commands reference a face directly; register it before encoding
	// translates the reference. emitFont writes resource frames and reuses
	// the encoder buffer, so it must run before the command buffer is reset.
	if err := w.emitFont(commandFace(cmd)); err != nil {
		return err
	}
	w.enc.reset()
	cmdNum := w.commandNum + 1
	if err := encodeCommand(&w.enc, cmd); err != nil {
		w.err = w.errAt("command", cmdNum, err)
		return w.err
	}
	if err := w.writeFrame(frameCommand, cmdNum, w.enc.buf); err != nil {
		return err
	}
	w.commandNum = cmdNum
	return nil
}

// commandFace returns the face referenced by a text command, or nil.
func commandFace(cmd Command) text.Face {
	switch c := cmd.(type) {
	case DrawTextCommand:
		return c.Face
	case StrokeTextCommand:
		return c.Face
	}
	return nil
}

// Finish writes the end frame and seals the stream. A second call reports
// ErrStreamFinished.
func (w *StreamWriter) Finish() error {
	if w.err != nil {
		return w.err
	}
	if w.finished {
		return ErrStreamFinished
	}
	if w.rec == nil && w.chunk == 0 {
		return w.errAt("end", 0, fmt.Errorf("%w: stream never started", ErrInvalidData))
	}
	w.enc.reset()
	w.enc.uvarint(endMarkerTag)
	w.enc.uvarint(uint64(w.nextID[slotPath]))
	w.enc.uvarint(uint64(w.nextID[slotBrush]))
	w.enc.uvarint(uint64(w.nextID[slotImage]))
	w.enc.uvarint(uint64(w.nextID[slotFont]))
	w.enc.uvarint(uint64(w.nextID[slotFontData]))
	w.enc.uvarint(w.commandNum)
	w.enc.u32(w.streamCRC)
	if err := w.writeFrame(frameEnd, 0, w.enc.buf); err != nil {
		return err
	}
	w.finished = true
	return nil
}

// endMarkerTag identifies a well-formed end frame payload.
const endMarkerTag uint64 = 0xC0DE

// CanvasSize returns the dimensions being streamed, or (0, 0) before attach.
func (w *StreamWriter) CanvasSize() (width, height int) {
	if w.rec == nil {
		return 0, 0
	}
	return w.rec.width, w.rec.height
}

// CommandsWritten returns the number of command frames written so far.
func (w *StreamWriter) CommandsWritten() uint64 { return w.commandNum }

// BytesWritten returns the total number of bytes handed to the underlying
// writer.
func (w *StreamWriter) BytesWritten() int64 { return w.offset }

// Err returns the first error encountered by the writer, if any.
func (w *StreamWriter) Err() error { return w.err }

// Finished reports whether Finish completed.
func (w *StreamWriter) Finished() bool { return w.finished }

// resetResourceCursor restarts resource-pool cursors after a Recorder.Reset
// cleared the pools. Stream IDs keep growing monotonically, so a reader that
// consumes the stream end-to-end sees consistent definitions throughout.
func (w *StreamWriter) resetResourceCursor() {
	w.emittedIndex = [3]uint32{}
}
