package recording

// This file defines the wire format for the recording stream protocol.
//
// # Frame Layout
//
// A stream is a sequence of frames. Every frame starts with a fixed 16-byte
// header, encoded in the byte order declared by the stream:
//
//	offset  size  field
//	0       4     magic "GGRS"
//	4       2     version (uint16, currently 1)
//	6       1     endianness (1 = little-endian, 2 = big-endian)
//	7       1     frame kind (1 = canvas, 2 = resource, 3 = command, 4 = end)
//	8       4     payload length in bytes (uint32)
//	12      4     CRC-32C (Castagnoli) of the payload
//
// The first frame is always a canvas frame carrying the recording dimensions.
// Resource frames define paths, brushes, images, and fonts by explicit ID.
// Command frames carry one drawing command. The final end frame carries the
// stream totals plus a running checksum over every preceding frame payload, so
// reordered, dropped, or spliced frames are detected.
//
// Multi-byte integers inside frame headers use the declared byte order.
// Lengths and IDs inside payloads are unsigned varints (byte-order
// independent); float64/float32 values use the declared byte order, so a
// big-endian producer is readable on a little-endian host and vice versa.
//
// # Robustness
//
// Readers validate the magic, version, endianness, frame kind, payload length
// (against MaxFrameSize), and payload checksum before interpreting any bytes.
// Every length read from a payload is checked against the remaining frame
// bytes before memory is allocated, so a corrupt or hostile stream cannot
// trigger an unbounded allocation or a panic. Errors are returned as
// *StreamError carrying the chunk (frame) number and, where applicable, the
// command number.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"math"
	"strings"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
)

const (
	// StreamVersion is the current wire format version.
	StreamVersion uint16 = 1

	// frameHeaderSize is the size of every frame header in bytes.
	frameHeaderSize = 16

	// streamMagic identifies a gg recording stream. Its first four bytes.
	streamMagic = "GGRS"

	// endianLittle and endianBig are the endianness tags stored in the header.
	endianLittle byte = 0x01
	endianBig    byte = 0x02
)

// DefaultMaxFrameSize is the default upper bound for a single frame payload.
// It bounds a single path, image, or command blob and prevents a corrupt
// length field from causing an unbounded allocation. Images larger than this
// must be split by the caller or the limit raised via StreamConfig.
const DefaultMaxFrameSize uint32 = 64 << 20 // 64 MiB

// DefaultMaxResources is the default upper bound on the number of resources
// of each kind (paths, brushes, images, fonts) a stream may define. It also
// bounds resource IDs, so a stream cannot force a huge sparse allocation.
const DefaultMaxResources uint32 = 1 << 20

// frameKind identifies the payload type of a frame.
type frameKind uint8

const (
	frameCanvas   frameKind = 0x01
	frameResource frameKind = 0x02
	frameCommand  frameKind = 0x03
	frameEnd      frameKind = 0x04
)

// String returns a human-readable name for the frame kind.
func (k frameKind) String() string {
	switch k {
	case frameCanvas:
		return "canvas"
	case frameResource:
		return "resource"
	case frameCommand:
		return "command"
	case frameEnd:
		return "end"
	default:
		return fmt.Sprintf("unknown(0x%02x)", uint8(k))
	}
}

// resourceKind identifies the resource type carried by a resource frame.
type resourceKind uint8

const (
	resPath     resourceKind = 0x01
	resBrush    resourceKind = 0x02
	resImage    resourceKind = 0x03
	resFont     resourceKind = 0x04
	resFontData resourceKind = 0x05
)

// String returns a human-readable name for the resource kind.
func (k resourceKind) String() string {
	switch k {
	case resPath:
		return "path"
	case resBrush:
		return "brush"
	case resImage:
		return "image"
	case resFont:
		return "font"
	case resFontData:
		return "font-data"
	default:
		return fmt.Sprintf("unknown(0x%02x)", uint8(k))
	}
}

// brushTag identifies a brush representation inside a brush resource payload.
type brushTag uint8

const (
	brushTagNil     brushTag = 0x00
	brushTagSolid   brushTag = 0x01
	brushTagLinear  brushTag = 0x02
	brushTagRadial  brushTag = 0x03
	brushTagSweep   brushTag = 0x04
	brushTagPattern brushTag = 0x05
)

// Sentinel errors returned (wrapped in *StreamError) by the stream reader and
// writer. Use errors.Is to test for them.
var (
	// ErrTruncated reports that a frame ended in the middle of a value, or
	// that the stream ended in the middle of a frame.
	ErrTruncated = errors.New("recording: truncated stream")

	// ErrChecksum reports a frame payload or whole-stream checksum mismatch.
	ErrChecksum = errors.New("recording: checksum mismatch")

	// ErrUnknownTag reports an unrecognized frame kind, resource kind,
	// brush tag, or command tag.
	ErrUnknownTag = errors.New("recording: unknown tag")

	// ErrLengthTooLarge reports a frame length (or resource count) that
	// exceeds the configured limits.
	ErrLengthTooLarge = errors.New("recording: length exceeds limit")

	// ErrInvalidHeader reports a bad magic, an unsupported version, an
	// unknown endianness tag, or a first frame that is not a canvas frame.
	ErrInvalidHeader = errors.New("recording: invalid stream header")

	// ErrInvalidData reports a value that is structurally invalid: an
	// out-of-range enum, trailing bytes, a non-canonical varint, or a
	// negative dimension.
	ErrInvalidData = errors.New("recording: invalid data")

	// ErrDuplicateResource reports a resource ID defined more than once.
	ErrDuplicateResource = errors.New("recording: duplicate resource definition")

	// ErrUnresolvedResource reports a command referencing a resource that
	// has not been defined at the point of use (out-of-order resource).
	ErrUnresolvedResource = errors.New("recording: unresolved resource reference")

	// ErrUnfinishedStream reports the underlying reader reaching EOF at a
	// frame boundary before an end frame was seen.
	ErrUnfinishedStream = errors.New("recording: stream ended without an end marker")

	// ErrCanceled reports that the caller canceled the operation.
	ErrCanceled = errors.New("recording: canceled")

	// ErrStreamFinished reports a write attempted after Finish, or Finish
	// called twice.
	ErrStreamFinished = errors.New("recording: stream already finished")

	// ErrAlreadyStreaming reports StreamTo called on a recorder that is
	// already attached to a stream.
	ErrAlreadyStreaming = errors.New("recording: recorder already streaming")

	// ErrUnsupportedFace reports a text.Face that cannot be serialized
	// because it has no FontSource or exposes no raw font data.
	ErrUnsupportedFace = errors.New("recording: unsupported font face")

	// ErrUnsupportedCommand reports a Command implementation outside the
	// package's sealed command set.
	ErrUnsupportedCommand = errors.New("recording: unsupported command")

	// ErrPartiallyConsumed reports ReadAll called after commands were
	// already delivered by Next when command retention is disabled.
	ErrPartiallyConsumed = errors.New("recording: stream partially consumed")
)

// StreamError wraps an error with the stream position at which it occurred.
type StreamError struct {
	// Op is "read" or "write".
	Op string
	// Chunk is the 1-based frame (chunk) number where the error occurred.
	// On write it is the frame that failed to be written.
	Chunk uint64
	// Command is the 1-based command number when the error is attributable
	// to a command. Zero for canvas, end, and unrelated resource errors.
	Command uint64
	// Offset is the number of bytes read or written before the failing frame.
	Offset int64
	// Kind names the frame or resource kind involved, if known.
	Kind string
	// Err is the underlying error; use errors.Is against the sentinels.
	Err error
}

// Error implements error.
func (e *StreamError) Error() string {
	var b strings.Builder
	b.WriteString("recording: ")
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(" ")
	}
	b.WriteString("stream")
	if e.Chunk > 0 {
		fmt.Fprintf(&b, " chunk %d", e.Chunk)
	}
	if e.Command > 0 {
		fmt.Fprintf(&b, " command %d", e.Command)
	}
	if e.Kind != "" {
		fmt.Fprintf(&b, " (%s)", e.Kind)
	}
	if e.Offset > 0 {
		fmt.Fprintf(&b, " at byte %d", e.Offset)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the underlying error.
func (e *StreamError) Unwrap() error { return e.Err }

// StreamConfig configures a StreamWriter or StreamReader. The zero value is
// valid and selects the defaults. A nil *StreamConfig is also valid.
type StreamConfig struct {
	// MaxFrameSize bounds a single frame payload. Values <= 0 select
	// DefaultMaxFrameSize.
	MaxFrameSize uint32

	// MaxResources bounds the number of resources of each kind and the
	// largest resource ID a reader accepts. Values <= 0 select
	// DefaultMaxResources.
	MaxResources uint32

	// BigEndian makes the writer emit big-endian headers and float values.
	// Readers accept both byte orders regardless of this setting.
	BigEndian bool

	// Cancel, when non-nil, is polled before each frame and before each
	// payload read. A closed channel cancels the operation with ErrCanceled.
	Cancel <-chan struct{}

	// RetainCommands makes a StreamReader keep every command it delivers via
	// Next, so ReadAll can return a complete Recording even after partial
	// consumption. Off by default to keep long streams out of memory.
	RetainCommands bool
}

// normalized returns a config with defaults applied.
func (c *StreamConfig) normalized() StreamConfig {
	var out StreamConfig
	if c != nil {
		out = *c
	}
	if out.MaxFrameSize == 0 {
		out.MaxFrameSize = DefaultMaxFrameSize
	}
	if out.MaxResources == 0 {
		out.MaxResources = DefaultMaxResources
	}
	return out
}

// crcTable is the Castagnoli polynomial table used for frame checksums.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// streamCRC computes the CRC-32C of b.
func streamCRC(b []byte) uint32 { return crc32.Checksum(b, crcTable) }

// zigzagEncode maps a signed integer to an unsigned one.
func zigzagEncode(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

// zigzagDecode maps an unsigned integer back to a signed one.
func zigzagDecode(v uint64) int64 { return int64(v>>1) ^ -int64(v&1) }

// byteOrder returns the binary.ByteOrder for a stream endianness tag.
func byteOrder(tag byte) (binary.ByteOrder, bool) {
	switch tag {
	case endianLittle:
		return binary.LittleEndian, true
	case endianBig:
		return binary.BigEndian, true
	default:
		return nil, false
	}
}

// endianTag returns the stream tag for a binary.ByteOrder.
func endianTag(bo binary.ByteOrder) byte {
	if bo == binary.BigEndian {
		return endianBig
	}
	return endianLittle
}

// --------------------------------------------------------------------------
// Encoder
// --------------------------------------------------------------------------

// encoder appends wire-format values to a byte slice. It never returns errors;
// the writer controls the frame limits and validates commands before encoding.
type encoder struct {
	buf []byte
	bo  binary.ByteOrder
	err error

	// idOf translates a resource-pool index to its stream ID. Pool indices
	// restart at zero after Recorder.Reset while stream IDs keep growing, so
	// the maps are append-only and never cleared mid-stream.
	idOf [3]map[uint32]uint32

	// faceIDs maps a face to the font resource ID assigned when it was
	// emitted. It is owned by the writer and survives reset.
	faceIDs map[text.Face]uint32
}

// resource slot indices in encoder.idOf. Fonts are keyed by face identity in
// encoder.faceIDs instead, since text commands carry the face directly.
const (
	idPath = iota
	idBrush
	idImage
)

// streamID returns the stream ID for a pool index, latching an error if the
// index was never registered.
func (e *encoder) streamID(slot int, poolIndex uint32) uint32 {
	if m := e.idOf[slot]; m != nil {
		if id, ok := m[poolIndex]; ok {
			return id
		}
	}
	e.fail(fmt.Errorf("%w: pool index %d (slot %d)", ErrUnresolvedResource, poolIndex, slot))
	return InvalidRef
}

// invalidRefID is the uvarint sentinel for a nil/absent resource reference.
// It matches InvalidRef but is encoded as a varint for compactness and
// byte-order independence, consistent with every other resource ID.
const invalidRefID = uint64(InvalidRef)

// pathID appends the stream ID for a path pool index, or the invalid sentinel.
func (e *encoder) pathID(ref PathRef) {
	if !ref.IsValid() {
		e.uvarint(invalidRefID)
		return
	}
	e.uvarint(uint64(e.streamID(idPath, uint32(ref))))
}

// brushID appends the stream ID for a brush pool index.
func (e *encoder) brushID(ref BrushRef) {
	if !ref.IsValid() {
		e.uvarint(invalidRefID)
		return
	}
	e.uvarint(uint64(e.streamID(idBrush, uint32(ref))))
}

// imageID appends the stream ID for an image pool index.
func (e *encoder) imageID(ref ImageRef) {
	if !ref.IsValid() {
		e.uvarint(invalidRefID)
		return
	}
	e.uvarint(uint64(e.streamID(idImage, uint32(ref))))
}

// reset clears the buffer while keeping its capacity.
func (e *encoder) reset() {
	e.buf = e.buf[:0]
	e.err = nil
}

// u8 appends a byte.
func (e *encoder) u8(v uint8) { e.buf = append(e.buf, v) }

// boolean appends a bool as one byte.
func (e *encoder) boolean(v bool) {
	if v {
		e.u8(1)
	} else {
		e.u8(0)
	}
}

// uvarint appends an unsigned varint.
func (e *encoder) uvarint(v uint64) { e.buf = binary.AppendUvarint(e.buf, v) }

// u32 appends a uint32 in stream byte order.
func (e *encoder) u32(v uint32) {
	var tmp [4]byte
	e.bo.PutUint32(tmp[:], v)
	e.buf = append(e.buf, tmp[:]...)
}

// u64 appends a uint64 in stream byte order.
func (e *encoder) u64(v uint64) {
	var tmp [8]byte
	e.bo.PutUint64(tmp[:], v)
	e.buf = append(e.buf, tmp[:]...)
}

// f32 appends a float32 in stream byte order.
func (e *encoder) f32(v float32) { e.u32(math.Float32bits(v)) }

// f64 appends a float64 in stream byte order.
func (e *encoder) f64(v float64) { e.u64(math.Float64bits(v)) }

// i64 appends a signed integer as a zigzag varint.
func (e *encoder) i64(v int64) { e.uvarint(zigzagEncode(v)) }

// face appends a font resource: the ID of its font-data resource, the face
// size, and the shaping options needed to recreate an equivalent face on the
// reading side. The caller supplies the font-data ID; it is emitted after the
// font-data frame so the reader can build the face immediately.
func (e *encoder) face(face text.Face, fontDataID uint32) {
	if face == nil {
		e.u8(0) // nil face
		return
	}
	e.u8(1)
	e.uvarint(uint64(fontDataID))
	e.f64(face.Size())
	e.i64(int64(face.Direction()))
	e.str(face.Language())

	features := face.Features()
	e.uvarint(uint64(len(features)))
	for _, f := range features {
		e.buf = append(e.buf, f.Tag[:]...)
		e.u32(f.Value)
	}
	variations := face.Variations()
	e.uvarint(uint64(len(variations)))
	for _, v := range variations {
		e.buf = append(e.buf, v.Tag[:]...)
		e.f32(v.Value)
	}
}

// fontData extracts the raw font file bytes backing a face.
func fontData(face text.Face) ([]byte, error) {
	if face == nil {
		return nil, nil
	}
	src := face.Source()
	if src == nil {
		return nil, fmt.Errorf("%w: %T has no FontSource", ErrUnsupportedFace, face)
	}
	provider, ok := src.Parsed().(text.RawFontDataProvider)
	if !ok {
		return nil, fmt.Errorf("%w: %T exposes no raw font data", ErrUnsupportedFace, src.Parsed())
	}
	raw := provider.RawFontData()
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty font data for %q", ErrUnsupportedFace, src.Name())
	}
	return raw, nil
}

// str appends a length-prefixed string.
func (e *encoder) str(s string) {
	e.uvarint(uint64(len(s)))
	e.buf = append(e.buf, s...)
}

// fail records a deferred encoding error. The writer converts it into a
// StreamError with the current frame position.
func (e *encoder) fail(err error) {
	if e.err == nil {
		e.err = err
	}
}

// path appends a path resource body: a verb stream and its coordinates.
func (e *encoder) path(p *gg.Path) {
	if p == nil {
		e.uvarint(0) // zero verbs marks a nil path
		return
	}
	verbs := p.Verbs()
	coords := p.Coords()
	e.uvarint(uint64(len(verbs)))
	for _, v := range verbs {
		e.u8(uint8(v))
	}
	e.uvarint(uint64(len(coords)))
	for _, c := range coords {
		e.f64(c)
	}
}

// brush appends a brush definition.
func (e *encoder) brush(b Brush) {
	switch v := b.(type) {
	case nil:
		e.u8(uint8(brushTagNil))
	case SolidBrush:
		e.u8(uint8(brushTagSolid))
		e.rgba(v.Color)
	case *SolidBrush:
		if v == nil {
			e.u8(uint8(brushTagNil))
			return
		}
		e.u8(uint8(brushTagSolid))
		e.rgba(v.Color)
	case LinearGradientBrush:
		e.u8(uint8(brushTagLinear))
		e.ggPoint(v.Start)
		e.ggPoint(v.End)
		e.gradientStops(v.Stops)
		e.i64(int64(v.Extend))
	case *LinearGradientBrush:
		if v == nil {
			e.u8(uint8(brushTagNil))
			return
		}
		e.brush(*v)
	case RadialGradientBrush:
		e.u8(uint8(brushTagRadial))
		e.ggPoint(v.Center)
		e.ggPoint(v.Focus)
		e.f64(v.StartRadius)
		e.f64(v.EndRadius)
		e.gradientStops(v.Stops)
		e.i64(int64(v.Extend))
	case *RadialGradientBrush:
		if v == nil {
			e.u8(uint8(brushTagNil))
			return
		}
		e.brush(*v)
	case SweepGradientBrush:
		e.u8(uint8(brushTagSweep))
		e.ggPoint(v.Center)
		e.f64(v.StartAngle)
		e.f64(v.EndAngle)
		e.gradientStops(v.Stops)
		e.i64(int64(v.Extend))
	case *SweepGradientBrush:
		if v == nil {
			e.u8(uint8(brushTagNil))
			return
		}
		e.brush(*v)
	case PatternBrush:
		e.u8(uint8(brushTagPattern))
		e.uvarint(uint64(v.Image))
		e.i64(int64(v.Repeat))
		e.ggMatrix(v.Transform)
	case *PatternBrush:
		if v == nil {
			e.u8(uint8(brushTagNil))
			return
		}
		e.brush(*v)
	default:
		// The Brush interface is sealed, so this only happens for a nil
		// pointer wrapped in a non-nil interface. Reject rather than guess.
		e.fail(fmt.Errorf("%w: brush %T", ErrInvalidData, b))
	}
}

// gradientStops appends a color-stop list.
func (e *encoder) gradientStops(stops []GradientStop) {
	e.uvarint(uint64(len(stops)))
	for _, s := range stops {
		e.f64(s.Offset)
		e.rgba(s.Color)
	}
}

// ggPoint appends a gg.Point.
func (e *encoder) ggPoint(p gg.Point) {
	e.f64(p.X)
	e.f64(p.Y)
}

// ggMatrix appends a gg.Matrix.
func (e *encoder) ggMatrix(m gg.Matrix) {
	e.f64(m.A)
	e.f64(m.B)
	e.f64(m.C)
	e.f64(m.D)
	e.f64(m.E)
	e.f64(m.F)
}

// bytes appends length-prefixed bytes.
func (e *encoder) bytes(b []byte) {
	e.uvarint(uint64(len(b)))
	e.buf = append(e.buf, b...)
}

// image appends an image resource: dimensions, a pixel format tag, and raw
// row data. Images are normalized to non-premultiplied RGBA8. A nil image is
// encoded with zero dimensions.
func (e *encoder) image(img image.Image) {
	if img == nil {
		e.uvarint(0)
		e.uvarint(0)
		e.u8(0)
		return
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		e.fail(fmt.Errorf("%w: image bounds %v", ErrInvalidData, b))
		return
	}
	// Guard the pixel-buffer allocation against integer overflow; the frame
	// size limit is enforced separately by the writer, but a corrupt or
	// absurd image should not allocate gigabytes before that check.
	if w > math.MaxInt/4/h || w*h*4 > 1<<30 {
		e.fail(fmt.Errorf("%w: image %dx%d too large", ErrLengthTooLarge, w, h))
		return
	}
	e.uvarint(uint64(w))
	e.uvarint(uint64(h))
	e.u8(1) // format: RGBA8, non-premultiplied
	// Row-major RGBA bytes, one row per write into the buffer.
	start := len(e.buf)
	e.buf = append(e.buf, make([]byte, w*h*4)...)
	row := make([]byte, w*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// color.NRGBAModel un-premultiplies and handles every image
			// implementation (RGBA, NRGBA, Gray, Paletted, YCbCr).
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			i := x * 4
			row[i+0] = c.R
			row[i+1] = c.G
			row[i+2] = c.B
			row[i+3] = c.A
		}
		copy(e.buf[start+y*w*4:], row)
	}
}

// rgba appends a color as four float64 values.
func (e *encoder) rgba(c gg.RGBA) {
	e.f64(c.R)
	e.f64(c.G)
	e.f64(c.B)
	e.f64(c.A)
}

// rect appends a rectangle as four float64 values.
func (e *encoder) rect(r Rect) {
	e.f64(r.MinX)
	e.f64(r.MinY)
	e.f64(r.MaxX)
	e.f64(r.MaxY)
}

// matrix appends a matrix as six float64 values.
func (e *encoder) matrix(m Matrix) {
	e.f64(m.A)
	e.f64(m.B)
	e.f64(m.C)
	e.f64(m.D)
	e.f64(m.E)
	e.f64(m.F)
}

// stroke appends a stroke style.
func (e *encoder) stroke(s Stroke) {
	e.f64(s.Width)
	e.u8(uint8(s.Cap))
	e.u8(uint8(s.Join))
	e.f64(s.MiterLimit)
	e.uvarint(uint64(len(s.DashPattern)))
	for _, d := range s.DashPattern {
		e.f64(d)
	}
	e.f64(s.DashOffset)
}

// --------------------------------------------------------------------------
// Decoder
// --------------------------------------------------------------------------

// decoder consumes wire-format values from a frame payload. All reads are
// bounds-checked; the first failure is latched in err and every later read
// returns a zero value. Callers check err once at the end.
type decoder struct {
	b   []byte
	i   int
	bo  binary.ByteOrder
	res *resourceStore
	err error
}

// resourceStore holds resources as they arrive, keyed by their explicit IDs.
// Definitions may arrive in any order as long as a command only references
// IDs that have already been defined. Duplicate IDs are rejected.
type resourceStore struct {
	// by ID, as they arrived.
	paths   map[uint32]*gg.Path
	brushes map[uint32]Brush
	images  map[uint32]image.Image
	fonts   map[uint32]text.Face

	// refs maps a stream resource ID to its index in the assembled
	// ResourcePool. Stream IDs and pool indices coincide for well-formed
	// streams but need not if a producer reorders definitions.
	pathRefs  map[uint32]PathRef
	brushRefs map[uint32]BrushRef
	imageRefs map[uint32]ImageRef
	fontRefs  map[uint32]FontRef

	// fontData holds raw font bytes until a font resource references them.
	fontData map[uint32][]byte
	fontSrc  map[uint32]*text.FontSource
}

func newResourceStore() *resourceStore {
	return &resourceStore{
		paths:     make(map[uint32]*gg.Path),
		brushes:   make(map[uint32]Brush),
		images:    make(map[uint32]image.Image),
		fonts:     make(map[uint32]text.Face),
		pathRefs:  make(map[uint32]PathRef),
		brushRefs: make(map[uint32]BrushRef),
		imageRefs: make(map[uint32]ImageRef),
		fontRefs:  make(map[uint32]FontRef),
		fontData:  make(map[uint32][]byte),
		fontSrc:   make(map[uint32]*text.FontSource),
	}
}

// kindIndex maps a resource kind to a stable slot, or -1 if unknown.
func kindIndex(k resourceKind) int {
	switch k {
	case resPath:
		return 0
	case resBrush:
		return 1
	case resImage:
		return 2
	case resFont:
		return 3
	case resFontData:
		return 4
	default:
		return -1
	}
}

// fail latches the first error.
func (d *decoder) fail(err error) {
	if d.err == nil {
		d.err = err
	}
}

// remaining returns the number of unread bytes.
func (d *decoder) remaining() int { return len(d.b) - d.i }

// need reports whether n more bytes are available, latching ErrTruncated.
func (d *decoder) need(n int) bool {
	if d.err != nil {
		return false
	}
	if n < 0 || n > len(d.b)-d.i {
		d.fail(ErrTruncated)
		return false
	}
	return true
}

// u8 reads a byte.
func (d *decoder) u8() uint8 {
	if !d.need(1) {
		return 0
	}
	v := d.b[d.i]
	d.i++
	return v
}

// boolean reads a bool and validates its encoding.
func (d *decoder) boolean() bool {
	v := d.u8()
	if v > 1 {
		d.fail(ErrInvalidData)
		return false
	}
	return v == 1
}

// u32 reads a uint32 in stream byte order.
func (d *decoder) u32() uint32 {
	if !d.need(4) {
		return 0
	}
	v := d.bo.Uint32(d.b[d.i:])
	d.i += 4
	return v
}

// u64 reads a uint64 in stream byte order.
func (d *decoder) u64() uint64 {
	if !d.need(8) {
		return 0
	}
	v := d.bo.Uint64(d.b[d.i:])
	d.i += 8
	return v
}

// f32 reads a float32 in stream byte order.
func (d *decoder) f32() float32 { return math.Float32frombits(d.u32()) }

// f64 reads a float64 in stream byte order.
func (d *decoder) f64() float64 { return math.Float64frombits(d.u64()) }

// uvarint reads an unsigned varint.
func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b[d.i:])
	if n <= 0 {
		d.fail(ErrInvalidData)
		return 0
	}
	d.i += n
	return v
}

// i64 reads a zigzag-encoded signed integer.
func (d *decoder) i64() int64 { return zigzagDecode(d.uvarint()) }

// raw reads length-prefixed bytes. The length must fit in the remaining
// payload; otherwise ErrInvalidData is latched.
func (d *decoder) raw() []byte {
	n := d.uvarint()
	if d.err != nil {
		return nil
	}
	if n > uint64(d.remaining()) {
		d.fail(ErrInvalidData)
		return nil
	}
	v := d.b[d.i : d.i+int(n)]
	d.i += int(n)
	return v
}

// str reads a length-prefixed string.
func (d *decoder) str() string { return string(d.raw()) }

// count reads an element count and validates that the remaining payload could
// hold that many elements of at least minBytes each. This bounds allocations.
func (d *decoder) count(minBytes int) int {
	c := d.uvarint()
	if d.err != nil {
		return 0
	}
	if minBytes < 1 {
		minBytes = 1
	}
	if c > math.MaxInt32 || c > uint64(d.remaining()/minBytes) {
		d.fail(ErrInvalidData)
		return 0
	}
	return int(c)
}

// rgba reads four float64 values as a color.
func (d *decoder) rgba() gg.RGBA {
	return gg.RGBA{R: d.f64(), G: d.f64(), B: d.f64(), A: d.f64()}
}

// rect reads four float64 values as a rectangle.
func (d *decoder) rect() Rect {
	return Rect{MinX: d.f64(), MinY: d.f64(), MaxX: d.f64(), MaxY: d.f64()}
}

// matrix reads six float64 values as a matrix.
func (d *decoder) matrix() Matrix {
	return Matrix{A: d.f64(), B: d.f64(), C: d.f64(), D: d.f64(), E: d.f64(), F: d.f64()}
}

// stroke reads a stroke style.
func (d *decoder) stroke() Stroke {
	s := Stroke{
		Width:      d.f64(),
		Cap:        LineCap(d.u8()),
		Join:       LineJoin(d.u8()),
		MiterLimit: d.f64(),
	}
	if s.Cap > LineCapSquare {
		d.fail(ErrInvalidData)
	}
	if s.Join > LineJoinBevel {
		d.fail(ErrInvalidData)
	}
	n := d.count(8)
	if n > 0 && d.err == nil {
		s.DashPattern = make([]float64, n)
		for i := range s.DashPattern {
			s.DashPattern[i] = d.f64()
		}
	}
	s.DashOffset = d.f64()
	return s
}

// done verifies that the whole payload was consumed.
func (d *decoder) done() error {
	if d.err != nil {
		return d.err
	}
	if d.i != len(d.b) {
		return ErrInvalidData
	}
	return nil
}

// --------------------------------------------------------------------------
// Resource payloads
// --------------------------------------------------------------------------

// pathBody decodes a path body. A zero verb count yields a nil path.
func (d *decoder) pathBody() *gg.Path {
	verbCount := d.count(1)
	if d.err != nil {
		return nil
	}
	if verbCount == 0 {
		return nil
	}
	verbs := make([]gg.PathVerb, verbCount)
	var coordsNeeded int
	for i := range verbs {
		v := gg.PathVerb(d.u8())
		if d.err != nil {
			return nil
		}
		n := pathVerbCoords(v)
		if n < 0 {
			d.fail(fmt.Errorf("%w: path verb %d", ErrInvalidData, v))
			return nil
		}
		verbs[i] = v
		coordsNeeded += n
	}
	coordCount := d.count(8)
	if d.err != nil {
		return nil
	}
	if coordCount != coordsNeeded {
		d.fail(fmt.Errorf("%w: path has %d coords, verbs need %d", ErrInvalidData, coordCount, coordsNeeded))
		return nil
	}
	b := gg.BuildPath()
	for _, v := range verbs {
		switch v {
		case gg.MoveTo:
			b.MoveTo(d.f64(), d.f64())
		case gg.LineTo:
			b.LineTo(d.f64(), d.f64())
		case gg.QuadTo:
			b.QuadTo(d.f64(), d.f64(), d.f64(), d.f64())
		case gg.CubicTo:
			b.CubicTo(d.f64(), d.f64(), d.f64(), d.f64(), d.f64(), d.f64())
		case gg.Close:
			b.Close()
		}
	}
	if d.err != nil {
		return nil
	}
	return b.Build()
}

// pathVerbCoords returns the coordinate count for a verb, or -1 if invalid.
func pathVerbCoords(v gg.PathVerb) int {
	switch v {
	case gg.MoveTo, gg.LineTo:
		return 2
	case gg.QuadTo:
		return 4
	case gg.CubicTo:
		return 6
	case gg.Close:
		return 0
	default:
		return -1
	}
}

// brushBody decodes a brush definition.
func (d *decoder) brushBody() Brush {
	tag := brushTag(d.u8())
	if d.err != nil {
		return nil
	}
	switch tag {
	case brushTagNil:
		return nil
	case brushTagSolid:
		return SolidBrush{Color: d.rgba()}
	case brushTagLinear:
		start := gg.Point{X: d.f64(), Y: d.f64()}
		end := gg.Point{X: d.f64(), Y: d.f64()}
		stops := d.gradientStops()
		extend := ExtendMode(d.i64())
		return &LinearGradientBrush{Start: start, End: end, Stops: stops, Extend: extend}
	case brushTagRadial:
		center := gg.Point{X: d.f64(), Y: d.f64()}
		focus := gg.Point{X: d.f64(), Y: d.f64()}
		startRadius := d.f64()
		endRadius := d.f64()
		stops := d.gradientStops()
		extend := ExtendMode(d.i64())
		return &RadialGradientBrush{
			Center: center, Focus: focus,
			StartRadius: startRadius, EndRadius: endRadius,
			Stops: stops, Extend: extend,
		}
	case brushTagSweep:
		center := gg.Point{X: d.f64(), Y: d.f64()}
		startAngle := d.f64()
		endAngle := d.f64()
		stops := d.gradientStops()
		extend := ExtendMode(d.i64())
		return &SweepGradientBrush{
			Center: center, StartAngle: startAngle, EndAngle: endAngle,
			Stops: stops, Extend: extend,
		}
	case brushTagPattern:
		imgID := uint32(d.uvarint())
		repeat := RepeatMode(d.i64())
		transform := gg.Matrix{A: d.f64(), B: d.f64(), C: d.f64(), D: d.f64(), E: d.f64(), F: d.f64()}
		if d.err != nil {
			return nil
		}
		// The pattern's image must already be defined. Translate the stream
		// ID to a pool index so the brush is immediately playable.
		imageRef, ok := d.res.imageRefs[imgID]
		if !ok {
			d.fail(fmt.Errorf("%w: pattern image %d", ErrUnresolvedResource, imgID))
			return nil
		}
		return &PatternBrush{Image: imageRef, Repeat: repeat, Transform: transform}
	default:
		d.fail(fmt.Errorf("%w: brush tag 0x%02x", ErrUnknownTag, uint8(tag)))
		return nil
	}
}

// gradientStops decodes a color-stop list.
func (d *decoder) gradientStops() []GradientStop {
	n := d.count(32) // offset f64 + four color f64
	if d.err != nil || n == 0 {
		return nil
	}
	stops := make([]GradientStop, n)
	for i := range stops {
		stops[i] = GradientStop{Offset: d.f64(), Color: d.rgba()}
	}
	return stops
}

// imageBody decodes an image resource into an *image.RGBA.
func (d *decoder) imageBody() image.Image {
	w := d.uvarint()
	h := d.uvarint()
	format := d.u8()
	if d.err != nil {
		return nil
	}
	if w == 0 && h == 0 {
		return nil // nil image
	}
	if format != 1 {
		d.fail(fmt.Errorf("%w: image format %d", ErrUnknownTag, format))
		return nil
	}
	if w > math.MaxInt32 || h > math.MaxInt32 {
		d.fail(ErrLengthTooLarge)
		return nil
	}
	iW, iH := int(w), int(h)
	// The payload must already hold w*h*4 bytes; count against the remaining
	// buffer before allocating so a bad length cannot exhaust memory.
	if iW > 0 && iH > math.MaxInt/iW/4 {
		d.fail(ErrLengthTooLarge)
		return nil
	}
	pixelBytes := iW * iH * 4
	if pixelBytes > d.remaining() {
		d.fail(ErrTruncated)
		return nil
	}
	img := image.NewNRGBA(image.Rect(0, 0, iW, iH))
	copy(img.Pix, d.b[d.i:d.i+pixelBytes])
	d.i += pixelBytes
	return img
}

// faceBody decodes a font resource, building a Face from its font data.
// It returns the face and any font-data ID it used.
func (d *decoder) faceBody() (text.Face, uint32, error) {
	present := d.u8()
	if d.err != nil {
		return nil, 0, d.err
	}
	if present == 0 {
		return nil, 0, nil
	}
	if present != 1 {
		return nil, 0, fmt.Errorf("%w: font presence %d", ErrInvalidData, present)
	}
	fontDataID := uint32(d.uvarint())
	size := d.f64()
	direction := text.Direction(d.i64())
	language := d.str()
	nFeatures := d.count(8)
	var features []text.FontFeature
	if nFeatures > 0 && d.err == nil {
		features = make([]text.FontFeature, nFeatures)
		for i := range features {
			if !d.need(8) {
				break
			}
			copy(features[i].Tag[:], d.b[d.i:d.i+4])
			d.i += 4
			features[i].Value = d.u32()
		}
	}
	nVariations := d.count(8)
	var variations []text.FontVariation
	if nVariations > 0 && d.err == nil {
		variations = make([]text.FontVariation, nVariations)
		for i := range variations {
			if !d.need(8) {
				break
			}
			copy(variations[i].Tag[:], d.b[d.i:d.i+4])
			d.i += 4
			variations[i].Value = d.f32()
		}
	}
	if d.err != nil {
		return nil, 0, d.err
	}
	if direction < text.DirectionLTR || direction > text.DirectionBTT {
		return nil, 0, fmt.Errorf("%w: direction %d", ErrInvalidData, direction)
	}

	src, ok := d.res.fontSrc[fontDataID]
	if !ok {
		return nil, 0, fmt.Errorf("%w: font data %d", ErrUnresolvedResource, fontDataID)
	}
	opts := []text.FaceOption{
		text.WithDirection(direction),
		text.WithLanguage(language),
	}
	if len(features) > 0 {
		opts = append(opts, text.WithFeatures(features...))
	}
	if len(variations) > 0 {
		opts = append(opts, text.WithVariations(variations...))
	}
	return src.Face(size, opts...), fontDataID, nil
}
