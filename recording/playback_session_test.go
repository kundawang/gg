package recording

import (
	"errors"
	"image"
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
)

// scriptedBackend is a Backend whose calls are recorded and whose lifecycle
// errors and each method can be made to fail or panic on demand.
//
// Backend drawing methods return nothing, so the only way a real backend can
// signal a runtime failure there is a panic. failOn therefore panics with an
// error value, which playback wraps in BackendPanicError and which unwraps back
// to the same error.
type scriptedBackend struct {
	calls []string

	beginErr error
	endErr   error
	failOn   string
	panicOn  string

	beginCalls int
	endCalls   int
	saves      int
	restores   int
	transform  Matrix
	stack      []Matrix
}

func (b *scriptedBackend) record(name string) {
	b.calls = append(b.calls, name)
	if b.failOn == name {
		panic(errors.New("injected " + name + " failure"))
	}
	if b.panicOn == name {
		panic("injected " + name + " panic")
	}
}

func (b *scriptedBackend) Begin(width, height int) error {
	b.beginCalls++
	b.transform = Identity()
	b.calls = append(b.calls, "Begin")
	return b.beginErr
}

func (b *scriptedBackend) End() error {
	b.endCalls++
	b.calls = append(b.calls, "End")
	return b.endErr
}

func (b *scriptedBackend) Save() {
	b.record("Save")
	b.saves++
	b.stack = append(b.stack, b.transform)
}

func (b *scriptedBackend) Restore() {
	b.record("Restore")
	b.restores++
	if n := len(b.stack); n > 0 {
		b.transform = b.stack[n-1]
		b.stack = b.stack[:n-1]
	}
}

func (b *scriptedBackend) SetTransform(m Matrix) {
	b.record("SetTransform")
	b.transform = m
}

func (b *scriptedBackend) SetClip(_ *gg.Path, _ FillRule)           { b.record("SetClip") }
func (b *scriptedBackend) ClearClip()                               { b.record("ClearClip") }
func (b *scriptedBackend) FillPath(_ *gg.Path, _ Brush, _ FillRule) { b.record("FillPath") }
func (b *scriptedBackend) StrokePath(_ *gg.Path, _ Brush, _ Stroke) {
	b.record("StrokePath")
}
func (b *scriptedBackend) FillRect(_ Rect, _ Brush) { b.record("FillRect") }
func (b *scriptedBackend) DrawImage(_ image.Image, _, _ Rect, _ ImageOptions) {
	b.record("DrawImage")
}
func (b *scriptedBackend) DrawText(_ string, _, _ float64, _ text.Face, _ Brush) {
	b.record("DrawText")
}

func TestPlaybackSessionRequiresBegin(t *testing.T) {
	backend := &scriptedBackend{}
	session := NewPlaybackSession(backend, NewResourcePool())

	if err := session.Dispatch(SaveCommand{}); !errors.Is(err, ErrSessionNotBegun) {
		t.Fatalf("Dispatch before Begin = %v, want ErrSessionNotBegun", err)
	}
	if err := session.Play(nil); !errors.Is(err, ErrSessionNotBegun) {
		t.Fatalf("Play before Begin = %v, want ErrSessionNotBegun", err)
	}
	if err := session.End(); !errors.Is(err, ErrSessionNotBegun) {
		t.Fatalf("End before Begin = %v, want ErrSessionNotBegun", err)
	}
	if backend.endCalls != 0 {
		t.Fatalf("End reached an uninitialized backend %d times, want 0", backend.endCalls)
	}
	if len(backend.calls) != 0 {
		t.Fatalf("uninitialized backend received calls: %v", backend.calls)
	}
}

func TestPlaybackSessionNilBackend(t *testing.T) {
	session := NewPlaybackSession(nil, nil)
	if err := session.Begin(10, 10); !errors.Is(err, ErrNilBackend) {
		t.Fatalf("Begin with nil backend = %v, want ErrNilBackend", err)
	}
	if err := session.End(); !errors.Is(err, ErrSessionNotBegun) {
		t.Fatalf("End after failed Begin = %v, want ErrSessionNotBegun", err)
	}
}

func TestPlaybackSessionBeginFailureSkipsEnd(t *testing.T) {
	backend := &scriptedBackend{beginErr: errors.New("cannot begin")}
	session := NewPlaybackSession(backend, NewResourcePool())

	if err := session.Begin(10, 10); err == nil || err.Error() != "cannot begin" {
		t.Fatalf("Begin error = %v, want the backend's error", err)
	}
	if err := session.Dispatch(SaveCommand{}); !errors.Is(err, ErrSessionNotBegun) {
		t.Fatalf("Dispatch after failed Begin = %v, want ErrSessionNotBegun", err)
	}
	if err := session.End(); !errors.Is(err, ErrSessionNotBegun) {
		t.Fatalf("End after failed Begin = %v, want ErrSessionNotBegun", err)
	}
	if backend.endCalls != 0 {
		t.Fatalf("End called %d times after failed Begin, want 0", backend.endCalls)
	}
}

func TestPlaybackSessionDoubleBegin(t *testing.T) {
	backend := &scriptedBackend{}
	session := NewPlaybackSession(backend, NewResourcePool())
	if err := session.Begin(10, 10); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := session.Begin(10, 10); !errors.Is(err, ErrSessionStarted) {
		t.Fatalf("second Begin = %v, want ErrSessionStarted", err)
	}
	if backend.beginCalls != 1 {
		t.Fatalf("backend Begin called %d times, want 1", backend.beginCalls)
	}
}

func TestPlaybackSessionEndIsIdempotent(t *testing.T) {
	backend := &scriptedBackend{endErr: errors.New("cannot end")}
	session := NewPlaybackSession(backend, NewResourcePool())
	if err := session.Begin(10, 10); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	first := session.End()
	second := session.End()
	if first == nil || first.Error() != "cannot end" {
		t.Fatalf("first End = %v, want the backend's error", first)
	}
	if !errors.Is(second, first) {
		t.Fatalf("second End = %v, want the same error as first End", second)
	}
	if backend.endCalls != 1 {
		t.Fatalf("backend End called %d times, want 1", backend.endCalls)
	}
	if err := session.Dispatch(SaveCommand{}); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("Dispatch after End = %v, want ErrSessionEnded", err)
	}
}

func TestPlaybackSessionBackendPanicIsWrapped(t *testing.T) {
	backend := &scriptedBackend{panicOn: "DrawText"}
	session := NewPlaybackSession(backend, NewResourcePool())
	if err := session.Begin(10, 10); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	err := session.Dispatch(DrawTextCommand{Text: "boom"})
	var panicErr *BackendPanicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("Dispatch panic = %v, want *BackendPanicError", err)
	}
	if panicErr.Operation != "DrawText" {
		t.Errorf("operation = %q, want DrawText", panicErr.Operation)
	}
	if panicErr.Value != "injected DrawText panic" {
		t.Errorf("panic value = %v, want the injected value", panicErr.Value)
	}
	if len(panicErr.Stack) == 0 {
		t.Error("panic error carries no stack trace")
	}

	// The session survives the panic and still ends the backend.
	if err := session.End(); err != nil {
		t.Fatalf("End after panic: %v", err)
	}
	if backend.endCalls != 1 {
		t.Fatalf("backend End called %d times, want 1", backend.endCalls)
	}
}

func TestPlaybackSessionPanicUnwrapsErrorValue(t *testing.T) {
	sentinel := errors.New("sentinel")
	backend := &scriptedBackend{panicOn: "ClearClip"}
	session := NewPlaybackSession(backend, NewResourcePool())
	if err := session.Begin(1, 1); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// Inject an error panic by wrapping the backend.
	errBackend := &panicErrorBackend{scriptedBackend: backend, err: sentinel}
	errSession := NewPlaybackSession(errBackend, NewResourcePool())
	if err := errSession.Begin(1, 1); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err := errSession.Dispatch(ClearClipCommand{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is(err, sentinel) = false; err = %v", err)
	}
	_ = session.End()
}

// panicErrorBackend panics with an error value from ClearClip.
type panicErrorBackend struct {
	*scriptedBackend
	err error
}

func (b *panicErrorBackend) ClearClip() { panic(b.err) }

func TestPlaybackSessionUnknownCommand(t *testing.T) {
	backend := &scriptedBackend{}
	session := NewPlaybackSession(backend, NewResourcePool())
	if err := session.Begin(10, 10); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	var unknown *UnknownCommandError
	err := session.Dispatch(&SaveCommand{})
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("Dispatch of pointer command = %v, want ErrUnknownCommand", err)
	}
	if !errors.As(err, &unknown) {
		t.Fatalf("Dispatch of pointer command = %v, want *UnknownCommandError", err)
	}

	if err := session.Dispatch(nil); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("Dispatch(nil) = %v, want ErrUnknownCommand", err)
	}
	// Backends saw nothing for the rejected commands.
	if len(backend.calls) != 2 || backend.calls[0] != "Begin" || backend.calls[1] != "Begin" {
		// Second Begin belongs to the other session's backend; this one only has Begin.
		if len(backend.calls) != 1 {
			t.Fatalf("backend calls = %v, want only Begin", backend.calls)
		}
	}
	_ = session.End()
}

func TestPlaybackPlayStopsAtFirstError(t *testing.T) {
	backend := &scriptedBackend{failOn: "FillRect"}
	session := NewPlaybackSession(backend, NewResourcePool())
	if err := session.Begin(10, 10); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	commands := []Command{
		SaveCommand{},
		FillRectCommand{Rect: NewRect(0, 0, 5, 5)},
		RestoreCommand{},
	}
	err := session.Play(commands)
	var panicErr *BackendPanicError
	if !errors.As(err, &panicErr) || panicErr.Operation != "FillRect" {
		t.Fatalf("Play error = %v, want a wrapped FillRect panic", err)
	}
	if backend.saves != 1 || backend.restores != 0 {
		t.Fatalf("Play did not stop at the failure: saves=%d restores=%d", backend.saves, backend.restores)
	}
	_ = session.End()
}

func TestPlaybackSessionRestoreEmptyStackForwardsToBackend(t *testing.T) {
	backend := &scriptedBackend{}
	session := NewPlaybackSession(backend, NewResourcePool())
	if err := session.Begin(10, 10); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	session.SetTransform(Translate(4, 5))
	if err := session.Dispatch(RestoreCommand{}); err != nil {
		t.Fatalf("Restore on empty stack: %v", err)
	}
	if backend.restores != 1 {
		t.Fatalf("backend Restore calls = %d, want 1", backend.restores)
	}
	if got, want := session.Transform(), Translate(4, 5); got != want {
		t.Fatalf("transform after empty Restore = %v, want unchanged %v", got, want)
	}
	if session.Depth() != 0 {
		t.Fatalf("depth = %d, want 0", session.Depth())
	}
}

func TestPlaybackSessionStateStackIsCopied(t *testing.T) {
	backend := &scriptedBackend{}
	session := NewPlaybackSession(backend, NewResourcePool())
	if err := session.Begin(10, 10); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	session.SetTransform(Translate(1, 2))
	if err := session.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	session.SetTransform(Translate(3, 4))
	if err := session.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	snapshot := session.Transforms()
	snapshot[0] = Translate(99, 99)
	snapshot[1] = Translate(98, 98)
	if session.Depth() != 2 {
		t.Fatalf("depth = %d, want 2", session.Depth())
	}
	if got, want := session.Transform(), Translate(3, 4); got != want {
		t.Fatalf("transform mutated through Transforms() copy: %v, want %v", got, want)
	}
	if err := session.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// Restoring the most recent snapshot puts back the state saved at the
	// second Save, i.e. Translate(3,4); the first snapshot holds Translate(1,2).
	if got, want := session.Transform(), Translate(3, 4); got != want {
		t.Fatalf("transform after first Restore = %v, want %v (mutation leaked)", got, want)
	}
	if err := session.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got, want := session.Transform(), Translate(1, 2); got != want {
		t.Fatalf("transform after second Restore = %v, want %v", got, want)
	}
	_ = session.End()
}

func TestPlaybackSessionStrokeRectRestoresTransform(t *testing.T) {
	backend := &scriptedBackend{}
	session := NewPlaybackSession(backend, NewResourcePool())
	if err := session.Begin(10, 10); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	transform := Matrix{A: 0, B: -1, C: 100, D: 1, E: 0}
	session.SetTransform(transform)

	if err := session.Dispatch(StrokeRectCommand{Rect: NewRect(10, 20, 30, 40)}); err != nil {
		t.Fatalf("StrokeRect: %v", err)
	}
	if backend.saves != 1 || backend.restores != 1 {
		t.Fatalf("isolation calls = Save %d / Restore %d, want 1/1", backend.saves, backend.restores)
	}
	if got, want := session.Transform(), transform; got != want {
		t.Fatalf("session transform = %v, want restored %v", got, want)
	}
	_ = session.End()
}

func TestPlaybackSessionStrokeRectRestoresAfterStrokeFailure(t *testing.T) {
	backend := &scriptedBackend{failOn: "StrokePath"}
	session := NewPlaybackSession(backend, NewResourcePool())
	if err := session.Begin(10, 10); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	transform := Translate(7, 8)
	session.SetTransform(transform)

	err := session.Dispatch(StrokeRectCommand{Rect: NewRect(0, 0, 10, 10)})
	if err == nil {
		t.Fatal("StrokeRect reported no error, want injected StrokePath failure")
	}
	if backend.saves != 1 || backend.restores != 1 {
		t.Fatalf("isolation calls = Save %d / Restore %d after failure, want 1/1", backend.saves, backend.restores)
	}
	if got, want := session.Transform(), transform; got != want {
		t.Fatalf("session transform after failure = %v, want %v", got, want)
	}
	if session.Depth() != 0 {
		t.Fatalf("session depth after failure = %d, want 0", session.Depth())
	}
	_ = session.End()
}

func TestPlaybackSessionSetClipRestoresTransformOnFailure(t *testing.T) {
	backend := &scriptedBackend{failOn: "SetClip"}
	session := NewPlaybackSession(backend, NewResourcePool())
	if err := session.Begin(10, 10); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	transform := Translate(3, 4)
	session.SetTransform(transform)

	err := session.Dispatch(SetClipCommand{Path: PathRef(0)})
	var panicErr *BackendPanicError
	if !errors.As(err, &panicErr) || panicErr.Operation != "SetClip" {
		t.Fatalf("SetClip error = %v, want a wrapped SetClip panic", err)
	}
	if got, want := session.Transform(), transform; got != want {
		t.Fatalf("session transform after clip failure = %v, want restored %v", got, want)
	}
	if got, want := backend.transform, transform; got != want {
		t.Fatalf("backend transform after clip failure = %v, want restored %v", got, want)
	}
	_ = session.End()
}

func TestPlaybackSessionNilResourcesResolveToNil(t *testing.T) {
	var seenPath *gg.Path
	var seenBrush Brush
	backend := newCaptureBackend(func(p *gg.Path, br Brush, _ FillRule) {
		seenPath, seenBrush = p, br
	})
	session := NewPlaybackSession(backend, nil)
	if err := session.Begin(10, 10); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := session.Dispatch(FillPathCommand{Path: PathRef(3), Brush: BrushRef(2)}); err != nil {
		t.Fatalf("FillPath: %v", err)
	}
	if seenPath != nil || seenBrush != nil {
		t.Fatalf("nil pool resolved to path=%v brush=%v, want nils", seenPath, seenBrush)
	}
	_ = session.End()
}

// captureBackend delegates to scriptedBackend but exposes FillPath arguments.
type captureBackend struct {
	*scriptedBackend
	onFillPath func(*gg.Path, Brush, FillRule)
}

func newCaptureBackend(onFillPath func(*gg.Path, Brush, FillRule)) *captureBackend {
	return &captureBackend{scriptedBackend: &scriptedBackend{}, onFillPath: onFillPath}
}

func (b *captureBackend) FillPath(p *gg.Path, brush Brush, rule FillRule) {
	b.onFillPath(p, brush, rule)
}

func TestRecordingPlaybackFinalizesAfterCommandFailure(t *testing.T) {
	backend := &scriptedBackend{panicOn: "FillRect"}
	rec := NewRecorder(10, 10)
	rec.FillRectangle(0, 0, 5, 5)
	recording := rec.FinishRecording()

	err := recording.Playback(backend)
	var panicErr *BackendPanicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("Playback error = %v, want *BackendPanicError", err)
	}
	if backend.endCalls != 1 {
		t.Fatalf("End called %d times after a command panic, want 1", backend.endCalls)
	}
}

func TestRecordingPlaybackCombinesCommandAndEndErrors(t *testing.T) {
	backend := &scriptedBackend{failOn: "FillRect", endErr: errors.New("teardown failed")}
	rec := NewRecorder(10, 10)
	rec.FillRectangle(0, 0, 5, 5)
	recording := rec.FinishRecording()

	err := recording.Playback(backend)
	if err == nil {
		t.Fatal("Playback reported no error, want command and End failures")
	}
	if !contains(err.Error(), "injected FillRect failure") {
		t.Errorf("combined error %q is missing the command failure", err)
	}
	if !contains(err.Error(), "teardown failed") {
		t.Errorf("combined error %q is missing the End failure", err)
	}
}

func TestRecordingPlaybackBeginFailure(t *testing.T) {
	backend := &scriptedBackend{beginErr: errors.New("no device")}
	rec := NewRecorder(10, 10)
	rec.FillRectangle(0, 0, 5, 5)
	recording := rec.FinishRecording()

	err := recording.Playback(backend)
	if err == nil || err.Error() != "no device" {
		t.Fatalf("Playback error = %v, want the Begin error", err)
	}
	if backend.endCalls != 0 {
		t.Fatalf("End called %d times after Begin failure, want 0", backend.endCalls)
	}
}

func TestRecordingNewPlaybackSession(t *testing.T) {
	rec := NewRecorder(40, 30)
	rec.Save()
	rec.Translate(1, 2)
	rec.DrawRectangle(0, 0, 10, 10)
	rec.Fill()
	rec.Restore()
	recorded := rec.FinishRecording()

	backend := &scriptedBackend{}
	session := recorded.NewPlaybackSession(backend)
	if err := session.Begin(recorded.Width(), recorded.Height()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := session.Play(recorded.Commands()); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if err := session.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	if session.Depth() != 0 {
		t.Fatalf("depth = %d, want 0 after balanced Save/Restore", session.Depth())
	}
	if got, want := session.Transform(), Identity(); got != want {
		t.Fatalf("final transform = %v, want %v", got, want)
	}
}

func contains(haystack, needle string) bool {
	return needle == "" || indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
