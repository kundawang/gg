package recording

import (
	"errors"
	"fmt"
	"image"
	"math"
	"runtime/debug"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
)

// Playback lifecycle and dispatch errors.
var (
	// ErrNilBackend is returned when a session is asked to drive a nil backend.
	ErrNilBackend = errors.New("recording: playback session has no backend")
	// ErrSessionNotBegun is returned when an operation is dispatched before Begin.
	ErrSessionNotBegun = errors.New("recording: playback session not begun")
	// ErrSessionStarted is returned when Begin is called twice on one session.
	ErrSessionStarted = errors.New("recording: playback session already begun")
	// ErrSessionEnded is returned when an operation is dispatched after End.
	ErrSessionEnded = errors.New("recording: playback session already ended")
	// ErrUnknownCommand is returned when a recording contains a command the
	// playback session does not know how to dispatch.
	ErrUnknownCommand = errors.New("recording: unknown command")
)

// BackendPanicError wraps a panic raised by a Backend method. Playback recovers
// such panics instead of allowing them to unwind through Recording.Playback,
// and still runs End exactly once so the backend can release its resources.
type BackendPanicError struct {
	// Operation is the Backend method that panicked (for example "FillPath").
	Operation string
	// Value is the value passed to panic.
	Value any
	// Stack is the stack trace captured at the point of recovery.
	Stack []byte
}

// Error implements error.
func (e *BackendPanicError) Error() string {
	return fmt.Sprintf("recording: backend panicked during %s: %v", e.Operation, e.Value)
}

// Unwrap returns the panic value when it is itself an error, so callers can use
// errors.Is/errors.As against the original cause.
func (e *BackendPanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// UnknownCommandError reports a command that PlaybackSession cannot dispatch.
// Recording always produces value commands; custom recordings that carry
// commands from a newer version (or pointer implementations) hit this error.
type UnknownCommandError struct {
	// Command is the unsupported command. It may be nil.
	Command Command
}

// Error implements error.
func (e *UnknownCommandError) Error() string {
	if e.Command == nil {
		return "recording: unknown command <nil>"
	}
	return fmt.Sprintf("recording: unknown command %T (type %s)", e.Command, safeCommandType(e.Command))
}

// Is reports whether target is [ErrUnknownCommand].
func (e *UnknownCommandError) Is(target error) bool {
	return target == ErrUnknownCommand
}

// safeCommandType returns cmd.Type() without letting a misbehaving Command
// implementation (for example a typed nil pointer) take down error reporting.
func safeCommandType(cmd Command) (name string) {
	defer func() {
		if recover() != nil {
			name = "?"
		}
	}()
	return cmd.Type().String()
}

// PlaybackSession replays recording commands to a [Backend]. It owns the
// backend lifecycle (Begin/End), the mirrored graphics state used by
// Save/Restore/SetTransform/SetClip, resource resolution, and command dispatch.
//
// Splitting these concerns out of [Recording.Playback] keeps every command
// handler in one place: adding a command means adding one case to
// [PlaybackSession.Dispatch], and the session's state operations make the
// clip/transform scoping that some commands need explicit and testable.
//
// A session is single-use: Begin may be called once, End may be called any
// number of times (only the first call reaches the backend), and commands may
// only be dispatched between them. All failures are reported as errors; backend
// panics are recovered and wrapped in [BackendPanicError].
//
// The mirrored state stack is never exposed as a slice. [PlaybackSession.Transforms]
// returns a copy.
type PlaybackSession struct {
	backend   Backend
	resources *ResourcePool

	// Mirrored graphics state. Recording paths and drawing commands already
	// carry world-space geometry, so the transform mirror exists only so
	// ClipRoundRect can be resolved against the transform at draw time.
	transform Matrix
	stack     []Matrix

	begun  bool
	ended  bool
	endErr error
}

// NewPlaybackSession creates a session that will drive backend using resources
// from the given pool. resources may be nil; path/brush/image references then
// resolve to nil, matching the defensive behavior of the resource pool.
//
// The session starts with an identity transform and an empty state stack.
func NewPlaybackSession(backend Backend, resources *ResourcePool) *PlaybackSession {
	return &PlaybackSession{
		backend:   backend,
		resources: resources,
		transform: Identity(),
		stack:     make([]Matrix, 0, 8),
	}
}

// Begin initializes the backend. It returns the backend's error, a wrapped
// panic, or one of [ErrNilBackend], [ErrSessionStarted], [ErrSessionEnded].
// When Begin fails the session is not considered started: End will not call the
// backend, because a backend that failed to initialize must not be finalized.
func (s *PlaybackSession) Begin(width, height int) error {
	if s.ended {
		return ErrSessionEnded
	}
	if s.begun {
		return ErrSessionStarted
	}
	if s.backend == nil {
		return ErrNilBackend
	}

	var beginErr error
	if err := s.call("Begin", func() { beginErr = s.backend.Begin(width, height) }); err != nil {
		return err
	}
	if beginErr != nil {
		return beginErr
	}

	s.begun = true
	return nil
}

// End finalizes the backend. It is idempotent: only the first call reaches the
// backend, and later calls return the same error. A backend panic is wrapped in
// [BackendPanicError]. If the session never began successfully, End does not
// call the backend and returns [ErrSessionNotBegun].
//
// End releases the session's mirrored state stack; the final transform remains
// available for inspection.
func (s *PlaybackSession) End() error {
	if s.ended {
		return s.endErr
	}
	s.ended = true
	s.stack = nil

	if !s.begun {
		s.endErr = ErrSessionNotBegun
		return s.endErr
	}

	var endErr error
	if err := s.call("End", func() { endErr = s.backend.End() }); err != nil {
		s.endErr = err
		return s.endErr
	}

	s.endErr = endErr
	return s.endErr
}

// Transform returns the current mirrored transformation matrix.
func (s *PlaybackSession) Transform() Matrix {
	return s.transform
}

// Depth returns the number of saved states currently on the mirrored stack.
func (s *PlaybackSession) Depth() int {
	return len(s.stack)
}

// Transforms returns a copy of the mirrored state stack, oldest first.
// Mutating the returned slice does not affect the session.
func (s *PlaybackSession) Transforms() []Matrix {
	out := make([]Matrix, len(s.stack))
	copy(out, s.stack)
	return out
}

// Play dispatches every command in order. It stops at the first error and
// leaves finalization to the caller (typically [Recording.Playback], which
// always calls End afterwards).
func (s *PlaybackSession) Play(commands []Command) error {
	if err := s.ready(); err != nil {
		return err
	}
	for _, cmd := range commands {
		if err := s.Dispatch(cmd); err != nil {
			return err
		}
	}
	return nil
}

// Dispatch resolves and executes a single command through the session state.
// Recognized style commands are no-ops: their effect is captured in the drawing
// commands themselves. Anything else returns [UnknownCommandError], which
// matches [ErrUnknownCommand] via errors.Is.
func (s *PlaybackSession) Dispatch(cmd Command) error {
	if err := s.ready(); err != nil {
		return err
	}
	if cmd == nil {
		return &UnknownCommandError{}
	}
	if handler, ok := styleCommandHandler(cmd); ok {
		_ = handler // style commands intentionally have no playback effect
		return nil
	}

	switch c := cmd.(type) {
	// State commands.
	case SaveCommand:
		return s.Save()
	case RestoreCommand:
		return s.Restore()
	case SetTransformCommand:
		return s.SetTransform(c.Matrix)
	case SetClipCommand:
		return s.SetClip(s.resolvePath(c.Path), c.Rule)
	case ClearClipCommand:
		return s.ClearClip()
	case ClipRoundRectCommand:
		return s.ClipRoundRect(c)

	// Drawing commands.
	case FillPathCommand:
		return s.FillPath(s.resolvePath(c.Path), s.resolveBrush(c.Brush), c.Rule)
	case StrokePathCommand:
		return s.StrokePath(s.resolvePath(c.Path), s.resolveBrush(c.Brush), c.Stroke)
	case FillRectCommand:
		return s.FillRect(c.Rect, s.resolveBrush(c.Brush))
	case StrokeRectCommand:
		return s.StrokeRect(c.Rect, s.resolveBrush(c.Brush), c.Stroke)
	case DrawImageCommand:
		return s.DrawImage(s.resolveImage(c.Image), c.SrcRect, c.DstRect, c.Options)
	case DrawTextCommand:
		return s.DrawText(c.Text, c.X, c.Y, c.Face, s.resolveBrush(c.Brush))
	case StrokeTextCommand:
		return s.DrawText(c.Text, c.X, c.Y, c.Face, s.resolveBrush(c.Brush))

	default:
		return &UnknownCommandError{Command: cmd}
	}
}

// styleCommandHandler reports whether cmd is a style command. Style commands are
// recorded for inspection, but the concrete style is embedded in the drawing
// commands, so playback deliberately ignores them. Keeping them in one predicate
// keeps Dispatch's type switch focused on commands with runtime effects.
func styleCommandHandler(cmd Command) (Command, bool) {
	switch cmd.(type) {
	case SetFillStyleCommand, SetStrokeStyleCommand:
	case SetLineWidthCommand, SetLineCapCommand, SetLineJoinCommand:
	case SetMiterLimitCommand, SetDashCommand, SetFillRuleCommand:
	case SetAntiAliasCommand:
	default:
		return nil, false
	}
	return cmd, true
}

// Save snapshots the mirrored transform and saves the backend state.
// The snapshot is recorded only after the backend accepts Save, so a backend
// panic cannot leave the mirror one level deeper than the backend.
func (s *PlaybackSession) Save() error {
	if err := s.ready(); err != nil {
		return err
	}
	if err := s.call("Save", func() { s.backend.Save() }); err != nil {
		return err
	}
	s.stack = append(s.stack, s.transform)
	return nil
}

// Restore restores the backend state and pops the mirrored transform.
// The backend is always asked to restore, even when the mirrored stack is empty,
// because the backend owns its own stack and may have entries the mirror does
// not know about. An empty mirror simply leaves the session transform unchanged.
// The mirror is popped even if the backend panics, keeping it balanced for any
// later inspection of the session.
func (s *PlaybackSession) Restore() error {
	if err := s.ready(); err != nil {
		return err
	}
	defer s.popMirror()
	return s.call("Restore", func() { s.backend.Restore() })
}

// SetTransform updates the mirrored transform and the backend transform.
func (s *PlaybackSession) SetTransform(m Matrix) error {
	if err := s.ready(); err != nil {
		return err
	}
	s.transform = m
	return s.call("SetTransform", func() { s.backend.SetTransform(m) })
}

// SetClip installs a world-space clip without disturbing the backend's current
// transform. Save/Restore cannot be used here because Restore would also
// discard the newly installed clip. The mirrored transform is restored even if
// the backend's SetClip fails or panics.
func (s *PlaybackSession) SetClip(path *gg.Path, rule FillRule) error {
	if err := s.ready(); err != nil {
		return err
	}

	transform := s.transform
	if err := s.SetTransform(Identity()); err != nil {
		return err
	}
	clipErr := s.call("SetClip", func() { s.backend.SetClip(path, rule) })
	// Always attempt to put the transform back: a backend that panicked in
	// SetClip must still see its pre-clip transform if it can accept one.
	restoreErr := s.SetTransform(transform)

	return joinErrors(clipErr, restoreErr)
}

// ClearClip removes the backend's clipping region.
func (s *PlaybackSession) ClearClip() error {
	return s.invoke("ClearClip", func() { s.backend.ClearClip() })
}

// ClipRoundRect installs a rounded-rectangle clip. The command's rectangle is
// in user space and is resolved through the session's current transform.
func (s *PlaybackSession) ClipRoundRect(c ClipRoundRectCommand) error {
	path := buildClipRoundRectPath(c, s.transform)
	return s.SetClip(path, FillRuleNonZero)
}

// FillPath fills a resolved path with the brush.
func (s *PlaybackSession) FillPath(path *gg.Path, brush Brush, rule FillRule) error {
	return s.invoke("FillPath", func() { s.backend.FillPath(path, brush, rule) })
}

// StrokePath strokes a resolved path with the brush and stroke style.
func (s *PlaybackSession) StrokePath(path *gg.Path, brush Brush, stroke Stroke) error {
	return s.invoke("StrokePath", func() { s.backend.StrokePath(path, brush, stroke) })
}

// FillRect fills a world-space rectangle.
func (s *PlaybackSession) FillRect(rect Rect, brush Brush) error {
	return s.invoke("FillRect", func() { s.backend.FillRect(rect, brush) })
}

// StrokeRect strokes a world-space rectangle. StrokeRectCommand stores the
// rectangle as normalized world-space bounds, so playback isolates the identity
// transform: backends must not apply the recording transform twice. The complete
// backend state is restored afterwards (even on failure) for later commands.
func (s *PlaybackSession) StrokeRect(rect Rect, brush Brush, stroke Stroke) (err error) {
	if err := s.ready(); err != nil {
		return err
	}

	path := gg.BuildPath().Rect(rect.MinX, rect.MinY, rect.Width(), rect.Height()).Build()

	if err := s.Save(); err != nil {
		return err
	}
	defer func() {
		err = joinErrors(err, s.Restore())
	}()

	if err := s.SetTransform(Identity()); err != nil {
		return err
	}
	return s.StrokePath(path, brush, stroke)
}

// DrawImage draws an image from the source rectangle to the destination rectangle.
func (s *PlaybackSession) DrawImage(img image.Image, src, dst Rect, opts ImageOptions) error {
	return s.invoke("DrawImage", func() { s.backend.DrawImage(img, src, dst, opts) })
}

// DrawText draws text at the given baseline position.
func (s *PlaybackSession) DrawText(str string, x, y float64, face text.Face, brush Brush) error {
	return s.invoke("DrawText", func() { s.backend.DrawText(str, x, y, face, brush) })
}

// --------------------------------------------------------------------------
// Internal helpers
// --------------------------------------------------------------------------

// ready reports whether commands may be dispatched.
func (s *PlaybackSession) ready() error {
	if s.ended {
		return ErrSessionEnded
	}
	if !s.begun {
		return ErrSessionNotBegun
	}
	return nil
}

// popMirror restores the most recently saved transform, if any.
func (s *PlaybackSession) popMirror() {
	if n := len(s.stack); n > 0 {
		s.transform = s.stack[n-1]
		s.stack = s.stack[:n-1]
	}
}

// invoke calls a backend operation that requires an initialized session.
func (s *PlaybackSession) invoke(operation string, fn func()) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.call(operation, fn)
}

// call runs fn and converts a backend panic into a [BackendPanicError].
func (s *PlaybackSession) call(operation string, fn func()) (err error) {
	if s.backend == nil {
		return ErrNilBackend
	}
	defer func() {
		if r := recover(); r != nil {
			err = &BackendPanicError{
				Operation: operation,
				Value:     r,
				Stack:     debug.Stack(),
			}
		}
	}()
	fn()
	return nil
}

func (s *PlaybackSession) resolvePath(ref PathRef) *gg.Path {
	if s.resources == nil {
		return nil
	}
	return s.resources.GetPath(ref)
}

func (s *PlaybackSession) resolveBrush(ref BrushRef) Brush {
	if s.resources == nil {
		return nil
	}
	return s.resources.GetBrush(ref)
}

func (s *PlaybackSession) resolveImage(ref ImageRef) image.Image {
	if s.resources == nil {
		return nil
	}
	return s.resources.GetImage(ref)
}

// joinErrors combines two errors without wrapping a single error in a joinError.
func joinErrors(first, second error) error {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	default:
		return errors.Join(first, second)
	}
}

// --------------------------------------------------------------------------
// Clip geometry
// --------------------------------------------------------------------------

// buildClipRoundRectPath builds a world-space rounded-rectangle clip path.
// For uniform axis-aligned transforms, uses the fast two-corner + scaled radius
// path. For rotation/shear/non-uniform scale, builds the rounded rect in user
// space and transforms the full path — matching Context.ClipRoundRect (#503).
func buildClipRoundRectPath(c ClipRoundRectCommand, t Matrix) *gg.Path {
	if isUniformAxisAligned(t) {
		x1, y1 := t.TransformPoint(c.X, c.Y)
		x2, y2 := t.TransformPoint(c.X+c.W, c.Y+c.H)
		x := math.Min(x1, x2)
		y := math.Min(y1, y2)
		w := math.Abs(x2 - x1)
		h := math.Abs(y2 - y1)
		radius := 0.0
		if c.Radius > 0 {
			radius = c.Radius * t.ScaleFactor()
			radius = math.Min(radius, math.Min(w, h)/2)
		}
		return gg.BuildPath().RoundRect(x, y, w, h, radius).Build()
	}
	px, py := c.X, c.Y
	pw, ph := math.Abs(c.W), math.Abs(c.H)
	if c.W < 0 {
		px += c.W
	}
	if c.H < 0 {
		py += c.H
	}
	userPath := gg.NewPath()
	userPath.RoundedRectangle(px, py, pw, ph, c.Radius)
	return userPath.Transform(gg.Matrix{
		A: t.A, B: t.B, C: t.C,
		D: t.D, E: t.E, F: t.F,
	})
}

// isUniformAxisAligned reports whether the transform keeps rounded rectangles
// axis-aligned with circular (not elliptical) corners.
func isUniformAxisAligned(t Matrix) bool {
	switch {
	case t.B == 0 && t.D == 0:
		return math.Abs(t.A) == math.Abs(t.E)
	case t.A == 0 && t.E == 0:
		return math.Abs(t.B) == math.Abs(t.D)
	default:
		return false
	}
}
