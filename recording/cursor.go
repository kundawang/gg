package recording

import (
	"errors"
	"fmt"

	"github.com/gogpu/gg"
)

// Errors returned by cursor and session operations. All errors returned by
// this file wrap one of these sentinels (or the backend's own error), so
// callers can classify failures with errors.Is.
var (
	// ErrEmptyRange is returned when a cursor range contains no commands
	// (start == end). An empty interval is never reported as a played frame.
	ErrEmptyRange = errors.New("recording: empty playback range")

	// ErrInvalidRange is returned when a range's end precedes its start.
	ErrInvalidRange = errors.New("recording: invalid playback range")

	// ErrRangeOutOfBounds is returned when a range or snapshot index falls
	// outside the recording's command stream.
	ErrRangeOutOfBounds = errors.New("recording: playback range out of bounds")

	// ErrUnknownLabel is returned when a named label does not exist in the
	// recording.
	ErrUnknownLabel = errors.New("recording: unknown label")

	// ErrDanglingResource is returned when a command in the playback range
	// (or in the state prefix it depends on) references a resource that is
	// missing from the recording's resource pool.
	ErrDanglingResource = errors.New("recording: dangling resource reference")

	// ErrAlreadyResumed is returned when a cursor is resumed more than once.
	// Cursors are single-use; create a new cursor (reusing the same
	// StateSnapshot) to replay the same range again.
	ErrAlreadyResumed = errors.New("recording: cursor already resumed")

	// ErrSessionFinished is returned when resuming into a session that has
	// already been finished.
	ErrSessionFinished = errors.New("recording: session already finished")

	// ErrRecordingMismatch is returned when a cursor created from one
	// recording is resumed into a session bound to another recording.
	ErrRecordingMismatch = errors.New("recording: cursor belongs to a different recording")
)

// --------------------------------------------------------------------------
// Executor
// --------------------------------------------------------------------------

// executor replays commands against a backend, tracking the current transform
// and the backend's Save/Restore depth. It is shared by Recording.Playback
// (full replay) and Session (range replay): the same command always produces
// the same backend calls in both paths.
type executor struct {
	backend        Backend
	resources      *ResourcePool
	transform      Matrix
	transformStack []Matrix
	saveDepth      int // backend Save calls not yet matched by Restore
}

func newExecutor(backend Backend, resources *ResourcePool) *executor {
	return &executor{
		backend:        backend,
		resources:      resources,
		transform:      Identity(),
		transformStack: make([]Matrix, 0, 8),
	}
}

// apply replays a single command against the backend.
func (e *executor) apply(cmd Command) {
	switch c := cmd.(type) {
	case SaveCommand:
		e.transformStack = append(e.transformStack, e.transform)
		e.backend.Save()
		e.saveDepth++
	case RestoreCommand:
		e.backend.Restore()
		if e.saveDepth > 0 {
			e.saveDepth--
		}
		if len(e.transformStack) > 0 {
			e.transform = e.transformStack[len(e.transformStack)-1]
			e.transformStack = e.transformStack[:len(e.transformStack)-1]
		}
	case SetTransformCommand:
		e.transform = c.Matrix
		e.backend.SetTransform(c.Matrix)
	case SetClipCommand:
		path := e.resources.GetPath(c.Path)
		setPlaybackClip(e.backend, path, c.Rule, e.transform)
	case ClearClipCommand:
		e.backend.ClearClip()
	case ClipRoundRectCommand:
		rrPath := buildClipRoundRectPath(c, e.transform)
		setPlaybackClip(e.backend, rrPath, FillRuleNonZero, e.transform)
	case FillPathCommand:
		path := e.resources.GetPath(c.Path)
		brush := e.resources.GetBrush(c.Brush)
		e.backend.FillPath(path, brush, c.Rule)
	case StrokePathCommand:
		path := e.resources.GetPath(c.Path)
		brush := e.resources.GetBrush(c.Brush)
		e.backend.StrokePath(path, brush, c.Stroke)
	case FillRectCommand:
		brush := e.resources.GetBrush(c.Brush)
		e.backend.FillRect(c.Rect, brush)
	case StrokeRectCommand:
		brush := e.resources.GetBrush(c.Brush)
		path := gg.BuildPath().Rect(c.Rect.MinX, c.Rect.MinY, c.Rect.Width(), c.Rect.Height()).Build()
		// StrokeRectCommand stores world-space bounds. Isolate the identity
		// transform so backends do not apply the recording transform twice,
		// then restore their complete graphics state for later commands.
		// The Save/Restore pair is balanced, so saveDepth is unchanged.
		e.backend.Save()
		e.backend.SetTransform(Identity())
		e.backend.StrokePath(path, brush, c.Stroke)
		e.backend.Restore()
	case DrawImageCommand:
		img := e.resources.GetImage(c.Image)
		e.backend.DrawImage(img, c.SrcRect, c.DstRect, c.Options)
	case DrawTextCommand:
		brush := e.resources.GetBrush(c.Brush)
		e.backend.DrawText(c.Text, c.X, c.Y, c.Face, brush)
	case StrokeTextCommand:
		brush := e.resources.GetBrush(c.Brush)
		e.backend.DrawText(c.Text, c.X, c.Y, c.Face, brush)
	case SetFillStyleCommand, SetStrokeStyleCommand,
		SetLineWidthCommand, SetLineCapCommand, SetLineJoinCommand,
		SetMiterLimitCommand, SetDashCommand, SetFillRuleCommand,
		SetAntiAliasCommand, LabelCommand:
		// Style commands are no-ops at playback: the actual style is captured
		// in the drawing commands themselves. Labels are pure markers.
	}
}

// unwind restores the backend's state stack to the depth it had when this
// executor started, discarding any Save commands left unbalanced by the
// replayed stream.
func (e *executor) unwind() {
	for e.saveDepth > 0 {
		e.backend.Restore()
		e.saveDepth--
	}
}

// --------------------------------------------------------------------------
// State Snapshot
// --------------------------------------------------------------------------

// StateSnapshot captures the state-affecting command prefix of a recording up
// to a command index: the Save/Restore nesting, transforms, and clip commands
// that a range playback must rebuild before executing commands at or after
// that index. Style commands are not included because playback binds styles
// from the drawing commands themselves.
//
// A snapshot is computed once and can be reused to create any number of
// cursors starting at its index (see StateSnapshot.Cursor), which amortizes
// the prefix scan for timeline scrubbing across a fixed frame boundary.
type StateSnapshot struct {
	rec   *Recording
	index int
	ops   []Command
}

// Snapshot computes the reusable state snapshot at the given command index.
// The index may equal the command count (snapshot of the final state).
// Returns ErrRangeOutOfBounds for negative or beyond-the-end indices.
func (r *Recording) Snapshot(index int) (*StateSnapshot, error) {
	if index < 0 || index > len(r.commands) {
		return nil, fmt.Errorf("recording: snapshot index %d of %d commands: %w",
			index, len(r.commands), ErrRangeOutOfBounds)
	}
	ops := make([]Command, 0, 16)
	for i := 0; i < index; i++ {
		switch r.commands[i].(type) {
		case SaveCommand, RestoreCommand, SetTransformCommand,
			SetClipCommand, ClearClipCommand, ClipRoundRectCommand:
			ops = append(ops, r.commands[i])
		}
	}
	return &StateSnapshot{rec: r, index: index, ops: ops}, nil
}

// Index returns the command index this snapshot was taken at.
func (s *StateSnapshot) Index() int {
	return s.index
}

// Cursor creates a cursor covering [snapshot index, end). The snapshot's
// precomputed state prefix is reused, and the range's resource references are
// validated eagerly so a dangling resource fails here, not mid-frame.
func (s *StateSnapshot) Cursor(end int) (*Cursor, error) {
	r := s.rec
	start := s.index
	switch {
	case end < 0 || end > len(r.commands):
		return nil, fmt.Errorf("recording: range [%d, %d) of %d commands: %w",
			start, end, len(r.commands), ErrRangeOutOfBounds)
	case end < start:
		return nil, fmt.Errorf("recording: range [%d, %d): %w", start, end, ErrInvalidRange)
	case end == start:
		return nil, fmt.Errorf("recording: range [%d, %d): %w", start, end, ErrEmptyRange)
	}
	if err := r.validateResources(s, end); err != nil {
		return nil, err
	}
	return &Cursor{rec: r, start: start, end: end, snapshot: s}, nil
}

// validateResources checks that every command the cursor will replay — the
// snapshot's state prefix and the range itself — resolves its resource
// references against the pool.
func (r *Recording) validateResources(snapshot *StateSnapshot, end int) error {
	for i, cmd := range snapshot.ops {
		if err := r.validateCommandRefs(cmd); err != nil {
			return fmt.Errorf("recording: state prefix op %d (%s): %w", i, cmd.Type(), err)
		}
	}
	for i := snapshot.index; i < end; i++ {
		if err := r.validateCommandRefs(r.commands[i]); err != nil {
			return fmt.Errorf("recording: command %d (%s): %w", i, r.commands[i].Type(), err)
		}
	}
	return nil
}

// validateCommandRefs checks the resource references of a single command.
func (r *Recording) validateCommandRefs(cmd Command) error {
	switch c := cmd.(type) {
	case SetClipCommand:
		return r.checkPath(c.Path)
	case FillPathCommand:
		if err := r.checkPath(c.Path); err != nil {
			return err
		}
		return r.checkBrush(c.Brush)
	case StrokePathCommand:
		if err := r.checkPath(c.Path); err != nil {
			return err
		}
		return r.checkBrush(c.Brush)
	case FillRectCommand:
		return r.checkBrush(c.Brush)
	case StrokeRectCommand:
		return r.checkBrush(c.Brush)
	case DrawImageCommand:
		return r.checkImage(c.Image)
	case DrawTextCommand:
		return r.checkBrush(c.Brush)
	case StrokeTextCommand:
		return r.checkBrush(c.Brush)
	}
	return nil
}

func (r *Recording) checkPath(ref PathRef) error {
	if !ref.IsValid() || int(ref) >= r.resources.PathCount() || r.resources.GetPath(ref) == nil {
		return fmt.Errorf("path ref %d: %w", uint32(ref), ErrDanglingResource)
	}
	return nil
}

func (r *Recording) checkBrush(ref BrushRef) error {
	if !ref.IsValid() || int(ref) >= r.resources.BrushCount() || r.resources.GetBrush(ref) == nil {
		return fmt.Errorf("brush ref %d: %w", uint32(ref), ErrDanglingResource)
	}
	return nil
}

func (r *Recording) checkImage(ref ImageRef) error {
	if !ref.IsValid() || int(ref) >= r.resources.ImageCount() || r.resources.GetImage(ref) == nil {
		return fmt.Errorf("image ref %d: %w", uint32(ref), ErrDanglingResource)
	}
	return nil
}

// --------------------------------------------------------------------------
// Cursor
// --------------------------------------------------------------------------

// Cursor is a resumable playback position over a half-open command range
// [Start, End) of a recording. Resuming a cursor first rebuilds the
// Save/Restore, transform, clip, and style state that the range depends on
// (from a reusable StateSnapshot), then executes only the range's commands.
//
// Ranges are half-open, so adjacent cursors [a, b) and [b, c) tile the
// stream without drawing any command twice. A cursor is single-use: Resume
// marks it consumed, and a second Resume fails with ErrAlreadyResumed.
// Create cursors with Recording.Cursor, Recording.CursorForLabel,
// Recording.CursorBetweenLabels, or StateSnapshot.Cursor.
type Cursor struct {
	rec      *Recording
	start    int
	end      int
	snapshot *StateSnapshot
	resumed  bool
}

// Cursor creates a cursor covering the half-open command range [start, end).
// The state at start is reconstructed from the command prefix, and all
// resource references used by the range (and its state prefix) are validated
// before the cursor is returned.
//
// Empty ranges fail with ErrEmptyRange, reversed ranges with ErrInvalidRange,
// and out-of-bounds indices with ErrRangeOutOfBounds.
func (r *Recording) Cursor(start, end int) (*Cursor, error) {
	snapshot, err := r.Snapshot(start)
	if err != nil {
		return nil, err
	}
	return snapshot.Cursor(end)
}

// CursorForLabel creates a cursor covering the segment introduced by the
// given label: from the label's command up to (but not including) the next
// label, or to the end of the recording if there is none. Returns
// ErrUnknownLabel if the label does not exist.
func (r *Recording) CursorForLabel(label string) (*Cursor, error) {
	start, ok := r.LabelIndex(label)
	if !ok {
		return nil, fmt.Errorf("recording: label %q: %w", label, ErrUnknownLabel)
	}
	end := len(r.commands)
	for i := start + 1; i < len(r.commands); i++ {
		if _, isLabel := r.commands[i].(LabelCommand); isLabel {
			end = i
			break
		}
	}
	cursor, err := r.Cursor(start, end)
	if err != nil {
		return nil, fmt.Errorf("recording: label %q: %w", label, err)
	}
	return cursor, nil
}

// CursorBetweenLabels creates a cursor covering [startLabel, endLabel): from
// the first label's command up to (but not including) the second. Returns
// ErrUnknownLabel if either label does not exist.
func (r *Recording) CursorBetweenLabels(startLabel, endLabel string) (*Cursor, error) {
	start, ok := r.LabelIndex(startLabel)
	if !ok {
		return nil, fmt.Errorf("recording: label %q: %w", startLabel, ErrUnknownLabel)
	}
	end, ok := r.LabelIndex(endLabel)
	if !ok {
		return nil, fmt.Errorf("recording: label %q: %w", endLabel, ErrUnknownLabel)
	}
	cursor, err := r.Cursor(start, end)
	if err != nil {
		return nil, fmt.Errorf("recording: labels %q..%q: %w", startLabel, endLabel, err)
	}
	return cursor, nil
}

// Start returns the inclusive start command index of the cursor's range.
func (c *Cursor) Start() int {
	return c.start
}

// End returns the exclusive end command index of the cursor's range.
func (c *Cursor) End() int {
	return c.end
}

// Resume plays the cursor's range to the backend as a self-contained frame:
// it begins the backend, rebuilds the state prefix, executes the range,
// restores the backend's state stack, and ends the backend. Backend Begin/End
// errors are returned wrapped; a failed frame is never reported as success.
//
// To stitch several ranges into one backend frame, use a Session instead.
func (c *Cursor) Resume(backend Backend) error {
	session := c.rec.NewSession(backend)
	if err := session.Resume(c); err != nil {
		return err
	}
	return session.Finish()
}

// --------------------------------------------------------------------------
// Session
// --------------------------------------------------------------------------

// Session replays one or more cursors into a single backend frame. The
// backend is begun lazily on the first Resume and ended by Finish. After each
// Resume the backend's state (transform, clip, state-stack depth) is restored
// to what it was before, so consecutive cursors compose exactly like one full
// playback — adjacent ranges tile the stream without double-drawing.
type Session struct {
	rec      *Recording
	backend  Backend
	executor *executor
	started  bool
	finished bool
}

// NewSession creates a session that replays cursors of this recording into
// the given backend.
func (r *Recording) NewSession(backend Backend) *Session {
	return &Session{rec: r, backend: backend}
}

// Resume plays the cursor's range into the session's backend frame. The
// cursor is consumed by a successful start of playback; resuming it again
// (into any session) fails with ErrAlreadyResumed. If the backend's Begin
// fails, the cursor is not consumed and the session stays unstarted.
func (s *Session) Resume(c *Cursor) error {
	if s.finished {
		return fmt.Errorf("recording: resume cursor [%d, %d): %w", c.start, c.end, ErrSessionFinished)
	}
	if c.rec != s.rec {
		return fmt.Errorf("recording: resume cursor [%d, %d): %w", c.start, c.end, ErrRecordingMismatch)
	}
	if c.resumed {
		return fmt.Errorf("recording: resume cursor [%d, %d): %w", c.start, c.end, ErrAlreadyResumed)
	}
	if !s.started {
		if err := s.backend.Begin(s.rec.width, s.rec.height); err != nil {
			return fmt.Errorf("recording: begin backend (%dx%d): %w", s.rec.width, s.rec.height, err)
		}
		s.started = true
		s.executor = newExecutor(s.backend, s.rec.resources)
	}
	c.resumed = true

	// Isolate the interval in its own saved state so that, after the range
	// and any state it left open are unwound, the backend is exactly where it
	// was before this Resume.
	s.executor.apply(SaveCommand{})
	for _, op := range c.snapshot.ops {
		s.executor.apply(op)
	}
	for i := c.start; i < c.end; i++ {
		s.executor.apply(s.rec.commands[i])
	}
	s.executor.unwind()
	return nil
}

// Finish ends the session's backend frame. It is a no-op if no cursor was
// ever resumed or if the session is already finished. Backend End errors are
// returned wrapped.
func (s *Session) Finish() error {
	if s.finished {
		return nil
	}
	s.finished = true
	if !s.started {
		return nil
	}
	if err := s.backend.End(); err != nil {
		return fmt.Errorf("recording: end backend: %w", err)
	}
	return nil
}
