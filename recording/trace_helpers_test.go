package recording

import (
	"errors"
	"fmt"
	"image"
	"io"
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
)

// traceSpyBackend is a Backend that logs every interface call in order. It
// is the ground truth that trace events are checked against: the trace's
// backend call sequence must match the spy's log exactly.
//
// failMethod, when non-empty, makes the matching lifecycle method return
// failErr (only Begin/End can surface errors through the Backend interface).
type traceSpyBackend struct {
	calls []spyCall

	beginWidth, beginHeight int
	transform               Matrix
	transformStack          []Matrix
	saveCount               int
	restoreCount            int
	clipCount               int
	clearClipCount          int
	fillPathCount           int
	strokePathCount         int
	fillRectCount           int
	drawImageCount          int
	drawTextCount           int

	failBegin bool
	failEnd   bool
	failErr   error
}

// spyCall is one observed Backend interface invocation.
type spyCall struct {
	Method string
	// Note holds method-specific scalar detail (transform entries, rule,
	// rect corners) so tests can assert the trace matches actual arguments.
	Note string
}

func (b *traceSpyBackend) Begin(width, height int) error {
	b.calls = append(b.calls, spyCall{Method: "Begin"})
	b.beginWidth, b.beginHeight = width, height
	b.transform = Identity()
	if b.failBegin {
		return b.failErr
	}
	return nil
}

func (b *traceSpyBackend) End() error {
	b.calls = append(b.calls, spyCall{Method: "End"})
	if b.failEnd {
		return b.failErr
	}
	return nil
}

func (b *traceSpyBackend) Save() {
	b.saveCount++
	b.calls = append(b.calls, spyCall{Method: "Save"})
	b.transformStack = append(b.transformStack, b.transform)
}

func (b *traceSpyBackend) Restore() {
	b.restoreCount++
	b.calls = append(b.calls, spyCall{Method: "Restore"})
	if len(b.transformStack) > 0 {
		n := len(b.transformStack) - 1
		b.transform = b.transformStack[n]
		b.transformStack = b.transformStack[:n]
	}
}

func (b *traceSpyBackend) SetTransform(m Matrix) {
	b.transform = m
	b.calls = append(b.calls, spyCall{Method: "SetTransform", Note: fmt.Sprintf("%v", m)})
}

func (b *traceSpyBackend) SetClip(path *gg.Path, rule FillRule) {
	b.clipCount++
	b.calls = append(b.calls, spyCall{Method: "SetClip", Note: fillRuleName(rule)})
}

func (b *traceSpyBackend) ClearClip() {
	b.clearClipCount++
	b.calls = append(b.calls, spyCall{Method: "ClearClip"})
}

func (b *traceSpyBackend) FillPath(path *gg.Path, brush Brush, rule FillRule) {
	b.fillPathCount++
	b.calls = append(b.calls, spyCall{Method: "FillPath", Note: fillRuleName(rule)})
}

func (b *traceSpyBackend) StrokePath(path *gg.Path, brush Brush, stroke Stroke) {
	b.strokePathCount++
	b.calls = append(b.calls, spyCall{Method: "StrokePath"})
}

func (b *traceSpyBackend) FillRect(rect Rect, brush Brush) {
	b.fillRectCount++
	b.calls = append(b.calls, spyCall{Method: "FillRect"})
}

func (b *traceSpyBackend) DrawImage(img image.Image, src, dst Rect, opts ImageOptions) {
	b.drawImageCount++
	b.calls = append(b.calls, spyCall{Method: "DrawImage"})
}

func (b *traceSpyBackend) DrawText(s string, x, y float64, face text.Face, brush Brush) {
	b.drawTextCount++
	b.calls = append(b.calls, spyCall{Method: "DrawText"})
}

// ensure traceSpyBackend satisfies Backend at compile time.
var _ Backend = (*traceSpyBackend)(nil)

// sampleRecording builds a recording that exercises every state, style,
// geometry, clip and text-free drawing path so a single trace covers the
// full command switch.
func sampleRecording() *Recording {
	rec := NewRecorder(200, 120)
	rec.SetRGB(1, 0, 0)
	rec.Save()
	rec.Translate(10, 20)
	rec.DrawRectangle(0, 0, 40, 30)
	rec.Fill()
	rec.SetLineWidth(2.5)
	rec.SetLineCap(LineCapRound)
	rec.SetLineJoin(LineJoinBevel)
	rec.SetMiterLimit(6)
	rec.SetDash(3, 2)
	rec.SetFillRule(FillRuleEvenOdd)
	rec.SetAntiAlias(false)
	rec.DrawCircle(60, 40, 15)
	rec.Stroke()
	rec.ClearDash()
	rec.MoveTo(0, 0)
	rec.LineTo(50, 50)
	rec.Clip()
	rec.FillRectangle(5, 5, 10, 10)
	rec.ResetClip()
	rec.ClipRoundRect(1, 2, 8, 9, 3)
	rec.Restore()
	rec.StrokeRectangle(2, 3, 12, 14)
	return rec.FinishRecording()
}

func TestTracePlaybackNilBackend(t *testing.T) {
	r := sampleRecording()
	var events []TraceEvent
	err := r.TracePlayback(nil, TraceOptions{OnEvent: func(e TraceEvent) {
		events = append(events, e)
	}})
	if !errors.Is(err, ErrNilBackend) {
		t.Fatalf("TracePlayback(nil) err = %v, want ErrNilBackend", err)
	}
	if len(events) != 0 {
		t.Fatalf("nil backend emitted %d events, want 0", len(events))
	}
	// Plain Playback gets the same explicit error instead of panicking.
	if err := r.Playback(nil); !errors.Is(err, ErrNilBackend) {
		t.Fatalf("Playback(nil) err = %v, want ErrNilBackend", err)
	}
}

func TestTracePlaybackSinkValidation(t *testing.T) {
	r := sampleRecording()
	backend := &traceSpyBackend{}

	if err := r.TracePlayback(backend, TraceOptions{}); !errors.Is(err, ErrTraceNoSink) {
		t.Fatalf("no sink: err = %v, want ErrTraceNoSink", err)
	}
	var sink TraceEvent
	err := r.TracePlayback(backend, TraceOptions{
		Writer:  io.Discard,
		OnEvent: func(TraceEvent) { sink = TraceEvent{} },
	})
	if !errors.Is(err, ErrTraceBothSinks) {
		t.Fatalf("both sinks: err = %v, want ErrTraceBothSinks", err)
	}
	_ = sink

	// Negative and over-cap limits are rejected before any backend call.
	if err := r.TracePlayback(backend, TraceOptions{Writer: io.Discard, MaxStringLength: -1}); err == nil {
		t.Fatal("negative MaxStringLength: want error, got nil")
	}
	if err := r.TracePlayback(backend, TraceOptions{
		Writer:          io.Discard,
		MaxStringLength: hardMaxStringLength + 1,
	}); err == nil {
		t.Fatal("over-cap MaxStringLength: want error, got nil")
	}
	if backend.beginWidth != 0 {
		t.Fatalf("validation failures must not call Begin; got width=%d", backend.beginWidth)
	}
}
