package recording

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
	"golang.org/x/image/font/gofont/goregular"
)

// TestTraceEventShapeAndOrder verifies the stable skeleton of a trace:
// begin event first, one event per command in recording order, end event
// last, with contiguous command indices.
func TestTraceEventShapeAndOrder(t *testing.T) {
	r := sampleRecording()
	backend := &traceSpyBackend{}

	events, err := r.TraceEvents(backend)
	if err != nil {
		t.Fatalf("TraceEvents: %v", err)
	}
	cmds := r.Commands()
	if want := len(cmds) + 2; len(events) != want {
		t.Fatalf("events = %d, want %d (%d commands + begin/end)", len(events), want, len(cmds))
	}
	if events[0].Phase != "begin" || events[0].Index != -1 {
		t.Fatalf("first event = %+v, want begin/index=-1", events[0])
	}
	last := events[len(events)-1]
	if last.Phase != "end" || last.Index != -1 {
		t.Fatalf("last event = %+v, want end/index=-1", last)
	}
	for i, cmd := range cmds {
		ev := events[i+1]
		if ev.Phase != "command" || ev.Index != i {
			t.Fatalf("event %d = phase %q index %d, want command/%d", i, ev.Phase, ev.Index, i)
		}
		if ev.Type != cmd.Type().String() {
			t.Fatalf("event %d type = %q, want %q", i, ev.Type, cmd.Type().String())
		}
	}
}

// TestTraceBackendCallSequence checks that the trace's per-event call counts
// match the Backend methods actually invoked, and total to the spy log.
func TestTraceBackendCallSequence(t *testing.T) {
	r := sampleRecording()
	backend := &traceSpyBackend{}

	events, err := r.TraceEvents(backend)
	if err != nil {
		t.Fatalf("TraceEvents: %v", err)
	}

	total := 0
	for _, ev := range events {
		if ev.Backend.Status != "ok" {
			t.Fatalf("event %d (%s) status = %q, want ok", ev.Index, ev.Type, ev.Backend.Status)
		}
		if ev.Backend.Error != nil {
			t.Fatalf("event %d unexpectedly carries error %+v", ev.Index, ev.Backend.Error)
		}
		total += ev.Backend.Calls
	}
	if total != len(backend.calls) {
		t.Fatalf("traced calls = %d, spy observed %d (%v)", total, len(backend.calls), backend.calls)
	}
	if len(backend.calls) < 2 || backend.calls[0].Method != "Begin" ||
		backend.calls[len(backend.calls)-1].Method != "End" {
		t.Fatalf("spy call boundaries wrong: %v", backend.calls)
	}

	// StrokeRect must be the documented four-call isolate.
	for _, ev := range events {
		if ev.Type == "StrokeRect" && ev.Backend.Calls != 4 {
			t.Fatalf("StrokeRect calls = %d, want 4", ev.Backend.Calls)
		}
		if ev.Type == "SetClip" && ev.Backend.Calls != 3 {
			t.Fatalf("SetClip calls = %d, want 3", ev.Backend.Calls)
		}
	}
}

// TestTraceTransformAndClipState verifies the reconstructed transform and
// clip state evolve exactly as Playback drives them, including Save/Restore.
func TestTraceTransformAndClipState(t *testing.T) {
	r := sampleRecording()
	backend := &traceSpyBackend{}
	events, err := r.TraceEvents(backend)
	if err != nil {
		t.Fatalf("TraceEvents: %v", err)
	}

	// Events describe post-dispatch state. Replay the same bookkeeping,
	// applying each command's state change before comparing the event, so
	// the trace is cross-checked against an independent implementation.
	wantTransform := Identity()
	stack := []Matrix{}
	clipDepth := 0
	clipStack := []int{}
	for _, ev := range events {
		// Apply the expected state transition first.
		switch ev.Type {
		case "Save":
			stack = append(stack, wantTransform)
			clipStack = append(clipStack, clipDepth)
		case "Restore":
			if len(stack) > 0 {
				wantTransform = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			}
			if len(clipStack) > 0 {
				clipDepth = clipStack[len(clipStack)-1]
				clipStack = clipStack[:len(clipStack)-1]
			}
		case "SetTransform":
			if p, ok := ev.Params["matrix"].(MatrixInfo); ok {
				wantTransform = Matrix(p)
			}
		case "SetClip", "ClipRoundRect":
			clipDepth++
		case "ClearClip":
			clipDepth = 0
		}

		if ev.Transform != matrixInfo(wantTransform) {
			t.Fatalf("event %d (%s) transform = %+v, want %+v",
				ev.Index, ev.Type, ev.Transform, wantTransform)
		}
		if ev.Clip.Depth != clipDepth {
			t.Fatalf("event %d (%s) clip depth = %d, want %d",
				ev.Index, ev.Type, ev.Clip.Depth, clipDepth)
		}
		switch ev.Type {
		case "SetClip", "ClipRoundRect":
			if ev.Clip.Last == nil {
				t.Fatalf("event %d (%s): clip depth %d but no Last path summary",
					ev.Index, ev.Type, clipDepth)
			}
			if ev.Clip.Last.Empty {
				t.Fatalf("event %d (%s): clip path summary is empty", ev.Index, ev.Type)
			}
		case "ClearClip":
			if ev.Clip.Last != nil {
				t.Fatalf("ClearClip event still carries Last = %+v", ev.Clip.Last)
			}
		}
	}

	// The recording's lone Restore must unwind both clips installed inside
	// the Save, leaving the end event with an empty clip state.
	if got := events[len(events)-1].Clip.Depth; got != 0 {
		t.Fatalf("end clip depth = %d, want 0 after Restore", got)
	}
}

// TestTraceJSONLStableEncoding writes the same recording twice and requires
// byte-identical JSONL, valid JSON per line, and stable number formatting.
func TestTraceJSONLStableEncoding(t *testing.T) {
	r := sampleRecording()

	run := func() []byte {
		var buf bytes.Buffer
		if err := r.TracePlayback(&traceSpyBackend{}, TraceOptions{Writer: &buf}); err != nil {
			t.Fatalf("TracePlayback: %v", err)
		}
		return buf.Bytes()
	}
	first, second := run(), run()
	if !bytes.Equal(first, second) {
		t.Fatalf("JSONL not stable across runs\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	lines := bytes.Split(bytes.TrimRight(first, "\n"), []byte("\n"))
	if len(lines) != len(r.Commands())+2 {
		t.Fatalf("JSONL lines = %d, want %d", len(lines), len(r.Commands())+2)
	}
	for i, line := range lines {
		if !json.Valid(line) {
			t.Fatalf("line %d is not valid JSON: %s", i, line)
		}
		// Field order is fixed: every object begins with the index field.
		if !bytes.HasPrefix(line, []byte(`{"index":`)) {
			t.Fatalf("line %d does not start with index field: %s", i, line)
		}
		// No NaN/Inf/undefined tokens can appear in valid JSON, but assert
		// explicitly so a future encoder change cannot regress silently.
		lower := strings.ToLower(string(line))
		if strings.Contains(lower, "nan") || strings.Contains(lower, "inf") {
			t.Fatalf("line %d contains non-finite token: %s", i, line)
		}
	}
}

// TestTraceResourceSummaries verifies that path, brush and image references
// are described by summaries containing no bulk data, and that refs match
// the resource pool.
func TestTraceResourceSummaries(t *testing.T) {
	rec := NewRecorder(80, 60)
	rec.SetFillRGBA(0.25, 0.5, 0.75, 1)
	rec.DrawCircle(40, 30, 20)
	rec.Fill()

	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	rec.DrawImage(img, 1, 2)
	r := rec.FinishRecording()

	events, err := r.TraceEvents(&traceSpyBackend{})
	if err != nil {
		t.Fatalf("TraceEvents: %v", err)
	}

	var sawFill, sawImage bool
	for _, ev := range events {
		res := ev.Resources
		if res == nil {
			continue
		}
		assertPathSummary(t, ev, res)
		assertBrushSummary(t, r, res)
		if ev.Type == "FillPath" {
			sawFill = true
			if res.Path == nil || res.Brush == nil {
				t.Fatalf("FillPath event missing path/brush summary: %+v", res)
			}
		}
		if ev.Type == "DrawImage" {
			sawImage = true
			assertImageSummary(t, res)
		}
	}
	if !sawFill || !sawImage {
		t.Fatalf("missing FillPath=%v or DrawImage=%v event", sawFill, sawImage)
	}
}

// assertPathSummary verifies a path summary is non-empty and leaks no
// coordinate arrays.
func assertPathSummary(t *testing.T, ev TraceEvent, res *EventResources) {
	t.Helper()
	if res.Path == nil {
		return
	}
	if res.Path.Verbs == 0 || res.Path.Empty {
		t.Fatalf("event %d path summary unexpectedly empty", ev.Index)
	}
	raw, _ := json.Marshal(res.Path)
	if strings.Contains(string(raw), "coords") {
		t.Fatalf("path summary leaks coordinates: %s", raw)
	}
}

// assertBrushSummary checks pool range and solid color encoding.
func assertBrushSummary(t *testing.T, r *Recording, res *EventResources) {
	t.Helper()
	if res.Brush == nil {
		return
	}
	if res.Brush.Ref < 0 || res.Brush.Ref >= r.Resources().BrushCount() {
		t.Fatalf("brush ref %d out of pool range", res.Brush.Ref)
	}
	if res.Brush.Kind == "solid" && res.Brush.Color != "#4080BFFF" {
		t.Fatalf("solid color = %q, want #4080BFFF (0.25,0.5,0.75,1)", res.Brush.Color)
	}
}

// assertImageSummary validates dimensions, byte estimate, type and that no
// pixel buffer is embedded.
func assertImageSummary(t *testing.T, res *EventResources) {
	t.Helper()
	if res.Image == nil {
		t.Fatal("DrawImage event missing image summary")
	}
	if res.Image.Width != 4 || res.Image.Height != 3 {
		t.Fatalf("image summary dims = %dx%d, want 4x3", res.Image.Width, res.Image.Height)
	}
	if res.Image.Bytes != 4*3*4 {
		t.Fatalf("image byte estimate = %d, want %d", res.Image.Bytes, 4*3*4)
	}
	if res.Image.Kind != "*image.RGBA" {
		t.Fatalf("image kind = %q, want *image.RGBA", res.Image.Kind)
	}
	raw, _ := json.Marshal(res)
	if bytes.Contains(raw, []byte(`"Pix"`)) {
		t.Fatalf("image summary leaks pixel buffer: %s", raw)
	}
}

// TestTraceGradientAndPatternSummaries exercises every brush kind so the
// summary switch has full structural coverage without pixel rendering.
func TestTraceGradientAndPatternSummaries(t *testing.T) {
	rec := NewRecorder(64, 64)
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	imgRef := rec.resources.AddImage(img)

	linear := NewLinearGradientBrush(0, 0, 10, 10).
		AddColorStop(0, gg.RGB(1, 0, 0)).
		AddColorStop(1, gg.RGB(0, 0, 1)).
		SetExtend(ExtendRepeat)
	radial := NewRadialGradientBrush(5, 5, 1, 8).AddColorStop(0.5, gg.Black)
	sweep := NewSweepGradientBrush(3, 3, 0).AddColorStop(1, gg.White)
	pattern := NewPatternBrush(imgRef).SetRepeat(RepeatX)

	rec.SetFillStyle(linear)
	rec.DrawRectangle(0, 0, 4, 4)
	rec.Fill()
	rec.SetFillStyle(radial)
	rec.FillRectangle(0, 0, 2, 2)
	rec.SetFillStyle(sweep)
	rec.FillRectangle(0, 0, 2, 2)
	rec.SetFillStyle(pattern)
	rec.FillRectangle(0, 0, 2, 2)
	r := rec.FinishRecording()

	events, err := r.TraceEvents(&traceSpyBackend{})
	if err != nil {
		t.Fatalf("TraceEvents: %v", err)
	}
	kinds := map[string]bool{}
	for _, ev := range events {
		if ev.Resources == nil || ev.Resources.Brush == nil {
			continue
		}
		brush := ev.Resources.Brush
		kinds[brush.Kind] = true
		raw, err := json.Marshal(brush)
		if err != nil {
			t.Fatalf("marshal brush: %v", err)
		}
		if !json.Valid(raw) {
			t.Fatalf("invalid brush JSON: %s", raw)
		}
		if brush.Kind == "pattern" && brush.Pattern == nil {
			t.Fatal("pattern brush missing nested pattern image summary")
		}
	}
	for _, want := range []string{"linearGradient", "radialGradient", "sweepGradient", "pattern"} {
		if !kinds[want] {
			t.Fatalf("missing brush kind %q in trace (saw %v)", want, kinds)
		}
	}
}

// TestTraceFontSummaryNoBulkData checks that a real font face is summarized
// by metadata and that a nil face is reported explicitly.
func TestTraceFontSummaryNoBulkData(t *testing.T) {
	face := loadGoRegularFace(t)
	rec := NewRecorder(200, 60)
	rec.SetFont(face)
	rec.SetFontSize(24)
	rec.SetFillRGB(0, 0, 0)
	rec.DrawString("Hi", 10, 40)
	// Second text command without a face.
	rec.SetFont(nil)
	rec.DrawString("Ghost", 10, 55)
	r := rec.FinishRecording()

	events, err := r.TraceEvents(&traceSpyBackend{})
	if err != nil {
		t.Fatalf("TraceEvents: %v", err)
	}
	fontEvents := 0
	for _, ev := range events {
		if ev.Type != "DrawText" {
			continue
		}
		fontEvents++
		if ev.Resources == nil || ev.Resources.Font == nil {
			t.Fatalf("DrawText event %d missing font summary", ev.Index)
		}
		raw, _ := json.Marshal(ev.Resources)
		lower := strings.ToLower(string(raw))
		// Font tables/glyph buffers must never appear; only metadata fields.
		if strings.Contains(lower, "table") || strings.Contains(lower, "glyphs") {
			t.Fatalf("font summary leaks bulk data: %s", raw)
		}
	}
	if fontEvents != 2 {
		t.Fatalf("DrawText events = %d, want 2", fontEvents)
	}
	var nilFontSeen bool
	for _, ev := range events {
		if ev.Type == "DrawText" && ev.Params["text"] == "Ghost" && ev.Resources.Font.Nil {
			nilFontSeen = true
		}
	}
	if !nilFontSeen {
		t.Fatal("nil-face DrawText not flagged with font.nil=true")
	}
}

// loadGoRegularFace loads the standard Go font for tests; defined here
// separately from the raster backend package which has its own helper.
func loadGoRegularFace(t *testing.T) text.Face {
	t.Helper()
	src, err := text.NewFontSource(goregular.TTF)
	if err != nil {
		t.Fatalf("NewFontSource: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src.Face(24)
}

// TestTraceNonFiniteValuesRejected proves NaN/Inf cannot enter the JSONL
// stream: the trace returns ErrNonFinite naming the offending field.
func TestTraceNonFiniteValuesRejected(t *testing.T) {
	cases := []struct {
		name string
		run  func(rec *Recorder)
		want string
	}{
		{
			name: "lineWidth",
			run:  func(rec *Recorder) { rec.SetLineWidth(math.NaN()) },
			want: "width",
		},
		{
			name: "transform",
			run:  func(rec *Recorder) { rec.SetTransform(Matrix{A: math.Inf(1)}) },
			want: "a",
		},
		{
			name: "miterLimit",
			run:  func(rec *Recorder) { rec.SetMiterLimit(math.Inf(-1)) },
			want: "limit",
		},
		{
			name: "dash",
			run:  func(rec *Recorder) { rec.SetDash(1, math.NaN()) },
			want: "pattern[1]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := NewRecorder(40, 40)
			tc.run(rec)
			r := rec.FinishRecording()

			var events []TraceEvent
			err := r.TracePlayback(&traceSpyBackend{}, TraceOptions{
				OnEvent: func(e TraceEvent) { events = append(events, e) },
			})
			if !errors.Is(err, ErrNonFinite) {
				t.Fatalf("err = %v, want ErrNonFinite", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to name field %q", err.Error(), tc.want)
			}
			// The malformed command's event must not have been emitted.
			for _, ev := range events {
				if ev.Backend.Status == "error" {
					t.Fatalf("non-finite build failure must not fabricate an error event: %+v", ev)
				}
			}
			// Writer path must produce the same error class.
			var buf bytes.Buffer
			if err := r.TracePlayback(&traceSpyBackend{}, TraceOptions{Writer: &buf}); !errors.Is(err, ErrNonFinite) {
				t.Fatalf("writer path err = %v, want ErrNonFinite", err)
			}
		})
	}
}

// TestTraceOversizedStringRejected verifies the string length gate on the
// two data-borne string fields (text content and font family).
func TestTraceOversizedStringRejected(t *testing.T) {
	long := strings.Repeat("x", DefaultMaxStringLength+1)
	rec := NewRecorder(200, 60)
	rec.DrawString(long, 0, 10)
	r := rec.FinishRecording()

	_, err := r.TraceEvents(&traceSpyBackend{})
	if !errors.Is(err, ErrStringTooLong) {
		t.Fatalf("long text err = %v, want ErrStringTooLong", err)
	}
	if !strings.Contains(err.Error(), "text") {
		t.Fatalf("err = %q should name field text", err.Error())
	}

	// A relaxed cap makes the same recording traceable.
	events, err := r.TraceEvents(&traceSpyBackend{}, func(o *TraceOptions) {
		o.MaxStringLength = DefaultMaxStringLength * 2
	})
	if err != nil {
		t.Fatalf("trace with raised cap: %v", err)
	}
	if len(events) != len(r.Commands())+2 {
		t.Fatalf("events = %d with raised cap", len(events))
	}
}

// TestTraceWriterFailurePropagates uses a failing writer to prove write
// errors surface wrapped and stop the run.
func TestTraceWriterFailurePropagates(t *testing.T) {
	r := sampleRecording()
	fw := &failingWriter{failAfter: 1}
	err := r.TracePlayback(&traceSpyBackend{}, TraceOptions{Writer: fw})
	if err == nil || !strings.Contains(err.Error(), "trace write failed") {
		t.Fatalf("err = %v, want wrapped trace write failure", err)
	}
}

type failingWriter struct {
	failAfter int
	writes    int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.failAfter {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

// TestTraceReentrantCallbackRejected ensures a callback that starts another
// trace of the same recording fails deterministically instead of recursing.
func TestTraceReentrantCallbackRejected(t *testing.T) {
	r := sampleRecording()
	backend := &traceSpyBackend{}
	err := r.TracePlayback(backend, TraceOptions{
		OnEvent: func(TraceEvent) {
			var nested []TraceEvent
			if nestedErr := r.TracePlayback(&traceSpyBackend{}, TraceOptions{
				OnEvent: func(e TraceEvent) { nested = append(nested, e) },
			}); !errors.Is(nestedErr, ErrTraceReentrant) {
				t.Errorf("nested trace err = %v, want ErrTraceReentrant", nestedErr)
			}
		},
	})
	if err != nil {
		t.Fatalf("outer trace should succeed: %v", err)
	}

	// Sequential traces on the same recording must still work (guard reset).
	if _, err := r.TraceEvents(&traceSpyBackend{}); err != nil {
		t.Fatalf("sequential trace after reentrant attempt failed: %v", err)
	}
}

// TestTraceConcurrentAndCrossRecording covers the two non-reentrant cases:
// concurrent traces of the same Recording from separate goroutines (allowed,
// Recording is immutable) and a nested trace of a *different* Recording
// inside a callback (also allowed).
func TestTraceConcurrentAndCrossRecording(t *testing.T) {
	r := sampleRecording()

	// Concurrent traces on the same Recording.
	const goroutines = 8
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			if err := r.TracePlayback(&traceSpyBackend{}, TraceOptions{Writer: &buf}); err != nil {
				errs <- err
				return
			}
			if buf.Len() == 0 {
				errs <- errors.New("concurrent trace produced empty output")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent trace: %v", err)
	}

	// Nested trace of a different Recording inside an OnEvent callback.
	other := sampleRecording()
	nestedRan := false
	err := r.TracePlayback(&traceSpyBackend{}, TraceOptions{
		OnEvent: func(TraceEvent) {
			if nestedRan {
				return
			}
			nestedRan = true
			if _, err := other.TraceEvents(&traceSpyBackend{}); err != nil {
				t.Errorf("nested trace of other recording: %v", err)
			}
		},
	})
	if err != nil {
		t.Fatalf("outer trace: %v", err)
	}
	if !nestedRan {
		t.Fatal("callback never executed")
	}
}
