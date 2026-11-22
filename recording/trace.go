package recording

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"reflect"
	"runtime"
	"sort"
	"sync"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
)

// Structured replay tracing.
//
// When two backends produce different pixels, the final image alone cannot
// identify which command changed observable state. TracePlayback replays a
// Recording exactly as Playback does while emitting one structured event per
// lifecycle phase and per command. Each event carries the command index,
// command type, a summary of every resource it touches, the reconstructed
// transform and clip state, and the result reported by the backend.
//
// The trace output is deterministic for the same Recording: field order,
// number formatting and event ordering are fixed, and bulky resources
// (image pixels, font binaries, path coordinates) appear as summaries only.

// Sentinel errors for trace validation. Callers can use errors.Is to branch
// on the specific failure without parsing error strings.
var (
	// ErrNilBackend is returned when Playback or TracePlayback receives a
	// nil backend.
	ErrNilBackend = errors.New("recording: backend is nil")

	// ErrTraceNoSink is returned when TracePlayback has neither a Writer nor
	// an OnEvent callback to receive events.
	ErrTraceNoSink = errors.New("recording: trace requires Writer or OnEvent")

	// ErrTraceBothSinks is returned when TracePlayback is configured with
	// both a Writer and an OnEvent callback; exactly one sink is allowed so
	// events cannot be silently duplicated or diverge.
	ErrTraceBothSinks = errors.New("recording: trace accepts Writer or OnEvent, not both")

	// ErrTraceReentrant is returned when an OnEvent callback starts another
	// TracePlayback on the same Recording. Allowing that would recurse into
	// the replay and produce an event stream with no stable ordering.
	ErrTraceReentrant = errors.New("recording: trace callback must not start another trace of the same recording")

	// ErrNonFinite is returned when a recorded numeric value is NaN or
	// +/-Inf. JSON has no representation for those values, so emitting them
	// would corrupt the JSONL stream; the trace aborts with this error and
	// the offending command's context.
	ErrNonFinite = errors.New("recording: trace value is NaN or Inf")

	// ErrStringTooLong is returned when a recorded string exceeds
	// MaxStringLength. The limit prevents a single command (e.g. a huge text
	// run) from producing an unbounded trace line.
	ErrStringTooLong = errors.New("recording: trace string exceeds MaxStringLength")
)

// DefaultMaxStringLength is the default per-string length cap for trace
// fields that originate from recorded data (text, font family).
const DefaultMaxStringLength = 256

// hardMaxStringLength bounds how large a caller-supplied MaxStringLength may
// be, keeping a single JSONL line bounded even when the caller relaxes the
// default cap.
const hardMaxStringLength = 1 << 20 // 1 MiB

// TraceOptions configures a traced playback.
//
// Exactly one of Writer or OnEvent must be set. Writer receives one compact
// JSON object per line (JSONL); OnEvent receives the same events in memory
// for programmatic comparison (use TraceEvents for the common collect case).
type TraceOptions struct {
	// Writer receives JSONL-encoded events, one JSON object per line.
	Writer io.Writer

	// OnEvent is called with each trace event in replay order. It must not
	// invoke TracePlayback on the same Recording (ErrTraceReentrant), and it
	// must not mutate the Recording or its resources.
	OnEvent func(TraceEvent)

	// MaxStringLength caps the byte length of data-borne strings (command
	// text, font family names). Zero means DefaultMaxStringLength. Values
	// above the internal hard cap are rejected. Error messages, which must
	// remain available for debugging, are truncated rather than rejected.
	MaxStringLength int

	// FailAfter, when non-nil, is consulted before each command is
	// dispatched. Returning a non-nil error simulates a backend failure at
	// that command: the backend method is not called for the command, an
	// error event carrying the full command context is emitted, replay stops
	// without an "end" event, and the same error is returned. A nil return
	// lets replay proceed. This is the deterministic injection point for
	// mid-playback failure tests without requiring a custom backend.
	FailAfter func(index int, cmd Command) error
}

// TraceEvent is one structured record emitted during a traced playback.
//
// JSON field order is fixed by struct declaration order, so replaying the
// same Recording twice yields byte-identical JSONL for identical results.
type TraceEvent struct {
	// Index is the zero-based command index, or -1 for lifecycle events
	// ("begin" before the first command and "end" after the last).
	Index int `json:"index"`

	// Phase is "begin", "command" or "end".
	Phase string `json:"phase"`

	// Type is the command type name ("FillPath", "SetTransform", ...) and
	// is empty for lifecycle events.
	Type string `json:"type,omitempty"`

	// Params holds command-specific scalar parameters. nil when a command
	// has no parameters beyond its resource references. Map keys are sorted
	// during JSON encoding.
	Params map[string]any `json:"params,omitempty"`

	// Resources summarizes the resources the command references. nil when
	// the command references no pooled resources.
	Resources *EventResources `json:"resources,omitempty"`

	// Transform is the reconstructed current transform matrix at the time
	// the command is dispatched (after SetTransform/Save/Restore bookkeeping).
	Transform MatrixInfo `json:"transform"`

	// Clip describes the reconstructed clip state at dispatch time.
	Clip ClipInfo `json:"clip"`

	// Backend reports the backend's result for this event. It is present on
	// every event: lifecycle events carry Begin/End errors, command events
	// carry "ok" or "error" plus the number of Backend interface calls the
	// dispatch made (e.g. StrokeRect issues four calls).
	Backend BackendResult `json:"backend"`
}

// EventResources contains summaries of the pooled resources referenced by a
// command. Every field is optional; a nil pointer means "not referenced".
// Summaries never contain pixel buffers, font binaries or coordinate arrays.
type EventResources struct {
	Path  *PathInfo  `json:"path,omitempty"`
	Brush *BrushInfo `json:"brush,omitempty"`
	Image *ImageInfo `json:"image,omitempty"`
	Font  *FontInfo  `json:"font,omitempty"`
}

// PathInfo is a stable summary of a referenced path. It deliberately omits
// coordinates: Bounds plus verb counts identify a path change between
// backends without flooding the trace.
type PathInfo struct {
	// Ref is the resource pool handle. Synthetic paths built during replay
	// (rounded-rect clips) carry -1 because they are not pooled.
	Ref int `json:"ref"`

	// Verbs is the total number of path verbs.
	Verbs int `json:"verbs"`

	// VerbKinds counts verbs by kind ("move", "line", "quad", "cubic",
	// "close"). Absent kinds are omitted by the JSON encoder.
	VerbKinds map[string]int `json:"verbKinds,omitempty"`

	// Bounds is the exact floating-point bounding box computed from the
	// path coordinates.
	Bounds RectInfo `json:"bounds"`

	// Empty reports whether the path has no verbs.
	Empty bool `json:"empty"`
}

// BrushInfo is a stable summary of a referenced brush.
type BrushInfo struct {
	// Ref is the resource pool handle.
	Ref int `json:"ref"`

	// Kind is "solid", "linearGradient", "radialGradient", "sweepGradient",
	// "pattern" or "unknown".
	Kind string `json:"kind"`

	// Color is the solid color as #RRGGBBAA, present for solid brushes.
	Color string `json:"color,omitempty"`

	// Stops summarizes gradient color stops (offset plus #RRGGBBAA color).
	Stops []StopInfo `json:"stops,omitempty"`

	// Fields holds kind-specific scalar parameters (gradient endpoints,
	// radii, angles, extend/repeat modes). Keys sort alphabetically in JSON.
	Fields map[string]float64 `json:"fields,omitempty"`

	// Pattern summarizes the pattern image for PatternBrush.
	Pattern *ImageInfo `json:"pattern,omitempty"`
}

// StopInfo is one gradient color stop summary.
type StopInfo struct {
	Offset float64 `json:"offset"`
	Color  string  `json:"color"`
}

// ImageInfo is a stable summary of a referenced image. Pixel contents are
// never included.
type ImageInfo struct {
	// Ref is the resource pool handle, or -1 for images referenced
	// indirectly (pattern brushes).
	Ref int `json:"ref"`

	// Kind is the concrete image type name, or "nil" for a nil image.
	Kind string `json:"kind"`

	// Width and Height come from the image bounds.
	Width  int `json:"width"`
	Height int `json:"height"`

	// Bytes estimates the in-memory pixel footprint from bounds and pixel
	// stride without reading or copying pixels. It is a size hint, not an
	// exact allocation count.
	Bytes int `json:"bytes"`
}

// FontInfo is a stable summary of a text.Face. Font tables and glyph data
// are never included.
type FontInfo struct {
	// Family is the font's family/name when discoverable, otherwise "".
	// It is length-capped like other data-borne strings.
	Family string `json:"family,omitempty"`

	// Size is the face size in points.
	Size float64 `json:"size"`

	// Language is the BCP 47 language tag configured on the face.
	Language string `json:"language,omitempty"`

	// Features is the number of configured OpenType features.
	Features int `json:"features"`

	// Variations is the number of configured font variation axes.
	Variations int `json:"variations"`

	// Nil reports that no font face was attached to the command.
	Nil bool `json:"nil"`
}

// MatrixInfo is the JSON-friendly form of the six affine matrix entries.
type MatrixInfo struct {
	A float64 `json:"a"`
	B float64 `json:"b"`
	C float64 `json:"c"`
	D float64 `json:"d"`
	E float64 `json:"e"`
	F float64 `json:"f"`
}

// ClipInfo describes the reconstructed clip state at a command.
type ClipInfo struct {
	// Depth is the number of intersecting clip regions currently active.
	Depth int `json:"depth"`

	// Last summarizes the most recently installed clip path, or nil when no
	// clip is active.
	Last *PathInfo `json:"last,omitempty"`
}

// RectInfo is the JSON-friendly form of a Rect.
type RectInfo struct {
	MinX float64 `json:"minX"`
	MinY float64 `json:"minY"`
	MaxX float64 `json:"maxX"`
	MaxY float64 `json:"maxY"`
}

// BackendResult reports what the backend did with one event.
type BackendResult struct {
	// Status is "ok" or "error".
	Status string `json:"status"`

	// Calls is the number of Backend interface methods invoked while
	// dispatching this event (lifecycle events report 1 for Begin/End).
	Calls int `json:"calls"`

	// Error carries the failure context when Status is "error".
	Error *ErrorInfo `json:"error,omitempty"`
}

// ErrorInfo is a JSON-friendly error description.
type ErrorInfo struct {
	// Message is the error text, truncated to MaxStringLength with an
	// ellipsis when longer (error messages are diagnostics, not command
	// data, so they are capped rather than rejected).
	Message string `json:"message"`

	// Truncated reports that Message was shortened.
	Truncated bool `json:"truncated,omitempty"`
}

// goroutineTraceState is the per-goroutine re-entrancy state. It is reached
// through a pointer (a comparable value) so sync.Map CAS stays valid, and
// carries its own mutex because one goroutine can run nested traces of
// different Recordings from its callbacks.
type goroutineTraceState struct {
	mu         sync.Mutex
	recordings map[uintptr]struct{}
}

// activeTraces maps a goroutine ID to its trace state. Re-entrancy is a
// same-goroutine property (an OnEvent callback starting a nested trace);
// independent goroutines may trace the same immutable Recording
// concurrently, matching Recording's documented Playback contract.
var activeTraces sync.Map // map[int64]*goroutineTraceState

// goroutineID extracts the current goroutine ID from its runtime header.
// It parses the first line of runtime.Stack ("goroutine 18 [running]:")
// without depending on internal linkname-stable symbols.
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	line := string(buf[:n])
	const prefix = "goroutine "
	if len(line) <= len(prefix) {
		return -1
	}
	var id int64
	for i := len(prefix); i < len(line); i++ {
		c := line[i]
		if c < '0' || c > '9' {
			break
		}
		id = id*10 + int64(c-'0')
	}
	return id
}

// goroutineTrace returns the lazily-created trace state for this goroutine.
func goroutineTrace() *goroutineTraceState {
	gid := goroutineID()
	if v, ok := activeTraces.Load(gid); ok {
		return v.(*goroutineTraceState)
	}
	state := &goroutineTraceState{recordings: map[uintptr]struct{}{}}
	actual, _ := activeTraces.LoadOrStore(gid, state)
	return actual.(*goroutineTraceState)
}

// enterTrace marks this Recording as actively traced on the current
// goroutine. It returns ErrTraceReentrant when a trace of the same Recording
// is already running here.
func (r *Recording) enterTrace() error {
	key := reflect.ValueOf(r).Pointer()
	state := goroutineTrace()
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, busy := state.recordings[key]; busy {
		return ErrTraceReentrant
	}
	state.recordings[key] = struct{}{}
	return nil
}

// exitTrace removes the Recording marker for the current goroutine.
func (r *Recording) exitTrace() {
	key := reflect.ValueOf(r).Pointer()
	state := goroutineTrace()
	state.mu.Lock()
	defer state.mu.Unlock()
	delete(state.recordings, key)
}

// TracePlayback replays the recording to backend while emitting a structured
// trace. It drives the same dispatch path as Playback, so the backend call
// sequence is identical; tracing never alters replay behavior.
//
// Exactly one sink (opts.Writer or opts.OnEvent) must be configured. The
// returned error is also recorded on the failing event so failed runs keep
// the full context of the command that failed.
func (r *Recording) TracePlayback(backend Backend, opts TraceOptions) error {
	if backend == nil {
		return ErrNilBackend
	}
	maxLen, err := opts.resolve()
	if err != nil {
		return err
	}

	if err := r.enterTrace(); err != nil {
		return err
	}
	defer r.exitTrace()

	tracer := &tracer{
		rec:        r,
		opts:       opts,
		maxStrLen:  maxLen,
		engine:     newPlaybackEngine(backend, r.width, r.height),
		jsonWriter: json.NewEncoder(opts.Writer),
	}
	return tracer.run()
}

func (o TraceOptions) resolve() (int, error) {
	if o.Writer == nil && o.OnEvent == nil {
		return 0, ErrTraceNoSink
	}
	if o.Writer != nil && o.OnEvent != nil {
		return 0, ErrTraceBothSinks
	}
	maxLen := o.MaxStringLength
	if maxLen == 0 {
		maxLen = DefaultMaxStringLength
	}
	if maxLen < 0 {
		return 0, fmt.Errorf("recording: MaxStringLength must be non-negative, got %d", maxLen)
	}
	if maxLen > hardMaxStringLength {
		return 0, fmt.Errorf("recording: MaxStringLength %d exceeds hard limit %d", maxLen, hardMaxStringLength)
	}
	return maxLen, nil
}

// TraceEvents replays the recording to backend and collects the trace events
// in replay order. It is a convenience wrapper over TracePlayback for tests
// and quality gates that compare two backends event-by-event. Functional
// options may adjust MaxStringLength or FailAfter.
func (r *Recording) TraceEvents(backend Backend, opts ...func(*TraceOptions)) ([]TraceEvent, error) {
	options := TraceOptions{}
	for _, adjust := range opts {
		adjust(&options)
	}
	var events []TraceEvent
	options.OnEvent = func(e TraceEvent) {
		events = append(events, e)
	}
	options.Writer = nil
	if err := r.TracePlayback(backend, options); err != nil {
		return events, err
	}
	return events, nil
}

// tracer holds the per-run state for one traced playback.
type tracer struct {
	rec        *Recording
	opts       TraceOptions
	maxStrLen  int
	engine     *playbackEngine
	jsonWriter *json.Encoder
}

func (t *tracer) run() error {
	backend := t.engine.backend

	// Lifecycle: Begin.
	beginErr := backend.Begin(t.engine.width, t.engine.height)
	if beginErr != nil {
		// Even a failed Begin gets an event so consumers see one record per
		// attempted phase, preserving the trace's stable skeleton.
		ev := t.lifecycleEvent("begin", 1, beginErr)
		if err := t.emit(ev); err != nil {
			return err
		}
		return fmt.Errorf("recording: trace playback Begin failed at lifecycle: %w", beginErr)
	}
	if err := t.emit(t.lifecycleEvent("begin", 1, nil)); err != nil {
		return err
	}

	for i, cmd := range t.rec.commands {
		// Phase 1: validate and describe the command against its recorded
		// values. Any failure here happens before the backend is touched.
		params, resources, buildErr := t.describeCommand(cmd)
		if buildErr != nil {
			// A malformed recorded value (non-finite number, overlong string)
			// prevents a faithful trace. Emit nothing fabricated; report with
			// the command context. The backend has not been called for this
			// command, so normal replay semantics are not altered.
			return fmt.Errorf("recording: trace build failed at command %d (%s): %w",
				i, cmd.Type().String(), buildErr)
		}

		// Failure injection is a pre-dispatch stop: the backend is not
		// called, so the event describes the state on entry to the command.
		if t.opts.FailAfter != nil {
			if injected := t.opts.FailAfter(i, cmd); injected != nil {
				ev := t.assembleEvent(i, cmd, params, resources)
				ev.Backend = BackendResult{
					Status: "error",
					Calls:  0,
					Error:  t.errorInfo(injected),
				}
				if err := t.emit(ev); err != nil {
					return err
				}
				// Mirror Playback semantics: a failed command aborts the run
				// and End() is never reached. No "end" event is emitted, so
				// the stream's tail itself signals where replay stopped.
				return injected
			}
		}

		// Phase 2: run the exact shared dispatch, then assemble the event
		// from the engine's post-dispatch state so transform/clip reflect
		// what the command established.
		calls := t.rec.dispatchCounted(t.engine, cmd)
		ev := t.assembleEvent(i, cmd, params, resources)
		ev.Backend = BackendResult{Status: "ok", Calls: calls}
		if err := t.emit(ev); err != nil {
			return err
		}
	}

	endErr := backend.End()
	endStatus := BackendResult{Status: "ok", Calls: 1}
	if endErr != nil {
		endStatus = BackendResult{
			Status: "error",
			Calls:  1,
			Error:  t.errorInfo(endErr),
		}
	}
	endEv := t.lifecycleEvent("end", 1, nil)
	endEv.Backend = endStatus
	if err := t.emit(endEv); err != nil {
		return err
	}
	if endErr != nil {
		return fmt.Errorf("recording: trace playback End failed: %w", endErr)
	}
	return nil
}

// lifecycleEvent builds a "begin"/"end" skeleton event. Index -1 marks
// non-command events; the current transform/clip at the boundaries are
// identity/empty for begin and the final state for end.
func (t *tracer) lifecycleEvent(phase string, calls int, err error) TraceEvent {
	ev := TraceEvent{
		Index:     -1,
		Phase:     phase,
		Transform: matrixInfo(t.engine.transform),
	}
	clip, _ := t.clipInfoEvent()
	ev.Clip = clip
	if err != nil {
		ev.Backend = BackendResult{Status: "error", Calls: calls, Error: t.errorInfo(err)}
	} else {
		ev.Backend = BackendResult{Status: "ok", Calls: calls}
	}
	return ev
}

// assembleEvent builds a command event from pre-validated parameters and the
// engine's current state. It is called either after a successful dispatch
// (state reflects what the command established) or before an injected
// failure (state reflects the entry condition). The clip error is dropped
// because every path that can reach clip state was already validated for
// finiteness inside describeCommand before dispatch.
func (t *tracer) assembleEvent(index int, cmd Command, params map[string]any, resources *EventResources) TraceEvent {
	clip, _ := t.clipInfoEvent()
	return TraceEvent{
		Index:     index,
		Phase:     "command",
		Type:      cmd.Type().String(),
		Params:    params,
		Resources: resources,
		Transform: matrixInfo(t.engine.transform),
		Clip:      clip,
	}
}

func (t *tracer) emit(ev TraceEvent) error {
	if t.opts.OnEvent != nil {
		t.opts.OnEvent(ev)
		return nil
	}
	if err := t.jsonWriter.Encode(ev); err != nil {
		return fmt.Errorf("recording: trace write failed: %w", err)
	}
	return nil
}

// dispatchCounted runs the shared dispatch and reports how many Backend
// interface calls it made. The call counts are derived from the command
// type's known dispatch shape, which is the same switch Playback uses.
func (r *Recording) dispatchCounted(engine *playbackEngine, cmd Command) int {
	calls := backendCallCount(cmd)
	r.dispatch(engine, cmd)
	return calls
}

// backendCallCount returns the number of Backend interface method calls the
// dispatch of cmd performs. It stays in lockstep with Recording.dispatch.
func backendCallCount(cmd Command) int {
	switch cmd.(type) {
	case SaveCommand, RestoreCommand, SetTransformCommand, ClearClipCommand:
		return 1
	case SetClipCommand, ClipRoundRectCommand:
		// SetTransform(identity) + SetClip + SetTransform(restore)
		return 3
	case FillPathCommand, StrokePathCommand, FillRectCommand,
		DrawImageCommand, DrawTextCommand, StrokeTextCommand:
		return 1
	case StrokeRectCommand:
		// Save + SetTransform + StrokePath + Restore
		return 4
	default:
		// Style commands reach no backend method.
		return 0
	}
}

// --- command description ---------------------------------------------------

func (t *tracer) describeCommand(cmd Command) (map[string]any, *EventResources, error) {
	switch c := cmd.(type) {
	case SaveCommand, RestoreCommand, ClearClipCommand:
		return nil, nil, nil
	case SetTransformCommand:
		return t.describeSetTransform(c)
	case SetClipCommand:
		return t.describeSetClip(c)
	case ClipRoundRectCommand:
		return t.describeClipRoundRect(c)
	case FillPathCommand:
		return t.describeFillPath(c)
	case StrokePathCommand:
		return t.describeStrokePath(c)
	case FillRectCommand:
		return t.describeFillRect(c)
	case StrokeRectCommand:
		return t.describeStrokeRect(c)
	case DrawImageCommand:
		return t.describeDrawImage(c)
	case DrawTextCommand:
		return t.textParams(c.Text, c.X, c.Y, c.FontSize, c.FontFamily, c.Face, c.Brush, nil)
	case StrokeTextCommand:
		return t.describeStrokeText(c)
	case SetFillStyleCommand, SetStrokeStyleCommand:
		return t.describeStyleCommand(c)
	case SetLineWidthCommand, SetLineCapCommand, SetLineJoinCommand,
		SetMiterLimitCommand, SetDashCommand, SetFillRuleCommand, SetAntiAliasCommand:
		return describeStyleScalar(cmd)
	default:
		return map[string]any{"unknown": cmd.Type().String()}, nil, nil
	}
}

func (t *tracer) describeSetTransform(c SetTransformCommand) (map[string]any, *EventResources, error) {
	if err := checkMatrix(c.Matrix); err != nil {
		return nil, nil, err
	}
	return map[string]any{"matrix": matrixInfo(c.Matrix)}, nil, nil
}

func (t *tracer) describeSetClip(c SetClipCommand) (map[string]any, *EventResources, error) {
	path := t.rec.resources.GetPath(c.Path)
	info, err := summarizePath(path, c.Path)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{
		"rule": fillRuleName(c.Rule),
		"ref":  uint32(c.Path),
	}, &EventResources{Path: info}, nil
}

func (t *tracer) describeClipRoundRect(c ClipRoundRectCommand) (map[string]any, *EventResources, error) {
	// The replay builds the world-space path; summarize the same path so
	// the trace reflects what the backend actually receives.
	worldPath := buildClipRoundRectPath(c, t.engine.transform)
	info, err := summarizePath(worldPath, PathRef(InvalidRef))
	if err != nil {
		return nil, nil, err
	}
	params, err := t.rectParams("x", "y", "w", "h", c.X, c.Y, c.W, c.H)
	if err != nil {
		return nil, nil, err
	}
	if err := checkFinite("radius", c.Radius); err != nil {
		return nil, nil, err
	}
	params["radius"] = c.Radius
	return params, &EventResources{Path: info}, nil
}

func (t *tracer) describeFillPath(c FillPathCommand) (map[string]any, *EventResources, error) {
	pathInfo, brush, err := t.pathAndBrush(c.Path, c.Brush)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{
		"rule":  fillRuleName(c.Rule),
		"path":  uint32(c.Path),
		"brush": uint32(c.Brush),
	}, &EventResources{Path: pathInfo, Brush: brush}, nil
}

func (t *tracer) describeStrokePath(c StrokePathCommand) (map[string]any, *EventResources, error) {
	pathInfo, brush, err := t.pathAndBrush(c.Path, c.Brush)
	if err != nil {
		return nil, nil, err
	}
	params, err := t.strokeParams(c.Stroke)
	if err != nil {
		return nil, nil, err
	}
	params["path"] = uint32(c.Path)
	params["brush"] = uint32(c.Brush)
	return params, &EventResources{Path: pathInfo, Brush: brush}, nil
}

func (t *tracer) describeFillRect(c FillRectCommand) (map[string]any, *EventResources, error) {
	brush, err := t.summarizeBrush(c.Brush)
	if err != nil {
		return nil, nil, err
	}
	params, err := rectParams(c.Rect)
	if err != nil {
		return nil, nil, err
	}
	params["brush"] = uint32(c.Brush)
	return params, &EventResources{Brush: brush}, nil
}

func (t *tracer) describeStrokeRect(c StrokeRectCommand) (map[string]any, *EventResources, error) {
	brush, err := t.summarizeBrush(c.Brush)
	if err != nil {
		return nil, nil, err
	}
	params, err := rectParams(c.Rect)
	if err != nil {
		return nil, nil, err
	}
	strokeParams, err := t.strokeParams(c.Stroke)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range strokeParams {
		params[k] = v
	}
	params["brush"] = uint32(c.Brush)
	return params, &EventResources{Brush: brush}, nil
}

func (t *tracer) describeDrawImage(c DrawImageCommand) (map[string]any, *EventResources, error) {
	img := t.rec.resources.GetImage(c.Image)
	params, err := rectParamsNamed("src", c.SrcRect)
	if err != nil {
		return nil, nil, err
	}
	dst, err := rectParamsNamed("dst", c.DstRect)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range dst {
		params[k] = v
	}
	if err := checkFinite("imageAlpha", c.Options.Alpha); err != nil {
		return nil, nil, err
	}
	params["image"] = uint32(c.Image)
	params["interpolation"] = interpolationName(c.Options.Interpolation)
	params["alpha"] = c.Options.Alpha
	return params, &EventResources{Image: summarizeImage(img, c.Image)}, nil
}

func (t *tracer) describeStrokeText(c StrokeTextCommand) (map[string]any, *EventResources, error) {
	strokeParams, err := t.strokeParams(c.Stroke)
	if err != nil {
		return nil, nil, err
	}
	return t.textParams(c.Text, c.X, c.Y, c.FontSize, c.FontFamily, c.Face, c.Brush, strokeParams)
}

func (t *tracer) describeStyleCommand(cmd Command) (map[string]any, *EventResources, error) {
	// Both style commands expose a Brush field; extract via type switch to
	// avoid an exported helper solely for tracing.
	var brushRef BrushRef
	switch c := cmd.(type) {
	case SetFillStyleCommand:
		brushRef = c.Brush
	case SetStrokeStyleCommand:
		brushRef = c.Brush
	}
	brush, err := t.summarizeBrush(brushRef)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"brush": uint32(brushRef)}, &EventResources{Brush: brush}, nil
}

// pathAndBrush resolves and summarizes the two pooled resources shared by
// the fill/stroke path commands.
func (t *tracer) pathAndBrush(pathRef PathRef, brushRef BrushRef) (*PathInfo, *BrushInfo, error) {
	pathInfo, err := summarizePath(t.rec.resources.GetPath(pathRef), pathRef)
	if err != nil {
		return nil, nil, err
	}
	brush, err := t.summarizeBrush(brushRef)
	if err != nil {
		return nil, nil, err
	}
	return pathInfo, brush, nil
}

// describeStyleScalar handles the parameter-only style commands that carry
// no pooled resources.
func describeStyleScalar(cmd Command) (map[string]any, *EventResources, error) {
	switch c := cmd.(type) {
	case SetLineWidthCommand:
		if err := checkFinite("width", c.Width); err != nil {
			return nil, nil, err
		}
		return map[string]any{"width": c.Width}, nil, nil
	case SetLineCapCommand:
		return map[string]any{"cap": lineCapName(c.Cap)}, nil, nil
	case SetLineJoinCommand:
		return map[string]any{"join": lineJoinName(c.Join)}, nil, nil
	case SetMiterLimitCommand:
		if err := checkFinite("limit", c.Limit); err != nil {
			return nil, nil, err
		}
		return map[string]any{"limit": c.Limit}, nil, nil
	case SetDashCommand:
		return describeDash(c)
	case SetFillRuleCommand:
		return map[string]any{"rule": fillRuleName(c.Rule)}, nil, nil
	case SetAntiAliasCommand:
		return map[string]any{"enabled": c.Enabled}, nil, nil
	default:
		return map[string]any{"unknown": cmd.Type().String()}, nil, nil
	}
}

func describeDash(c SetDashCommand) (map[string]any, *EventResources, error) {
	for i, v := range c.Pattern {
		if err := checkFinite(fmt.Sprintf("pattern[%d]", i), v); err != nil {
			return nil, nil, err
		}
	}
	if err := checkFinite("offset", c.Offset); err != nil {
		return nil, nil, err
	}
	return map[string]any{
		"pattern": c.Pattern,
		"offset":  c.Offset,
	}, nil, nil
}

// textParams builds the shared parameter/resource description for both text
// command variants. extra (non-nil for StrokeText) contributes stroke fields.
func (t *tracer) textParams(s string, x, y, fontSize float64, family string,
	face text.Face, brushRef BrushRef, extra map[string]any,
) (map[string]any, *EventResources, error) {
	if err := t.checkDataString("text", s); err != nil {
		return nil, nil, err
	}
	if err := t.checkDataString("fontFamily", family); err != nil {
		return nil, nil, err
	}
	for field, v := range map[string]float64{"x": x, "y": y, "fontSize": fontSize} {
		if err := checkFinite(field, v); err != nil {
			return nil, nil, err
		}
	}
	brush, err := t.summarizeBrush(brushRef)
	if err != nil {
		return nil, nil, err
	}
	params := map[string]any{
		"text":       s,
		"x":          x,
		"y":          y,
		"fontSize":   fontSize,
		"fontFamily": family,
		"brush":      uint32(brushRef),
	}
	for k, v := range extra {
		params[k] = v
	}
	return params, &EventResources{
		Brush: brush,
		Font:  summarizeFont(face, t.maxStrLen),
	}, nil
}

// --- summaries --------------------------------------------------------------

// summarizePath builds a PathInfo without exposing coordinates. All bounds
// values are validated as finite so the JSONL stream can never contain
// NaN/Inf tokens.
func summarizePath(path *gg.Path, ref PathRef) (*PathInfo, error) {
	info := &PathInfo{Ref: int(ref)}
	if path == nil {
		info.Empty = true
		return info, nil
	}
	verbs := path.Verbs()
	coords := path.Coords()
	info.Verbs = len(verbs)
	info.Empty = info.Verbs == 0
	kinds := map[string]int{}
	hasMin := false
	var minX, minY, maxX, maxY float64
	coordIdx := 0
	for _, v := range verbs {
		var n int
		var kind string
		switch v {
		case gg.MoveTo:
			n, kind = 1, "move"
		case gg.LineTo:
			n, kind = 1, "line"
		case gg.QuadTo:
			n, kind = 2, "quad"
		case gg.CubicTo:
			n, kind = 3, "cubic"
		case gg.Close:
			kind = "close"
		default:
			kind = "unknown"
		}
		kinds[kind]++
		for p := 0; p < n; p++ {
			x, y := coords[coordIdx*2], coords[coordIdx*2+1]
			coordIdx++
			if err := checkFinite("pathCoord.x", x); err != nil {
				return nil, err
			}
			if err := checkFinite("pathCoord.y", y); err != nil {
				return nil, err
			}
			if !hasMin {
				minX, maxX, minY, maxY = x, x, y, y
				hasMin = true
				continue
			}
			minX = math.Min(minX, x)
			maxX = math.Max(maxX, x)
			minY = math.Min(minY, y)
			maxY = math.Max(maxY, y)
		}
	}
	if len(kinds) > 0 {
		info.VerbKinds = kinds
	}
	if hasMin {
		info.Bounds = RectInfo{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
	}
	return info, nil
}

func (t *tracer) summarizeBrush(ref BrushRef) (*BrushInfo, error) {
	brush := t.rec.resources.GetBrush(ref)
	info := &BrushInfo{Ref: int(ref)}
	if brush == nil {
		info.Kind = "nil"
		return info, nil
	}
	switch b := brush.(type) {
	case SolidBrush:
		if err := checkRGBA(b.Color); err != nil {
			return nil, err
		}
		info.Kind = "solid"
		info.Color = rgbaHex(b.Color)
	case *LinearGradientBrush:
		return t.linearBrushInfo(info, b)
	case *RadialGradientBrush:
		return t.radialBrushInfo(info, b)
	case *SweepGradientBrush:
		return t.sweepBrushInfo(info, b)
	case *PatternBrush:
		return t.patternBrushInfo(info, b)
	default:
		info.Kind = "unknown"
	}
	return info, nil
}

func (t *tracer) linearBrushInfo(info *BrushInfo, b *LinearGradientBrush) (*BrushInfo, error) {
	info.Kind = "linearGradient"
	fields, stops, err := gradientSummary(b.Stops)
	if err != nil {
		return nil, err
	}
	fields["startX"] = b.Start.X
	fields["startY"] = b.Start.Y
	fields["endX"] = b.End.X
	fields["endY"] = b.End.Y
	fields["extend"] = float64(b.Extend)
	if err := checkAllFinite(fields); err != nil {
		return nil, err
	}
	info.Fields, info.Stops = fields, stops
	return info, nil
}

func (t *tracer) radialBrushInfo(info *BrushInfo, b *RadialGradientBrush) (*BrushInfo, error) {
	info.Kind = "radialGradient"
	fields, stops, err := gradientSummary(b.Stops)
	if err != nil {
		return nil, err
	}
	fields["centerX"] = b.Center.X
	fields["centerY"] = b.Center.Y
	fields["focusX"] = b.Focus.X
	fields["focusY"] = b.Focus.Y
	fields["startRadius"] = b.StartRadius
	fields["endRadius"] = b.EndRadius
	fields["extend"] = float64(b.Extend)
	if err := checkAllFinite(fields); err != nil {
		return nil, err
	}
	info.Fields, info.Stops = fields, stops
	return info, nil
}

func (t *tracer) sweepBrushInfo(info *BrushInfo, b *SweepGradientBrush) (*BrushInfo, error) {
	info.Kind = "sweepGradient"
	fields, stops, err := gradientSummary(b.Stops)
	if err != nil {
		return nil, err
	}
	fields["centerX"] = b.Center.X
	fields["centerY"] = b.Center.Y
	fields["startAngle"] = b.StartAngle
	fields["endAngle"] = b.EndAngle
	fields["extend"] = float64(b.Extend)
	if err := checkAllFinite(fields); err != nil {
		return nil, err
	}
	info.Fields, info.Stops = fields, stops
	return info, nil
}

func (t *tracer) patternBrushInfo(info *BrushInfo, b *PatternBrush) (*BrushInfo, error) {
	info.Kind = "pattern"
	info.Fields = map[string]float64{
		"repeat":  float64(b.Repeat),
		"matrixA": b.Transform.A,
		"matrixB": b.Transform.B,
		"matrixC": b.Transform.C,
		"matrixD": b.Transform.D,
		"matrixE": b.Transform.E,
		"matrixF": b.Transform.F,
	}
	if err := checkAllFinite(info.Fields); err != nil {
		return nil, err
	}
	img := t.rec.resources.GetImage(b.Image)
	info.Pattern = summarizeImage(img, b.Image)
	return info, nil
}

func gradientSummary(stops []GradientStop) (map[string]float64, []StopInfo, error) {
	out := make([]StopInfo, 0, len(stops))
	for i, s := range stops {
		if err := checkFinite(fmt.Sprintf("stops[%d].offset", i), s.Offset); err != nil {
			return nil, nil, err
		}
		if err := checkRGBA(s.Color); err != nil {
			return nil, nil, err
		}
		out = append(out, StopInfo{Offset: s.Offset, Color: rgbaHex(s.Color)})
	}
	return map[string]float64{}, out, nil
}

// summarizeImage reports dimensions, concrete type and an estimated byte
// footprint. It never reads pixels, so a large image costs O(1) trace work.
func summarizeImage(img image.Image, ref ImageRef) *ImageInfo {
	info := &ImageInfo{Ref: int(ref)}
	if img == nil {
		info.Kind = "nil"
		return info
	}
	info.Kind = reflect.TypeOf(img).String()
	bounds := img.Bounds()
	info.Width = bounds.Dx()
	info.Height = bounds.Dy()
	info.Bytes = imageByteEstimate(img, bounds)
	return info
}

// imageByteEstimate derives a pixel-buffer size hint from the image's
// concrete pixel type without touching pixel data.
func imageByteEstimate(img image.Image, bounds image.Rectangle) int {
	switch img.(type) {
	case *image.RGBA:
		return bounds.Dx() * bounds.Dy() * 4
	case *image.RGBA64:
		return bounds.Dx() * bounds.Dy() * 8
	case *image.NRGBA:
		return bounds.Dx() * bounds.Dy() * 4
	case *image.NRGBA64:
		return bounds.Dx() * bounds.Dy() * 8
	case *image.Gray:
		return bounds.Dx() * bounds.Dy()
	case *image.Gray16:
		return bounds.Dx() * bounds.Dy() * 2
	case *image.Alpha:
		return bounds.Dx() * bounds.Dy()
	case *image.Alpha16:
		return bounds.Dx() * bounds.Dy() * 2
	case *image.YCbCr:
		// 4:2:0 average 1.5 bytes/pixel; use a stable lower-bound estimate.
		return bounds.Dx() * bounds.Dy() * 3 / 2
	case *image.Paletted:
		return bounds.Dx() * bounds.Dy()
	default:
		// Unknown layout: report the sample count rather than guess stride.
		return bounds.Dx() * bounds.Dy()
	}
}

// summarizeFont reports face metadata only — never font table bytes or
// glyph outlines.
func summarizeFont(face text.Face, maxLen int) *FontInfo {
	info := &FontInfo{}
	if face == nil {
		info.Nil = true
		return info
	}
	info.Size = face.Size()
	info.Language = face.Language()
	info.Features = len(face.Features())
	info.Variations = len(face.Variations())
	if src := face.Source(); src != nil {
		info.Family = capString(src.Name(), maxLen)
	}
	return info
}

// --- validation helpers ------------------------------------------------------

// checkFinite returns an ErrNonFinite wrapper naming the offending field.
func checkFinite(field string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%w: %s=%v", ErrNonFinite, field, v)
	}
	return nil
}

func checkAllFinite(fields map[string]float64) error {
	// Sorted field names keep the error message stable.
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sortStrings(names)
	for _, k := range names {
		if err := checkFinite(k, fields[k]); err != nil {
			return err
		}
	}
	return nil
}

func checkMatrix(m Matrix) error {
	values := map[string]float64{
		"a": m.A, "b": m.B, "c": m.C,
		"d": m.D, "e": m.E, "f": m.F,
	}
	return checkAllFinite(values)
}

func checkRGBA(c gg.RGBA) error {
	return checkAllFinite(map[string]float64{
		"r": c.R, "g": c.G, "b": c.B, "a": c.A,
	})
}

// checkDataString enforces the per-string length gate on command data. The
// check happens before the backend is called, so an overlong string fails
// loudly instead of silently truncating recorded semantics.
func (t *tracer) checkDataString(field, s string) error {
	if len(s) > t.maxStrLen {
		return fmt.Errorf("%w: field %s length=%d limit=%d",
			ErrStringTooLong, field, len(s), t.maxStrLen)
	}
	return nil
}

// capString shortens diagnostic strings (font family names) to the cap.
// Unlike command data strings these cannot fail the run: losing a family
// suffix never changes rendering, and the summary contract explicitly
// permits shortened names.
func capString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func (t *tracer) errorInfo(err error) *ErrorInfo {
	msg := err.Error()
	info := &ErrorInfo{}
	if len(msg) > t.maxStrLen {
		info.Message = msg[:t.maxStrLen-3] + "..."
		info.Truncated = true
	} else {
		info.Message = msg
	}
	return info
}

// --- small formatting helpers ------------------------------------------------

func matrixInfo(m Matrix) MatrixInfo {
	return MatrixInfo(m)
}

func (t *tracer) clipInfoEvent() (ClipInfo, error) {
	info := ClipInfo{Depth: t.engine.clipDepth}
	if t.engine.clipDepth > 0 && t.engine.lastClipPath != nil {
		pathInfo, err := summarizePath(t.engine.lastClipPath, t.engine.lastClipRef)
		if err != nil {
			return ClipInfo{}, err
		}
		info.Last = pathInfo
	}
	return info, nil
}

func rectParams(r Rect) (map[string]any, error) {
	if err := checkAllFinite(map[string]float64{
		"minX": r.MinX, "minY": r.MinY, "maxX": r.MaxX, "maxY": r.MaxY,
	}); err != nil {
		return nil, err
	}
	return map[string]any{"rect": RectInfo(r)}, nil
}

func rectParamsNamed(prefix string, r Rect) (map[string]any, error) {
	values := map[string]float64{
		prefix + "MinX": r.MinX,
		prefix + "MinY": r.MinY,
		prefix + "MaxX": r.MaxX,
		prefix + "MaxY": r.MaxY,
	}
	if err := checkAllFinite(values); err != nil {
		return nil, err
	}
	return map[string]any{
		prefix: RectInfo(r),
	}, nil
}

func (t *tracer) rectParams(nx, ny, nw, nh string, x, y, w, h float64) (map[string]any, error) {
	values := map[string]float64{nx: x, ny: y, nw: w, nh: h}
	if err := checkAllFinite(values); err != nil {
		return nil, err
	}
	return map[string]any{nx: x, ny: y, nw: w, nh: h}, nil
}

func (t *tracer) strokeParams(s Stroke) (map[string]any, error) {
	if err := checkFinite("strokeWidth", s.Width); err != nil {
		return nil, err
	}
	if err := checkFinite("miterLimit", s.MiterLimit); err != nil {
		return nil, err
	}
	if err := checkFinite("dashOffset", s.DashOffset); err != nil {
		return nil, err
	}
	for i, v := range s.DashPattern {
		if err := checkFinite(fmt.Sprintf("dash[%d]", i), v); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"strokeWidth": s.Width,
		"cap":         lineCapName(s.Cap),
		"join":        lineJoinName(s.Join),
		"miterLimit":  s.MiterLimit,
		"dash":        append([]float64(nil), s.DashPattern...),
		"dashOffset":  s.DashOffset,
	}, nil
}

// rgbaHex renders a color as stable uppercase #RRGGBBAA. Components are
// clamped to [0,1] before scaling; callers must have validated finiteness.
func rgbaHex(c gg.RGBA) string {
	return fmt.Sprintf("#%02X%02X%02X%02X",
		clampByte(c.R), clampByte(c.G), clampByte(c.B), clampByte(c.A))
}

func clampByte(v float64) int {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return 255
	}
	return int(math.Round(v * 255))
}

func fillRuleName(r FillRule) string {
	if r == FillRuleEvenOdd {
		return "evenOdd"
	}
	return "nonZero"
}

func lineCapName(c LineCap) string {
	switch c {
	case LineCapRound:
		return "round"
	case LineCapSquare:
		return "square"
	default:
		return "butt"
	}
}

func lineJoinName(j LineJoin) string {
	switch j {
	case LineJoinRound:
		return "round"
	case LineJoinBevel:
		return "bevel"
	default:
		return "miter"
	}
}

func interpolationName(m InterpolationMode) string {
	if m == InterpolationNearest {
		return "nearest"
	}
	return "bilinear"
}

func sortStrings(s []string) {
	// Byte-wise ordering matches encoding/json's map-key sorting, keeping
	// error and output ordering consistent.
	sort.Strings(s)
}
