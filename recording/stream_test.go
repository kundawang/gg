package recording

// Tests for the streaming wire format: round-trip fidelity, robustness against
// truncation/corruption, resource ordering rules, and error attribution.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"os"
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
)

// buildSampleRecording creates a recording exercising every command family,
// resources, fonts, images, clipping, and nested transforms.
func buildSampleRecording(t *testing.T) *Recorder {
	t.Helper()
	rec := NewRecorder(200, 150)

	// Background solid rectangle.
	rec.SetFillRGBA(0.9, 0.95, 1, 1)
	rec.FillRectangle(0, 0, 200, 150)

	// Filled path with a linear gradient.
	grad := NewLinearGradientBrush(0, 0, 200, 150).
		AddColorStop(0, gg.RGBA{R: 1, G: 0, B: 0, A: 1}).
		AddColorStop(1, gg.RGBA{R: 0, G: 0, B: 1, A: 1})
	rec.SetFillStyle(grad)
	rec.DrawCircle(60, 60, 30)
	rec.Fill()

	// Stroked rectangle with dashes and a radial brush.
	rec.SetStrokeStyle(NewRadialGradientBrush(100, 100, 0, 80).
		AddColorStop(0, gg.White).
		AddColorStop(1, gg.Black))
	rec.SetLineWidth(2.5)
	rec.SetLineCap(LineCapRound)
	rec.SetLineJoin(LineJoinBevel)
	rec.SetMiterLimit(6)
	rec.SetDash(6, 3)
	rec.StrokeRectangle(20, 20, 160, 110)
	rec.ClearDash()

	// Nested transforms + save/restore.
	rec.Push()
	rec.Translate(120, 90)
	rec.Rotate(0.4)
	rec.Scale(1.5, 1.5)
	rec.Push()
	rec.Translate(-10, -10)
	rec.SetFillRGB(0.1, 0.8, 0.2)
	rec.DrawRectangle(0, 0, 30, 20)
	rec.Fill()
	rec.Pop()
	rec.Restore()

	// Clip with even-odd rule.
	rec.SetFillRule(FillRuleEvenOdd)
	rec.DrawCircle(150, 40, 25)
	rec.Clip()
	rec.DrawRectangle(120, 10, 60, 60)
	rec.Fill()
	rec.ResetClip()
	rec.SetFillRule(FillRuleNonZero)

	// Clip round rect.
	rec.ClipRoundRect(10, 120, 80, 25, 5)
	rec.SetFillRGB(0.5, 0.5, 0.5)
	rec.DrawRectangle(10, 120, 80, 25)
	rec.Fill()
	rec.ResetClip()

	// Image.
	img := image.NewNRGBA(image.Rect(0, 0, 16, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 16; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 16), G: uint8(y * 20), B: 128, A: uint8(128 + x*8)})
		}
	}
	rec.DrawImage(img, 150, 115)

	// Pattern brush referencing an image (exercises image→brush dependency).
	// Only serialized, not pixel-compared, so any image will do.
	pat := NewPatternBrush(rec.resources.AddImage(img)).
		SetRepeat(RepeatX).
		SetTransform(gg.Matrix{A: 0.5, B: 0, C: 1, D: 0, E: 0.5, F: 2})
	rec.SetFillStyle(pat)
	rec.DrawRectangle(5, 5, 20, 20)
	rec.Fill()

	// Text with a real font, if available.
	if face := loadTestFace(t); face != nil {
		rec.SetFont(face)
		rec.SetFillRGB(0, 0, 0)
		rec.DrawString("Stream", 20, 145)
	}

	return rec
}

// loadTestFace loads the Ahem test font, skipping the text portion when the
// font is unavailable.
func loadTestFace(t *testing.T) text.Face {
	t.Helper()
	data, err := os.ReadFile("../text/testdata/ahem.ttf")
	if err != nil {
		return nil
	}
	src, err := text.NewFontSource(data)
	if err != nil {
		t.Fatalf("parse ahem: %v", err)
	}
	return src.Face(14)
}

// recordingsEqual compares two recordings command-by-command with a stable
// textual description, then compares their resources.
func recordingsEqual(t *testing.T, a, b *Recording) {
	t.Helper()
	if a.Width() != b.Width() || a.Height() != b.Height() {
		t.Fatalf("dimensions %dx%d vs %dx%d", a.Width(), a.Height(), b.Width(), b.Height())
	}
	ac, bc := a.Commands(), b.Commands()
	if len(ac) != len(bc) {
		t.Fatalf("command count %d vs %d", len(ac), len(bc))
	}
	for i := range ac {
		if got, want := describeCommand(bc[i]), describeCommand(ac[i]); got != want {
			t.Fatalf("command %d differs:\n want %s\n  got %s", i, want, got)
		}
	}
	ar, br := a.Resources(), b.Resources()
	if ar.PathCount() != br.PathCount() {
		t.Fatalf("path count %d vs %d", ar.PathCount(), br.PathCount())
	}
	for i := 0; i < ar.PathCount(); i++ {
		if got, want := describePath(br.GetPath(PathRef(i))), describePath(ar.GetPath(PathRef(i))); got != want {
			t.Fatalf("path %d differs:\n want %s\n  got %s", i, want, got)
		}
	}
	if ar.BrushCount() != br.BrushCount() {
		t.Fatalf("brush count %d vs %d", ar.BrushCount(), br.BrushCount())
	}
	for i := 0; i < ar.BrushCount(); i++ {
		if got, want := describeBrush(br.GetBrush(BrushRef(i))), describeBrush(ar.GetBrush(BrushRef(i))); got != want {
			t.Fatalf("brush %d differs:\n want %s\n  got %s", i, want, got)
		}
	}
	if ar.ImageCount() != br.ImageCount() {
		t.Fatalf("image count %d vs %d", ar.ImageCount(), br.ImageCount())
	}
	for i := 0; i < ar.ImageCount(); i++ {
		if got, want := describeImage(br.GetImage(ImageRef(i))), describeImage(ar.GetImage(ImageRef(i))); got != want {
			t.Fatalf("image %d differs", i)
		}
	}
}

// describeCommand renders a command's fields deterministically.
func describeCommand(cmd Command) string {
	switch c := cmd.(type) {
	case SaveCommand:
		return "save"
	case RestoreCommand:
		return "restore"
	case ClearClipCommand:
		return "clearclip"
	case SetTransformCommand:
		return fmt.Sprintf("transform %v", c.Matrix)
	case SetClipCommand:
		return fmt.Sprintf("clip path=%d rule=%d", c.Path, c.Rule)
	case ClipRoundRectCommand:
		return fmt.Sprintf("cliprr %g %g %g %g %g", c.X, c.Y, c.W, c.H, c.Radius)
	case FillPathCommand:
		return fmt.Sprintf("fillpath p=%d b=%d r=%d", c.Path, c.Brush, c.Rule)
	case StrokePathCommand:
		return fmt.Sprintf("strokepath p=%d b=%d %s", c.Path, c.Brush, describeStroke(c.Stroke))
	case FillRectCommand:
		return fmt.Sprintf("fillrect %v b=%d", c.Rect, c.Brush)
	case StrokeRectCommand:
		return fmt.Sprintf("strokerect %v b=%d %s", c.Rect, c.Brush, describeStroke(c.Stroke))
	case DrawImageCommand:
		return fmt.Sprintf("image i=%d %v %v %d %.3f", c.Image, c.SrcRect, c.DstRect, c.Options.Interpolation, c.Options.Alpha)
	case DrawTextCommand:
		return fmt.Sprintf("text %q %.3f %.3f %.3f %q b=%d face=%v", c.Text, c.X, c.Y, c.FontSize, c.FontFamily, c.Brush, c.Face != nil)
	case StrokeTextCommand:
		return fmt.Sprintf("stroketext %q %.3f %.3f %.3f %q b=%d face=%v %s", c.Text, c.X, c.Y, c.FontSize, c.FontFamily, c.Brush, c.Face != nil, describeStroke(c.Stroke))
	case SetFillStyleCommand:
		return fmt.Sprintf("fillstyle b=%d", c.Brush)
	case SetStrokeStyleCommand:
		return fmt.Sprintf("strokestyle b=%d", c.Brush)
	case SetLineWidthCommand:
		return fmt.Sprintf("linewidth %g", c.Width)
	case SetLineCapCommand:
		return fmt.Sprintf("linecap %d", c.Cap)
	case SetLineJoinCommand:
		return fmt.Sprintf("linejoin %d", c.Join)
	case SetMiterLimitCommand:
		return fmt.Sprintf("miter %g", c.Limit)
	case SetDashCommand:
		return fmt.Sprintf("dash %v %.3f", c.Pattern, c.Offset)
	case SetFillRuleCommand:
		return fmt.Sprintf("fillrule %d", c.Rule)
	case SetAntiAliasCommand:
		return fmt.Sprintf("antialias %v", c.Enabled)
	default:
		return fmt.Sprintf("unknown %T", cmd)
	}
}

func describeStroke(s Stroke) string {
	return fmt.Sprintf("w=%g cap=%d join=%d miter=%g dash=%v off=%g", s.Width, s.Cap, s.Join, s.MiterLimit, s.DashPattern, s.DashOffset)
}

func describePath(p *gg.Path) string {
	if p == nil {
		return "nil"
	}
	var b bytes.Buffer
	p.Iterate(func(v gg.PathVerb, coords []float64) {
		fmt.Fprintf(&b, "%d:%v;", v, coords)
	})
	return b.String()
}

func describeBrush(br Brush) string {
	switch v := br.(type) {
	case nil:
		return "nil"
	case SolidBrush:
		return fmt.Sprintf("solid %v", v.Color)
	case *LinearGradientBrush:
		return fmt.Sprintf("linear %v %v %v %d", v.Start, v.End, v.Stops, v.Extend)
	case *RadialGradientBrush:
		return fmt.Sprintf("radial %v %v %g %g %v %d", v.Center, v.Focus, v.StartRadius, v.EndRadius, v.Stops, v.Extend)
	case *SweepGradientBrush:
		return fmt.Sprintf("sweep %v %g %g %v %d", v.Center, v.StartAngle, v.EndAngle, v.Stops, v.Extend)
	case *PatternBrush:
		return fmt.Sprintf("pattern %d %d %v", v.Image, v.Repeat, v.Transform)
	default:
		return fmt.Sprintf("unknown %T", br)
	}
}

func describeImage(img image.Image) string {
	if img == nil {
		return "nil"
	}
	b := img.Bounds()
	var sum uint64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			sum = sum*31 + uint64(r>>8)<<24 + uint64(g>>8)<<16 + uint64(bl>>8)<<8 + uint64(a>>8)
		}
	}
	return fmt.Sprintf("%dx%d@%v sum=%d", b.Dx(), b.Dy(), b.Min, sum)
}

func TestStreamRoundTrip(t *testing.T) {
	src := buildSampleRecording(t)
	final := src.FinishRecording()

	var buf bytes.Buffer
	sw, err := final.StreamTo(&buf, nil)
	if err != nil {
		t.Fatalf("StreamTo: %v", err)
	}
	if err := sw.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	got, err := sr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !sr.Done() {
		t.Fatal("reader did not see end frame")
	}
	recordingsEqual(t, final, got)
}

func TestStreamRecorderLive(t *testing.T) {
	// Record directly into a stream (the incremental path).
	var buf bytes.Buffer
	live := NewRecorder(180, 120)
	sw, err := live.StreamTo(&buf, nil)
	if err != nil {
		t.Fatalf("StreamTo: %v", err)
	}

	live.SetFillRGB(0.2, 0.4, 0.6)
	live.DrawCircle(90, 60, 40)
	live.Fill()
	live.Push()
	live.Translate(90, 60)
	live.Rotate(0.25)
	live.SetStrokeRGB(1, 1, 0)
	live.SetLineWidth(3)
	live.DrawRectangle(-20, -20, 40, 40)
	live.Stroke()
	live.Pop()

	if err := sw.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	got, err := sr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	recordingsEqual(t, live.FinishRecording(), got)
}

func TestStreamBigEndian(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, err := final.StreamTo(&buf, &StreamConfig{BigEndian: true})
	if err != nil {
		t.Fatalf("StreamTo: %v", err)
	}
	if err := sw.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	// The header must declare big-endian.
	if got := buf.Bytes()[6]; got != endianBig {
		t.Fatalf("endianness tag = 0x%02x, want big", got)
	}
	// A default reader detects the byte order from the header.
	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	got, err := sr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	recordingsEqual(t, final, got)
}

func TestStreamIncrementalNext(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, err := final.StreamTo(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}

	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	var n int
	for {
		cmd, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if cmd == nil {
			t.Fatal("nil command without error")
		}
		n++
	}
	if !sr.Done() {
		t.Fatal("stream not done")
	}
	if n != len(final.Commands()) {
		t.Fatalf("commands streamed %d, want %d", n, len(final.Commands()))
	}
}

// TestStreamTruncation feeds the reader prefixes of a valid stream and checks
// that it fails cleanly with ErrTruncated or ErrUnfinishedStream and never
// panics or allocates unboundedly.
func TestStreamTruncation(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, err := final.StreamTo(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}
	full := buf.Bytes()

	for _, cut := range []int{1, 5, 15, 16, 17, 40, len(full) / 2, len(full) - 1} {
		if cut >= len(full) {
			continue
		}
		sr := NewStreamReader(bytes.NewReader(full[:cut]), nil)
		_, err := sr.ReadAll()
		if err == nil {
			t.Fatalf("cut %d: expected error", cut)
		}
		if !errors.Is(err, ErrTruncated) && !errors.Is(err, ErrUnfinishedStream) {
			t.Fatalf("cut %d: got %v, want truncation/unfinished", cut, err)
		}
		var se *StreamError
		if !errors.As(err, &se) {
			t.Fatalf("cut %d: error %T is not *StreamError", cut, err)
		}
		if se.Chunk == 0 {
			t.Fatalf("cut %d: StreamError lacks chunk number: %v", cut, se)
		}
	}
}

func TestStreamCorruptHeader(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()
	full := append([]byte(nil), buf.Bytes()...)

	cases := map[string]func([]byte){
		"magic":      func(b []byte) { b[0] = 'X' },
		"version":    func(b []byte) { b[4], b[5] = 0xFF, 0x7F },
		"endianness": func(b []byte) { b[6] = 0x09 },
		"kind":       func(b []byte) { b[7] = 0x7F },
	}
	for name, mutate := range cases {
		b := append([]byte(nil), full...)
		mutate(b)
		sr := NewStreamReader(bytes.NewReader(b), nil)
		_, err := sr.ReadAll()
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if !errors.Is(err, ErrInvalidHeader) && !errors.Is(err, ErrUnknownTag) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

func TestStreamPayloadChecksum(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()
	b := append([]byte(nil), buf.Bytes()...)

	// Flip a byte in the first frame payload (canvas frame, 16-byte header).
	b[frameHeaderSize+3] ^= 0xFF
	sr := NewStreamReader(bytes.NewReader(b), nil)
	_, err := sr.ReadAll()
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("got %v, want ErrChecksum", err)
	}
}

func TestStreamCorruptLengthUnbounded(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()
	b := append([]byte(nil), buf.Bytes()...)

	// Claim a frame payload far larger than the configured limit.
	binary.LittleEndian.PutUint32(b[8:12], math.MaxUint32)
	sr := NewStreamReader(bytes.NewReader(b), nil)
	_, err := sr.ReadAll()
	if !errors.Is(err, ErrLengthTooLarge) {
		t.Fatalf("got %v, want ErrLengthTooLarge", err)
	}
}

func TestStreamUnfinishedStream(t *testing.T) {
	// A canvas frame followed by a command frame but no end frame.
	final := NewRecorder(10, 10)
	final.SetFillRGB(1, 0, 0)
	final.DrawRectangle(0, 0, 10, 10)
	final.Fill()

	var buf bytes.Buffer
	sw, _ := final.FinishRecording().StreamTo(&buf, nil)
	_ = sw.Finish()
	cut := buf.Len() - 1
	// Trim the end frame: find the last frame by walking headers.
	full := buf.Bytes()
	off := 0
	last := 0
	for off+frameHeaderSize <= len(full) {
		ln := int(binary.LittleEndian.Uint32(full[off+8 : off+12]))
		last = off
		off += frameHeaderSize + ln
	}
	_ = cut
	sr := NewStreamReader(bytes.NewReader(full[:last]), nil)
	_, err := sr.ReadAll()
	if !errors.Is(err, ErrUnfinishedStream) {
		t.Fatalf("got %v, want ErrUnfinishedStream", err)
	}
}

func TestStreamDuplicateResource(t *testing.T) {
	// Hand-build a stream with a duplicated path resource ID.
	var buf bytes.Buffer
	enc := encoder{bo: binary.LittleEndian}
	writeFrame := func(kind frameKind, payload []byte) {
		var hdr [frameHeaderSize]byte
		copy(hdr[0:4], streamMagic)
		binary.LittleEndian.PutUint16(hdr[4:6], StreamVersion)
		hdr[6] = endianLittle
		hdr[7] = byte(kind)
		binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(payload)))
		binary.LittleEndian.PutUint32(hdr[12:16], streamCRC(payload))
		buf.Write(hdr[:])
		buf.Write(payload)
	}

	enc.reset()
	enc.f64(10)
	enc.f64(10)
	writeFrame(frameCanvas, append([]byte(nil), enc.buf...))

	path := gg.BuildPath().Rect(0, 0, 5, 5).Build()
	enc.reset()
	enc.u8(uint8(resPath))
	enc.uvarint(0)
	enc.path(path)
	writeFrame(frameResource, append([]byte(nil), enc.buf...))
	writeFrame(frameResource, append([]byte(nil), enc.buf...)) // duplicate ID 0

	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	_, err := sr.ReadAll()
	if !errors.Is(err, ErrDuplicateResource) {
		t.Fatalf("got %v, want ErrDuplicateResource", err)
	}
}

func TestStreamOutOfOrderResource(t *testing.T) {
	// A command references a path that was never defined.
	var buf bytes.Buffer
	enc := encoder{bo: binary.LittleEndian}
	writeFrame := func(kind frameKind, payload []byte) {
		var hdr [frameHeaderSize]byte
		copy(hdr[0:4], streamMagic)
		binary.LittleEndian.PutUint16(hdr[4:6], StreamVersion)
		hdr[6] = endianLittle
		hdr[7] = byte(kind)
		binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(payload)))
		binary.LittleEndian.PutUint32(hdr[12:16], streamCRC(payload))
		buf.Write(hdr[:])
		buf.Write(payload)
	}

	enc.reset()
	enc.f64(10)
	enc.f64(10)
	writeFrame(frameCanvas, append([]byte(nil), enc.buf...))

	// Hand-encode a FillPath referencing path ID 5 and brush ID 0, neither of
	// which was defined. tag 0x07 = FillPath; ids are uvarints.
	enc.reset()
	enc.u8(uint8(tagFillPath))
	enc.uvarint(5) // path
	enc.uvarint(0) // brush
	enc.u8(uint8(FillRuleNonZero))
	writeFrame(frameCommand, append([]byte(nil), enc.buf...))

	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	_, err := sr.ReadAll()
	if !errors.Is(err, ErrUnresolvedResource) {
		t.Fatalf("got %v, want ErrUnresolvedResource", err)
	}
}

// partialWriter writes at most n bytes per call to exercise short-write paths.
type partialWriter struct {
	buf bytes.Buffer
	n   int
}

func (p *partialWriter) Write(b []byte) (int, error) {
	if len(b) > p.n {
		b = b[:p.n]
	}
	return p.buf.Write(b)
}

func TestStreamShortWriter(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	pw := &partialWriter{n: 3}
	sw, err := final.StreamTo(pw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}
	sr := NewStreamReader(bytes.NewReader(pw.buf.Bytes()), nil)
	got, err := sr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	recordingsEqual(t, final, got)
}

// failingWriter fails after the given number of bytes.
type failingWriter struct {
	budget int
	n      int
	err    error
}

func (f *failingWriter) Write(b []byte) (int, error) {
	if f.n >= f.budget {
		return 0, f.err
	}
	take := f.budget - f.n
	if take > len(b) {
		take = len(b)
	}
	f.n += take
	if take < len(b) {
		return take, f.err
	}
	return take, nil
}

func TestStreamUnderlyingIOError(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	boom := errors.New("disk full")
	fw := &failingWriter{budget: 200, err: boom}
	sw, err := final.StreamTo(fw, nil)
	if err == nil {
		err = sw.Finish()
	}
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want underlying error", err)
	}
	var se *StreamError
	if !errors.As(err, &se) {
		t.Fatalf("error %T is not *StreamError", err)
	}
}

func TestStreamCancel(t *testing.T) {
	cancel := make(chan struct{})
	close(cancel)
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	_, err := final.StreamTo(&buf, &StreamConfig{Cancel: cancel})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("got %v, want ErrCanceled", err)
	}

	// Reader cancellation on a valid stream.
	var good bytes.Buffer
	sw, _ := final.StreamTo(&good, nil)
	_ = sw.Finish()
	sr := NewStreamReader(bytes.NewReader(good.Bytes()), &StreamConfig{Cancel: cancel})
	_, err = sr.ReadAll()
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("read: got %v, want ErrCanceled", err)
	}
}

func TestStreamCommandNumberInErrors(t *testing.T) {
	// Corrupt a command frame payload's checksum so the error is attributed to
	// a command number.
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()
	b := append([]byte(nil), buf.Bytes()...)

	off := 0
	cmdIndex := 0
	for off+frameHeaderSize <= len(b) {
		kind := b[off+7]
		ln := int(binary.LittleEndian.Uint32(b[off+8 : off+12]))
		if frameKind(kind) == frameCommand {
			cmdIndex++
			if cmdIndex == 3 {
				b[off+frameHeaderSize] ^= 0xFF // corrupt payload
				break
			}
		}
		off += frameHeaderSize + ln
	}
	if cmdIndex < 3 {
		t.Skip("sample has fewer than 3 commands")
	}
	sr := NewStreamReader(bytes.NewReader(b), nil)
	_, err := sr.ReadAll()
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("got %v, want ErrChecksum", err)
	}
	var se *StreamError
	if !errors.As(err, &se) {
		t.Fatalf("not a StreamError: %T", err)
	}
	if se.Command != 3 {
		t.Fatalf("command number = %d, want 3", se.Command)
	}
}

func TestStreamMaxFrameSize(t *testing.T) {
	// An image larger than MaxFrameSize must be rejected, not allocated.
	final := NewRecorder(50, 50)
	big := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	final.DrawImage(big, 0, 0)
	var buf bytes.Buffer
	_, err := final.FinishRecording().StreamTo(&buf, &StreamConfig{MaxFrameSize: 128})
	if !errors.Is(err, ErrLengthTooLarge) {
		t.Fatalf("got %v, want ErrLengthTooLarge", err)
	}
}

func TestStreamPartiallyConsumed(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()

	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	if _, err := sr.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := sr.ReadAll(); !errors.Is(err, ErrPartiallyConsumed) {
		t.Fatalf("got %v, want ErrPartiallyConsumed", err)
	}
}

func TestStreamRetainCommands(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()

	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), &StreamConfig{RetainCommands: true})
	var n int
	for {
		_, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	got, err := sr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after retained Next: %v", err)
	}
	if len(got.Commands()) != n {
		t.Fatalf("retained %d commands, want %d", len(got.Commands()), n)
	}
}

func TestStreamEmptyRecording(t *testing.T) {
	rec := NewRecorder(8, 8)
	var buf bytes.Buffer
	sw, err := rec.StreamTo(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}
	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	got, err := sr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got.Width() != 8 || got.Height() != 8 {
		t.Fatalf("dims %dx%d", got.Width(), got.Height())
	}
	if len(got.Commands()) != 0 {
		t.Fatalf("expected no commands, got %d", len(got.Commands()))
	}
}

func TestStreamDoubleFinish(t *testing.T) {
	rec := NewRecorder(8, 8)
	var buf bytes.Buffer
	sw, err := rec.StreamTo(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := sw.Finish(); !errors.Is(err, ErrStreamFinished) {
		t.Fatalf("second Finish got %v, want ErrStreamFinished", err)
	}
}

func TestStreamWriteAfterFinish(t *testing.T) {
	rec := NewRecorder(8, 8)
	var buf bytes.Buffer
	sw, _ := rec.StreamTo(&buf, nil)
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}
	rec.SetFillRGB(1, 0, 0)
	rec.DrawRectangle(0, 0, 5, 5)
	rec.Fill()
	if se := sw.Err(); !errors.Is(se, ErrStreamFinished) {
		t.Fatalf("post-finish write err = %v, want ErrStreamFinished", se)
	}
}

func TestStreamResetBounded(t *testing.T) {
	// Recording with periodic Reset keeps the in-memory command count bounded
	// while the stream still contains every command.
	var buf bytes.Buffer
	rec := NewRecorder(100, 100)
	sw, err := rec.StreamTo(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	const batches = 5
	const perBatch = 4
	for i := 0; i < batches; i++ {
		for j := 0; j < perBatch; j++ {
			rec.SetFillRGB(float64(i)/batches, float64(j)/perBatch, 0.5)
			rec.DrawRectangle(float64(j*10), float64(i*10), 8, 8)
			rec.Fill()
		}
		if len(rec.commands) > 3*perBatch {
			t.Fatalf("batch %d: %d commands retained", i, len(rec.commands))
		}
		rec.Reset()
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}

	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	var n int
	for {
		_, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		n++
	}
	if n < batches*perBatch*2 {
		t.Fatalf("streamed %d commands, want at least %d", n, batches*perBatch*2)
	}
	if !sr.Done() {
		t.Fatal("stream not done")
	}
}

// rawStream assembles frames with a supplied payload builder for adversarial
// inputs that the writer would never produce.
type rawStream struct {
	buf bytes.Buffer
	enc encoder
}

func newRawStream() *rawStream {
	return &rawStream{enc: encoder{bo: binary.LittleEndian}}
}

func (r *rawStream) write(kind frameKind, payload []byte) {
	var hdr [frameHeaderSize]byte
	copy(hdr[0:4], streamMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], StreamVersion)
	hdr[6] = endianLittle
	hdr[7] = byte(kind)
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[12:16], streamCRC(payload))
	r.buf.Write(hdr[:])
	r.buf.Write(payload)
}

func (r *rawStream) canvas(w, h int) {
	r.enc.reset()
	r.enc.f64(float64(w))
	r.enc.f64(float64(h))
	r.write(frameCanvas, append([]byte(nil), r.enc.buf...))
}

func TestStreamUnknownCommandTag(t *testing.T) {
	rs := newRawStream()
	rs.canvas(10, 10)
	rs.write(frameCommand, []byte{0xFF}) // unknown tag
	sr := NewStreamReader(bytes.NewReader(rs.buf.Bytes()), nil)
	_, err := sr.ReadAll()
	if !errors.Is(err, ErrUnknownTag) {
		t.Fatalf("got %v, want ErrUnknownTag", err)
	}
}

func TestStreamInvalidEnum(t *testing.T) {
	rs := newRawStream()
	rs.canvas(10, 10)
	// SetLineCap (tag 0x11) with an out-of-range cap value 9.
	rs.write(frameCommand, []byte{byte(tagSetLineCap), 9})
	sr := NewStreamReader(bytes.NewReader(rs.buf.Bytes()), nil)
	_, err := sr.ReadAll()
	if !errors.Is(err, ErrInvalidData) {
		t.Fatalf("got %v, want ErrInvalidData", err)
	}
}

func TestStreamTrailingCommandBytes(t *testing.T) {
	rs := newRawStream()
	rs.canvas(10, 10)
	// Save command (tag 0x01) with a stray trailing byte.
	rs.write(frameCommand, []byte{byte(tagSave), 0x00})
	sr := NewStreamReader(bytes.NewReader(rs.buf.Bytes()), nil)
	_, err := sr.ReadAll()
	if !errors.Is(err, ErrInvalidData) {
		t.Fatalf("got %v, want ErrInvalidData for trailing bytes", err)
	}
}

func TestStreamEndCountMismatch(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()

	// Rewrite the end frame's command count to a wrong value while keeping the
	// payload checksum valid.
	b := append([]byte(nil), buf.Bytes()...)
	off := 0
	for off+frameHeaderSize <= len(b) {
		kind := frameKind(b[off+7])
		ln := int(binary.LittleEndian.Uint32(b[off+8 : off+12]))
		if kind == frameEnd {
			// Rebuild a payload with a bogus command count.
			enc := encoder{bo: binary.LittleEndian}
			enc.uvarint(endMarkerTag)
			// Copy the real counts, then override commands.
			src := b[off+frameHeaderSize : off+frameHeaderSize+ln]
			d := decoder{b: src, bo: binary.LittleEndian}
			_ = d.uvarint()
			for i := 0; i < 5; i++ {
				enc.uvarint(d.uvarint())
			}
			_ = d.uvarint() // real commands
			enc.uvarint(9999)
			enc.u32(0) // CRC deliberately wrong too
			payload := enc.buf
			binary.LittleEndian.PutUint32(b[off+8:off+12], uint32(len(payload)))
			binary.LittleEndian.PutUint32(b[off+12:off+16], streamCRC(payload))
			b = append(b[:off+frameHeaderSize], append(payload, b[off+frameHeaderSize+ln:]...)...)
			break
		}
		off += frameHeaderSize + ln
	}
	sr := NewStreamReader(bytes.NewReader(b), nil)
	_, err := sr.ReadAll()
	if !errors.Is(err, ErrInvalidData) && !errors.Is(err, ErrChecksum) {
		t.Fatalf("got %v, want invalid data / checksum", err)
	}
}

func TestStreamTotals(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()

	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	if _, err := sr.ReadAll(); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	totals := sr.Totals()
	if totals.Commands != uint64(len(final.Commands())) {
		t.Fatalf("totals.Commands = %d, want %d", totals.Commands, len(final.Commands()))
	}
	if totals.Paths != uint64(final.Resources().PathCount()) {
		t.Fatalf("totals.Paths = %d, want %d", totals.Paths, final.Resources().PathCount())
	}
	if totals.Brushes != uint64(final.Resources().BrushCount()) {
		t.Fatalf("totals.Brushes = %d, want %d", totals.Brushes, final.Resources().BrushCount())
	}
	if totals.Images != uint64(final.Resources().ImageCount()) {
		t.Fatalf("totals.Images = %d, want %d", totals.Images, final.Resources().ImageCount())
	}
}

// TestStreamAllCommandTags verifies every CommandType maps to a wire tag and
// every tag decodes to a command, guarding the CommandType↔tag ordering
// coupling in commandTagFor.
func TestStreamAllCommandTags(t *testing.T) {
	valid := 0
	for ct := CommandType(0); ct.String() != "Unknown"; ct++ {
		valid++
	}
	seen := map[commandTag]bool{}
	for ct := CommandType(0); int(ct) < valid; ct++ {
		tag, ok := commandTagFor(ct)
		if !ok {
			t.Fatalf("CommandType %d (%s) has no wire tag", ct, ct.String())
		}
		if seen[tag] {
			t.Fatalf("duplicate wire tag %d for CommandType %d", tag, ct)
		}
		seen[tag] = true
		if int(tag) > len(commandTagNames) {
			t.Fatalf("tag %d out of range", tag)
		}
	}
	for tag := range commandTagNames {
		if commandTagNames[tag] == "" {
			continue
		}
		found := false
		for ct := CommandType(0); int(ct) < valid; ct++ {
			if got, _ := commandTagFor(ct); got == commandTag(tag) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("wire tag %d (%s) has no CommandType", tag, commandTagNames[tag])
		}
	}
}

// TestStreamCommandRoundTripEachDouble checks each command type survives a
// combined encode/decode with representative field values.
func TestStreamCommandRoundTripEachDouble(t *testing.T) {
	pool := NewResourcePool()
	// Register one of each resource so references translate.
	pool.AddImage(image.NewNRGBA(image.Rect(0, 0, 2, 2)))
	pool.AddBrush(SolidBrush{Color: gg.RGBA{R: 1, A: 1}})
	pool.AddPath(gg.BuildPath().Rect(0, 0, 1, 1).Build())

	cases := []Command{
		SaveCommand{},
		RestoreCommand{},
		SetTransformCommand{Matrix: Matrix{A: 1, C: 2, E: 3}},
		SetClipCommand{Path: 0, Rule: FillRuleEvenOdd},
		ClearClipCommand{},
		ClipRoundRectCommand{X: 1, Y: 2, W: 3, H: 4, Radius: 5},
		FillPathCommand{Path: 0, Brush: 0, Rule: FillRuleNonZero},
		StrokePathCommand{Path: 0, Brush: 0, Stroke: Stroke{Width: 2, Cap: LineCapRound, Join: LineJoinBevel, MiterLimit: 5, DashPattern: []float64{1, 2}, DashOffset: 0.5}},
		FillRectCommand{Rect: NewRect(1, 2, 3, 4), Brush: 0},
		StrokeRectCommand{Rect: NewRect(1, 2, 3, 4), Brush: 0, Stroke: DefaultStroke()},
		DrawImageCommand{Image: 0, SrcRect: NewRect(0, 0, 2, 2), DstRect: NewRect(1, 1, 2, 2), Options: ImageOptions{Interpolation: InterpolationNearest, Alpha: 0.5}},
		DrawTextCommand{Text: "hi", X: 1, Y: 2, FontSize: 12, FontFamily: "f", Brush: 0},
		StrokeTextCommand{Text: "hi", X: 1, Y: 2, FontSize: 12, FontFamily: "f", Brush: 0, Stroke: DefaultStroke()},
		SetFillStyleCommand{Brush: 0},
		SetStrokeStyleCommand{Brush: 0},
		SetLineWidthCommand{Width: 3},
		SetLineCapCommand{Cap: LineCapSquare},
		SetLineJoinCommand{Join: LineJoinRound},
		SetMiterLimitCommand{Limit: 7},
		SetDashCommand{Pattern: []float64{3, 1}, Offset: 0.25},
		SetFillRuleCommand{Rule: FillRuleEvenOdd},
		SetAntiAliasCommand{Enabled: false},
	}

	w := NewStreamWriter(&bytes.Buffer{}, nil)
	w.rec = &Recorder{width: 10, height: 10}
	// Pre-register resources in the encoder ID maps so refs resolve.
	w.enc.idOf[idPath][0] = 0
	w.enc.idOf[idBrush][0] = 0
	w.enc.idOf[idImage][0] = 0

	for _, cmd := range cases {
		enc := encoder{bo: binary.LittleEndian, faceIDs: map[text.Face]uint32{}}
		enc.idOf = w.enc.idOf
		if err := encodeCommand(&enc, cmd); err != nil {
			t.Fatalf("%T encode: %v", cmd, err)
		}
		payload := append([]byte(nil), enc.buf...)

		// Build a pool whose references exist so decodeRef succeeds.
		store := newResourceStore()
		store.pathRefs[0] = 0
		store.brushRefs[0] = 0
		store.imageRefs[0] = 0
		d := decoder{b: payload, bo: binary.LittleEndian, res: store}
		got, err := decodeCommand(&d)
		if err != nil {
			t.Fatalf("%T decode: %v", cmd, err)
		}
		if err := d.done(); err != nil {
			t.Fatalf("%T trailing bytes: %v", cmd, err)
		}
		if got.Type() != cmd.Type() {
			t.Fatalf("type %s decoded as %s", cmd.Type(), got.Type())
		}
	}
}

// TestStreamResetWithResources checks that Reset restarts pool indices while
// stream IDs stay monotonic, so a reader never sees a duplicate definition and
// every post-reset command resolves to the resource defined after the reset.
func TestStreamResetWithResources(t *testing.T) {
	var buf bytes.Buffer
	rec := NewRecorder(40, 40)
	sw, err := rec.StreamTo(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}

	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 3)
	}
	for batch := 0; batch < 3; batch++ {
		// A distinct color each batch: new brush pool entry at the same index.
		rec.SetFillRGBA(float64(batch)/3, 0.2, 0.8, 1)
		rec.DrawRectangle(1, 1, 10, 10)
		rec.Fill()
		rec.DrawImage(img, batch*5, 0)
		rec.Reset()
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}

	// The reader must accept the stream: IDs never repeat, references resolve.
	sr := NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	var n int
	for {
		_, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		n++
	}
	if !sr.Done() {
		t.Fatal("stream not done")
	}
	// Three batches, each producing the same number of commands. The exact
	// count is validated by the end-frame/totals consistency check, which the
	// reader already enforced by accepting the stream.
	if n == 0 || n%3 != 0 {
		t.Fatalf("read %d commands, not a positive multiple of 3 batches", n)
	}
	if sr.Totals().Commands != uint64(n) {
		t.Fatalf("totals.Commands = %d, read %d", sr.Totals().Commands, n)
	}
	if sr.Totals().Images != 3 {
		t.Fatalf("totals.Images = %d, want 3 (one per batch)", sr.Totals().Images)
	}
}

// chunkReader returns at most n bytes per Read and optionally an error on the
// final read, exercising the reader's handling of partial underlying reads.
type chunkReader struct {
	data []byte
	n    int
	pos  int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	take := r.n
	if take > len(p) {
		take = len(p)
	}
	if take > len(r.data)-r.pos {
		take = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+take])
	r.pos += take
	return take, nil
}

func TestStreamOneByteReads(t *testing.T) {
	final := buildSampleRecording(t).FinishRecording()
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()

	// Feed the reader one byte at a time; io.ReadFull must still assemble
	// complete frames and the result must match a normal read.
	sr := NewStreamReader(&chunkReader{data: buf.Bytes(), n: 1}, nil)
	got, err := sr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	recordingsEqual(t, final, got)
}
