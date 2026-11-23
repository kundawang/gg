package scene_test

// Test-only state-balance quality gate for scene command streams.
//
// Scene state has stack discipline: every Save needs a Restore, every
// BeginClip an EndClip, every PushLayer a PopLayer. The production renderers
// keep independent stacks:
//
//   - scene.Renderer (CPU tile renderer): TagBeginClip/TagEndClip maintain a
//     per-tile clip stack; TagTransform replaces the current transform.
//   - scene.GPUSceneRenderer: BeginClip maps to dc.Push, EndClip to dc.Pop,
//     TagTransform to dc.SetTransform, PushLayer/PopLayer to a layer stack.
//
// If a frame leaves any of those open the next frame inherits a stale clip,
// transform, or layer. gateRecorder is a TEST-ONLY renderer surface that
// records every state push/pop, clip and transform change, then reports the
// first unpaired command and a state summary at frame end.
//
// The gate never rasterizes, never creates a GPU handle, and lives entirely in
// _test.go files. Production rendering code is not modified, so the gate
// cannot change rendering results.
//
// Diagnostic categories:
//
//	balanced             all pushes paired, LIFO, no nesting deeper than one
//	legal-nested         balanced with at least one stack nested two or deeper
//	empty-stack-restore  Restore/EndClip/PopLayer/ClearClip with no peer
//	duplicate-clear-clip two ClearClip calls with no clip in between
//	unpaired-push        Save/BeginClip/PushLayer still open at frame end
//	cross-layer-leak     state opened in one layer is closed (or abandoned)
//	                     in another layer
//	cross-frame-leak     a frame starts with stacks already dirty
//	backend-error        the backend failed mid-frame (reported separately)
//
// The fault doubles in state_gate_doubles_test.go prove the gate fails when a
// Restore is dropped, ClearClip is duplicated, or the backend aborts midway.

import (
	"fmt"
	"strings"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/scene"
)

// gateCategory classifies the most severe problem in one frame.
type gateCategory string

const (
	gateBalanced       gateCategory = "balanced"
	gateLegalNested    gateCategory = "legal-nested"
	gateEmptyStack     gateCategory = "empty-stack-restore"
	gateDuplicateClear gateCategory = "duplicate-clear-clip"
	gateUnpairedPush   gateCategory = "unpaired-push"
	gateCrossLayerLeak gateCategory = "cross-layer-leak"
	gateCrossFrameLeak gateCategory = "cross-frame-leak"
	gateBackendError   gateCategory = "backend-error"
)

// ok reports whether the category represents a state-balanced frame.
func (c gateCategory) ok() bool {
	return c == gateBalanced || c == gateLegalNested
}

// gateEntry is one item on a state stack.
type gateEntry struct {
	Frame      int
	Seq        int
	Kind       string
	PathSig    string
	LayerDepth int

	// clipSnapshot is the clip stack captured by Save. Restore reinstates it,
	// matching gg.Context.Push/Pop (a saved clip survives ResetClip inside the
	// saved scope) and GPUSceneRenderer's Push/Pop around BeginClip.
	clipSnapshot []gateEntry
}

func (e gateEntry) describe() string {
	s := fmt.Sprintf("%s@#%d (frame %d, layer %d)", e.Kind, e.Seq, e.Frame, e.LayerDepth)
	if e.PathSig != "" {
		s += " path=" + e.PathSig
	}
	return s
}

// gateProblem is a diagnostic recorded while the frame is replayed.
type gateProblem struct {
	Kind   gateCategory
	Seq    int
	Detail string
}

// gateEvent is one recorded state transition.
type gateEvent struct {
	Seq        int
	Op         string
	Detail     string
	SaveDepth  int
	ClipDepth  int
	LayerDepth int
	Transform  scene.Affine
}

// gateReport is the frame-end diagnostic.
type gateReport struct {
	Label            string
	Frame            int
	Seed             int64
	Category         gateCategory
	FirstUnmatched   string
	SaveDepth        int
	ClipDepth        int
	LayerDepth       int
	MaxDepth         int
	Transform        scene.Affine
	TransformChanges int
	ActiveClips      []string
	BackendErrors    []string
	Carried          string
	Details          []string
	Events           []string
}

// ok reports whether the frame is free of state-balance problems.
func (r gateReport) ok() bool {
	return r.Category.ok() && len(r.BackendErrors) == 0
}

// summary renders the frame-end state snapshot.
func (r gateReport) summary() string {
	return fmt.Sprintf(
		"save=%d clip=%d layer=%d maxDepth=%d transform=%s transformChanges=%d activeClips=[%s]",
		r.SaveDepth, r.ClipDepth, r.LayerDepth, r.MaxDepth,
		formatGateAffine(r.Transform), r.TransformChanges, strings.Join(r.ActiveClips, ", "),
	)
}

// String renders the report for test failures and debug logs.
func (r gateReport) String() string {
	var b strings.Builder
	status := "PASS"
	if !r.ok() {
		status = "FAIL"
	}
	fmt.Fprintf(&b, "frame %q (#%d): %s (%s), seed=%d\n", r.Label, r.Frame, status, r.Category, r.Seed)
	if r.FirstUnmatched != "" {
		fmt.Fprintf(&b, "  first unmatched: %s\n", r.FirstUnmatched)
	}
	if r.Carried != "" {
		fmt.Fprintf(&b, "  carried state: %s\n", r.Carried)
	}
	fmt.Fprintf(&b, "  state at end: %s\n", r.summary())
	for _, d := range r.Details {
		fmt.Fprintf(&b, "  detail: %s\n", d)
	}
	for _, e := range r.BackendErrors {
		fmt.Fprintf(&b, "  backend error: %s\n", e)
	}
	if r.FirstUnmatched != "" || len(r.BackendErrors) > 0 {
		b.WriteString("  events:\n")
		for _, e := range r.Events {
			fmt.Fprintf(&b, "    %s\n", e)
		}
	}
	return b.String()
}

// gateSink is the stateful surface the gate replays into. The fault doubles
// in state_gate_doubles_test.go implement it to misbehave on purpose.
type gateSink interface {
	Save(cause string)
	Restore()
	BeginClip(pathSig string)
	EndClip()
	ClearClip()
	SetTransform(t scene.Affine)
	SetAntiAlias(enabled bool)
	BeginLayer(desc string)
	EndLayer()
	BackendError(msg string)
}

// Ensure the recorder itself satisfies the sink surface.
var _ gateSink = (*gateRecorder)(nil)

// gateRecorder is a test renderer that only tracks state transitions.
type gateRecorder struct {
	label string
	seed  int64

	frameLabel string
	frameNum   int
	frameSeq   int
	inFrame    bool

	saves  []gateEntry
	clips  []gateEntry
	layers []gateEntry

	transform        scene.Affine
	transformChanges int
	antiAlias        bool
	maxSave          int
	maxClip          int
	maxLayer         int
	lastClipOp       string

	events      []gateEvent
	problems    []gateProblem
	backendErrs []string
	details     []string

	carriedSaves  int
	carriedClips  int
	carriedLayers int

	frames []gateReport
}

// newGateRecorder creates a clean gate recorder.
func newGateRecorder(label string, seed int64) *gateRecorder {
	return &gateRecorder{
		label:     label,
		seed:      seed,
		transform: scene.IdentityAffine(),
		antiAlias: true,
	}
}

// Reset returns the recorder to a pristine pre-first-frame state.
func (g *gateRecorder) Reset() {
	*g = gateRecorder{
		label:     g.label,
		seed:      g.seed,
		transform: scene.IdentityAffine(),
		antiAlias: true,
	}
}

// BeginFrame starts a frame and records carried-over state. Stacks are
// intentionally NOT cleared: a real renderer that leaked state carries it into
// the next frame, and the gate has to observe that.
func (g *gateRecorder) BeginFrame(label string) {
	g.frameNum++
	g.frameLabel = label
	g.frameSeq = 0
	g.inFrame = true
	g.events = g.events[:0]
	g.problems = g.problems[:0]
	g.backendErrs = g.backendErrs[:0]
	g.details = g.details[:0]
	g.lastClipOp = ""
	g.transformChanges = 0

	g.carriedSaves = len(g.saves)
	g.carriedClips = len(g.clips)
	g.carriedLayers = len(g.layers)

	g.addEvent("BeginFrame", "label="+label)

	if g.carriedSaves+g.carriedClips+g.carriedLayers > 0 {
		g.addProblem(gateCrossFrameLeak, 0, fmt.Sprintf(
			"frame started with state carried from a previous frame: save=%d clip=%d layer=%d",
			g.carriedSaves, g.carriedClips, g.carriedLayers))
	}
}

// EndFrame closes the frame and returns its report. Stack contents survive so
// the next frame can observe cross-frame contamination.
func (g *gateRecorder) EndFrame() gateReport {
	if !g.inFrame {
		g.BeginFrame("implicit")
	}
	g.inFrame = false

	leftovers := g.leftoverEntries()
	first := g.firstUnmatched(leftovers)
	category := g.classify(len(leftovers) > 0)

	activeClips := make([]string, 0, len(g.clips))
	for _, c := range g.clips {
		activeClips = append(activeClips, fmt.Sprintf("%s@#%d(L%d)", c.PathSig, c.Seq, c.LayerDepth))
	}

	events := make([]string, len(g.events))
	for i, e := range g.events {
		events[i] = fmt.Sprintf("#%d %s%s [save=%d clip=%d layer=%d xform=%s]",
			e.Seq, e.Op, eventSuffix(e.Detail), e.SaveDepth, e.ClipDepth, e.LayerDepth,
			formatGateAffine(e.Transform))
	}

	carried := ""
	if g.carriedSaves+g.carriedClips+g.carriedLayers > 0 {
		carried = fmt.Sprintf("save=%d clip=%d layer=%d",
			g.carriedSaves, g.carriedClips, g.carriedLayers)
	}

	report := gateReport{
		Label:            g.frameLabel,
		Frame:            g.frameNum,
		Seed:             g.seed,
		Category:         category,
		FirstUnmatched:   first,
		SaveDepth:        len(g.saves),
		ClipDepth:        len(g.clips),
		LayerDepth:       len(g.layers),
		MaxDepth:         maxInt(g.maxSave, maxInt(g.maxClip, g.maxLayer)),
		Transform:        g.transform,
		TransformChanges: g.transformChanges,
		ActiveClips:      activeClips,
		BackendErrors:    append([]string(nil), g.backendErrs...),
		Carried:          carried,
		Details:          append([]string(nil), g.details...),
		Events:           events,
	}
	g.frames = append(g.frames, report)
	return report
}

// ReplayScene audits one frame of a scene encoding, mirroring the tag loop of
// scene.Renderer and scene.GPUSceneRenderer without rasterizing anything.
func (g *gateRecorder) ReplayScene(label string, s *scene.Scene) gateReport {
	g.BeginFrame(label)
	if s == nil {
		return g.EndFrame()
	}
	g.replayEncodingInto(s.Encoding(), g)
	return g.EndFrame()
}

// Render gives the gate the same call shape as *scene.Renderer so it can be
// substituted for the real renderer in tests. It audits the frame instead of
// rasterizing; the target is ignored. The report is available via LastReport.
func (g *gateRecorder) Render(_ *gg.Pixmap, s *scene.Scene) error {
	g.ReplayScene("render", s)
	return nil
}

// LastReport returns the most recent frame report, or a zero report if no
// frame has been audited yet.
func (g *gateRecorder) LastReport() gateReport {
	if len(g.frames) == 0 {
		return gateReport{}
	}
	return g.frames[len(g.frames)-1]
}

// Reports returns all frame reports recorded so far.
func (g *gateRecorder) Reports() []gateReport {
	return append([]gateReport(nil), g.frames...)
}

// classify picks the most severe category present in the frame.
func (g *gateRecorder) classify(hasLeftovers bool) gateCategory {
	if len(g.backendErrs) > 0 {
		return gateBackendError
	}
	rank := map[gateCategory]int{
		gateCrossLayerLeak: 5,
		gateCrossFrameLeak: 4,
		gateDuplicateClear: 3,
		gateEmptyStack:     2,
		gateUnpairedPush:   1,
	}
	best := gateCategory("")
	for _, p := range g.problems {
		if best == "" || rank[p.Kind] > rank[best] {
			best = p.Kind
		}
	}
	if best != "" {
		return best
	}
	if hasLeftovers {
		return gateUnpairedPush
	}
	if maxInt(g.maxSave, maxInt(g.maxClip, g.maxLayer)) >= 2 {
		return gateLegalNested
	}
	return gateBalanced
}

// leftoverEntries returns all entries still on any stack.
func (g *gateRecorder) leftoverEntries() []gateEntry {
	out := make([]gateEntry, 0, len(g.saves)+len(g.clips)+len(g.layers))
	out = append(out, g.saves...)
	out = append(out, g.clips...)
	out = append(out, g.layers...)
	return out
}

// firstUnmatched reports the earliest unpaired command, preferring an explicit
// offending operation (empty pop, cross-layer close) over leftovers.
func (g *gateRecorder) firstUnmatched(leftovers []gateEntry) string {
	bestSeq := -1
	bestFrame := 0
	best := ""
	for _, p := range g.problems {
		if p.Kind == gateCrossFrameLeak && p.Seq == 0 {
			if best == "" {
				best = p.Detail
			}
			continue
		}
		if bestSeq < 0 || p.Seq < bestSeq {
			best = p.Detail
			bestSeq = p.Seq
			bestFrame = g.frameNum
		}
	}
	for _, e := range leftovers {
		if bestSeq < 0 || e.Frame < bestFrame || (e.Frame == bestFrame && e.Seq < bestSeq) {
			best = e.describe() + " has no matching close before frame end"
			bestSeq = e.Seq
			bestFrame = e.Frame
		}
	}
	return best
}

// addEvent records one state transition, auto-opening a frame if needed.
func (g *gateRecorder) addEvent(op, detail string) int {
	if !g.inFrame {
		g.BeginFrame("implicit")
	}
	g.frameSeq++
	g.events = append(g.events, gateEvent{
		Seq:        g.frameSeq,
		Op:         op,
		Detail:     detail,
		SaveDepth:  len(g.saves),
		ClipDepth:  len(g.clips),
		LayerDepth: len(g.layers),
		Transform:  g.transform,
	})
	return g.frameSeq
}

// addProblem records a diagnostic without aborting the replay.
func (g *gateRecorder) addProblem(kind gateCategory, seq int, detail string) {
	g.problems = append(g.problems, gateProblem{Kind: kind, Seq: seq, Detail: detail})
	g.details = append(g.details, detail)
}

// Save pushes a saved state. cause describes what triggered it (BeginClip, a
// recording Save, ...). The saved entry snapshots the current clip stack, so a
// Restore reinstates it — exactly what gg.Context.Push/Pop and the GPU
// renderer's Push/Pop around clips do.
func (g *gateRecorder) Save(cause string) {
	label := "Save"
	if cause != "" {
		label = "Save(" + cause + ")"
	}
	seq := g.addEvent("Save", cause)
	snapshot := make([]gateEntry, len(g.clips))
	copy(snapshot, g.clips)
	g.saves = append(g.saves, gateEntry{
		Frame: g.frameNum, Seq: seq, Kind: label, LayerDepth: len(g.layers),
		clipSnapshot: snapshot,
	})
	g.maxSave = maxInt(g.maxSave, len(g.saves))
}

// Restore pops a saved state and reinstates the clip stack it captured.
// Restoring an empty stack is a state error: real backends either no-op
// silently (hiding the bug) or over-pop parent state.
func (g *gateRecorder) Restore() {
	if len(g.saves) == 0 {
		seq := g.addEvent("Restore", "empty save stack")
		g.addProblem(gateEmptyStack, seq, "Restore with empty save stack")
		return
	}
	entry := g.saves[len(g.saves)-1]
	g.saves = g.saves[:len(g.saves)-1]
	g.addEvent("Restore", entry.Kind)
	if entry.LayerDepth != len(g.layers) {
		g.addProblem(gateCrossLayerLeak, entry.Seq, fmt.Sprintf(
			"%s opened in layer %d but restored at layer %d",
			entry.Kind, entry.LayerDepth, len(g.layers)))
	}
	if entry.clipSnapshot != nil {
		// A clip installed after the Save is scoped by this Restore: that is
		// the normal recording pattern (Save / SetClip / ... / Restore), so it
		// is reinstated, not reported. Only clips that outlive the Restore are
		// leaks, and those stay on the stack.
		g.clips = append(g.clips[:len(entry.clipSnapshot)], entry.clipSnapshot...)
	}
}

// BeginClip pushes a clip region.
func (g *gateRecorder) BeginClip(pathSig string) {
	seq := g.addEvent("BeginClip", pathSig)
	if pathSig == "" {
		pathSig = "<path>"
	}
	g.clips = append(g.clips, gateEntry{
		Frame: g.frameNum, Seq: seq, Kind: "BeginClip",
		LayerDepth: len(g.layers), PathSig: pathSig,
	})
	g.maxClip = maxInt(g.maxClip, len(g.clips))
	g.lastClipOp = "push"
}

// EndClip pops a clip region.
func (g *gateRecorder) EndClip() {
	if len(g.clips) == 0 {
		seq := g.addEvent("EndClip", "empty clip stack")
		g.addProblem(gateEmptyStack, seq,
			"EndClip with empty clip stack (stale clip will leak to next frame)")
		return
	}
	entry := g.clips[len(g.clips)-1]
	g.clips = g.clips[:len(g.clips)-1]
	g.addEvent("EndClip", entry.PathSig)
	if entry.LayerDepth != len(g.layers) {
		g.addProblem(gateCrossLayerLeak, entry.Seq, fmt.Sprintf(
			"clip %s opened in layer %d but closed at layer %d",
			entry.PathSig, entry.LayerDepth, len(g.layers)))
	}
	g.lastClipOp = "pop"
}

// ClearClip removes all clip regions.
func (g *gateRecorder) ClearClip() {
	seq := g.addEvent("ClearClip", fmt.Sprintf("active=%d", len(g.clips)))
	if len(g.clips) == 0 {
		if g.lastClipOp == "clear" {
			g.addProblem(gateDuplicateClear, seq,
				"duplicate ClearClip with no clip active since the previous ClearClip")
		} else {
			g.addProblem(gateEmptyStack, seq, "ClearClip with no active clip")
		}
		g.lastClipOp = "clear"
		return
	}
	for _, entry := range g.clips {
		if entry.LayerDepth != len(g.layers) {
			g.addProblem(gateCrossLayerLeak, entry.Seq, fmt.Sprintf(
				"ClearClip at layer %d discards clip %s owned by layer %d",
				len(g.layers), entry.PathSig, entry.LayerDepth))
			break
		}
	}
	g.clips = g.clips[:0]
	g.lastClipOp = "clear"
}

// SetTransform replaces the current transform and records the change.
func (g *gateRecorder) SetTransform(t scene.Affine) {
	detail := formatGateAffine(t)
	if g.transform != t {
		detail = formatGateAffine(g.transform) + " -> " + detail
		g.transformChanges++
	}
	g.transform = t
	g.addEvent("SetTransform", detail)
}

// SetAntiAlias records an AA state change.
func (g *gateRecorder) SetAntiAlias(enabled bool) {
	g.antiAlias = enabled
	g.addEvent("SetAntiAlias", fmt.Sprintf("%t", enabled))
}

// BeginLayer pushes a compositing layer.
func (g *gateRecorder) BeginLayer(desc string) {
	seq := g.addEvent("PushLayer", desc)
	g.layers = append(g.layers, gateEntry{
		Frame: g.frameNum, Seq: seq, Kind: "PushLayer", LayerDepth: len(g.layers),
	})
	g.maxLayer = maxInt(g.maxLayer, len(g.layers))
}

// EndLayer pops a compositing layer. Any clip/save still open inside that
// layer is a cross-layer leak: the real renderer either drops it or closes it
// against the parent layer's state.
func (g *gateRecorder) EndLayer() {
	if len(g.layers) == 0 {
		seq := g.addEvent("PopLayer", "empty layer stack")
		g.addProblem(gateEmptyStack, seq, "PopLayer with empty layer stack")
		return
	}
	layerDepth := len(g.layers)
	first := g.firstOwnedState(layerDepth)
	g.layers = g.layers[:len(g.layers)-1]
	g.addEvent("PopLayer", fmt.Sprintf("depth=%d", layerDepth))
	if first != nil {
		g.addProblem(gateCrossLayerLeak, first.Seq, fmt.Sprintf(
			"%s is still open in layer %d when PopLayer runs (state leaks across layers)",
			first.Kind, layerDepth))
	}
}

// firstOwnedState returns the earliest stack entry opened at or inside the
// given layer depth, or nil.
func (g *gateRecorder) firstOwnedState(layerDepth int) *gateEntry {
	var first *gateEntry
	consider := func(entries []gateEntry) {
		for i := range entries {
			e := &entries[i]
			if e.LayerDepth < layerDepth {
				continue
			}
			if first == nil || e.Frame < first.Frame || (e.Frame == first.Frame && e.Seq < first.Seq) {
				first = e
			}
		}
	}
	consider(g.clips)
	consider(g.saves)
	return first
}

// BackendError records a backend failure. The state snapshot at failure time is
// retained in the frame report so backend errors are not mistaken for scene
// state bugs.
func (g *gateRecorder) BackendError(msg string) {
	g.backendErrs = append(g.backendErrs, msg)
	g.addEvent("BackendError", msg)
}

// replayEncodingInto walks a scene.Encoding and maps its tags onto the given
// state surface. Clip boundaries are mapped to Save/BeginClip and
// EndClip/Restore, matching scene.GPUSceneRenderer's Push/Pop around clips.
func (g *gateRecorder) replayEncodingInto(enc *scene.Encoding, sink gateSink) {
	if enc == nil {
		return
	}
	dec := scene.NewDecoder(enc)
	if dec == nil {
		return
	}

	var path gatePathSig
	pathActive := false

	for dec.Next() {
		switch dec.Tag() {
		case scene.TagTransform:
			sink.SetTransform(dec.Transform())

		case scene.TagSetAntiAlias:
			sink.SetAntiAlias(dec.AntiAlias())

		case scene.TagBeginPath:
			path.reset()
			pathActive = true

		case scene.TagMoveTo:
			x, y := dec.MoveTo()
			if pathActive {
				path.add("M", x, y)
			}

		case scene.TagLineTo:
			x, y := dec.LineTo()
			if pathActive {
				path.add("L", x, y)
			}

		case scene.TagQuadTo:
			cx, cy, x, y := dec.QuadTo()
			if pathActive {
				path.add("Q", cx, cy, x, y)
			}

		case scene.TagCubicTo:
			c1x, c1y, c2x, c2y, x, y := dec.CubicTo()
			if pathActive {
				path.add("C", c1x, c1y, c2x, c2y, x, y)
			}

		case scene.TagClosePath:
			if pathActive {
				path.add("Z")
			}

		case scene.TagEndPath:
			// Path complete; the consuming command follows.

		case scene.TagFill:
			dec.Fill()
			pathActive = false

		case scene.TagFillRoundRect:
			dec.FillRoundRect()
			pathActive = false

		case scene.TagStroke:
			dec.Stroke()
			pathActive = false

		case scene.TagPushLayer:
			blend, alpha := dec.PushLayer()
			sink.BeginLayer(fmt.Sprintf("blend=%d alpha=%.3f", blend, alpha))

		case scene.TagPopLayer:
			sink.EndLayer()

		case scene.TagBeginClip:
			sink.Save("BeginClip")
			sink.BeginClip(path.signature())
			pathActive = false

		case scene.TagEndClip:
			sink.EndClip()
			sink.Restore()

		case scene.TagImage:
			dec.Image()

		case scene.TagText:
			dec.Text()

		case scene.TagBrush:
			dec.Brush()
		}
	}
}

// gatePathSig builds a compact, bounded description of the current path.
type gatePathSig struct {
	b   strings.Builder
	ops int
}

const gatePathSigMaxOps = 24

// reset clears the signature for a new path.
func (p *gatePathSig) reset() {
	p.b.Reset()
	p.ops = 0
}

// add appends one path verb, truncating the signature past gatePathSigMaxOps.
func (p *gatePathSig) add(verb string, vals ...float32) {
	if p.ops >= gatePathSigMaxOps {
		if p.ops == gatePathSigMaxOps {
			p.b.WriteString("...")
			p.ops++
		}
		return
	}
	p.ops++
	p.b.WriteString(verb)
	for _, v := range vals {
		p.b.WriteString(",")
		fmt.Fprintf(&p.b, "%g", v)
	}
}

// signature returns the bounded path description.
func (p *gatePathSig) signature() string {
	if s := p.b.String(); s != "" {
		return s
	}
	return "empty"
}

// formatGateAffine renders a transform in a stable, compact form.
func formatGateAffine(a scene.Affine) string {
	return fmt.Sprintf("(%g,%g,%g,%g,%g,%g)", a.A, a.B, a.C, a.D, a.E, a.F)
}

// eventSuffix renders an event detail with a leading space.
func eventSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " " + detail
}

// maxInt returns the larger of two ints.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
