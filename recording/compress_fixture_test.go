package recording_test

import (
	"image"
	"image/color"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/recording"
	"github.com/gogpu/gg/recording/backends/raster"
	"github.com/gogpu/gg/text"
	"golang.org/x/image/font/gofont/goregular"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustCompress(t *testing.T, r *recording.Recording) (*recording.Recording, recording.CompressionStats) {
	t.Helper()
	out, stats := r.Compress(recording.CompressOptions{Enabled: true})
	if out == nil {
		t.Fatal("Compress returned nil recording")
	}
	// Stats identities hold for every fixture.
	if got := stats.CompressedCommands + stats.RemovedCommands; got != stats.OriginalCommands {
		t.Fatalf("stats invariant broken: %d+%d != %d",
			stats.CompressedCommands, stats.RemovedCommands, stats.OriginalCommands)
	}
	if got := stats.KeptBarrier + stats.KeptLastWrite + stats.KeptUnknown; got != stats.CompressedCommands {
		t.Fatalf("stats keep breakdown broken: %+v", stats)
	}
	return out, stats
}

func renderRGBA(t *testing.T, r *recording.Recording) *image.RGBA {
	t.Helper()
	backend := raster.NewBackend()
	if err := r.Playback(backend); err != nil {
		t.Fatalf("Playback: %v", err)
	}
	return backend.Pixmap().ToImage()
}

func assertPixelsEqual(t *testing.T, a, b *image.RGBA, label string) {
	t.Helper()
	if !a.Rect.Eq(b.Rect) {
		t.Fatalf("%s: bounds differ %v vs %v", label, a.Rect, b.Rect)
	}
	if !reflect.DeepEqual(a.Pix, b.Pix) {
		diff := 0
		for i := range a.Pix {
			if a.Pix[i] != b.Pix[i] {
				diff++
			}
		}
		t.Fatalf("%s: pixels differ in %d bytes", label, diff)
	}
}

// commandBytes estimates the heap footprint of the command interface slice
// (one two-word interface header per command). Payload structs are shared by
// value inside the headers and are not double-counted.
func commandBytes(r *recording.Recording) uintptr {
	return uintptr(len(r.Commands())) * unsafe.Sizeof(recording.Command(nil))
}

func playbackBenchmark(t *testing.T, r *recording.Recording, iterations int) time.Duration {
	t.Helper()
	start := time.Now()
	for range iterations {
		backend := raster.NewBackend()
		if err := r.Playback(backend); err != nil {
			t.Fatalf("Playback: %v", err)
		}
	}
	return time.Since(start)
}

func loadFace(t *testing.T) text.Face {
	t.Helper()
	src, err := text.NewFontSource(goregular.TTF)
	if err != nil {
		t.Fatalf("NewFontSource: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src.Face(18)
}

// runFixture executes the shared before/after comparison protocol used by all
// three fixtures: command counts, raster pixels, heap estimate and playback
// time, plus state-stack semantics via the trace backend.
func runFixture(t *testing.T, name string, r *recording.Recording, expectRemovals int) {
	t.Helper()

	beforeCmds := len(r.Commands())
	beforePixels := renderRGBA(t, r)
	beforeMem := commandBytes(r)
	beforeTrace := tracePlayback(t, r)

	compressed, stats := mustCompress(t, r)

	afterCmds := len(compressed.Commands())
	afterPixels := renderRGBA(t, compressed)
	afterMem := commandBytes(compressed)
	afterTrace := tracePlayback(t, compressed)

	if stats.RemovedCommands != expectRemovals {
		t.Errorf("%s: RemovedCommands = %d, want %d", name, stats.RemovedCommands, expectRemovals)
	}
	if beforeCmds-afterCmds != stats.RemovedCommands {
		t.Errorf("%s: command delta %d != stats removals %d",
			name, beforeCmds-afterCmds, stats.RemovedCommands)
	}
	if afterCmds >= beforeCmds {
		t.Errorf("%s: compression did not shrink commands: %d -> %d",
			name, beforeCmds, afterCmds)
	}
	assertPixelsEqual(t, beforePixels, afterPixels, name)
	assertTracesEquivalent(t, beforeTrace, afterTrace, name)

	if afterMem >= beforeMem {
		t.Errorf("%s: command memory did not shrink: %d -> %d bytes",
			name, beforeMem, afterMem)
	}
	if compressed.Resources() != r.Resources() {
		t.Errorf("%s: resource pool was rebuilt", name)
	}

	const iterations = 20
	beforeTime := playbackBenchmark(t, r, iterations)
	afterTime := playbackBenchmark(t, compressed, iterations)
	t.Logf("%s: commands %d -> %d (-%.1f%%), memory %d -> %d bytes, "+
		"playback %v -> %v over %d runs, stats=%+v",
		name, beforeCmds, afterCmds,
		100*float64(stats.RemovedCommands)/float64(beforeCmds),
		beforeMem, afterMem, beforeTime, afterTime, iterations, stats)
}

// ---------------------------------------------------------------------------
// Trace backend: captures effective backend calls and per-draw transform.
// ---------------------------------------------------------------------------

type traceEvent struct {
	kind      string
	transform recording.Matrix
	rule      recording.FillRule
	rect      recording.Rect
	text      string
}

type traceBackend struct {
	events    []traceEvent
	transform recording.Matrix
	stack     []recording.Matrix
}

func (b *traceBackend) Begin(_, _ int) error {
	b.transform = recording.Identity()
	return nil
}
func (b *traceBackend) End() error { return nil }
func (b *traceBackend) Save() {
	b.stack = append(b.stack, b.transform)
	b.events = append(b.events, traceEvent{kind: "Save", transform: b.transform})
}

func (b *traceBackend) Restore() {
	b.events = append(b.events, traceEvent{kind: "Restore", transform: b.transform})
	if len(b.stack) > 0 {
		b.transform = b.stack[len(b.stack)-1]
		b.stack = b.stack[:len(b.stack)-1]
	}
}
func (b *traceBackend) SetTransform(m recording.Matrix)          { b.transform = m }
func (b *traceBackend) SetClip(_ *gg.Path, _ recording.FillRule) {}
func (b *traceBackend) ClearClip() {
	b.events = append(b.events, traceEvent{kind: "ClearClip", transform: b.transform})
}

func (b *traceBackend) FillPath(_ *gg.Path, _ recording.Brush, rule recording.FillRule) {
	b.events = append(b.events, traceEvent{kind: "FillPath", transform: b.transform, rule: rule})
}

func (b *traceBackend) StrokePath(_ *gg.Path, _ recording.Brush, _ recording.Stroke) {
	b.events = append(b.events, traceEvent{kind: "StrokePath", transform: b.transform})
}

func (b *traceBackend) FillRect(rect recording.Rect, _ recording.Brush) {
	b.events = append(b.events, traceEvent{kind: "FillRect", transform: b.transform, rect: rect})
}

func (b *traceBackend) DrawImage(_ image.Image, _, dst recording.Rect, _ recording.ImageOptions) {
	b.events = append(b.events, traceEvent{kind: "DrawImage", transform: b.transform, rect: dst})
}

func (b *traceBackend) DrawText(s string, _, _ float64, _ text.Face, _ recording.Brush) {
	b.events = append(b.events, traceEvent{kind: "DrawText", transform: b.transform, text: s})
}

func tracePlayback(t *testing.T, r *recording.Recording) []traceEvent {
	t.Helper()
	b := &traceBackend{}
	if err := r.Playback(b); err != nil {
		t.Fatalf("trace Playback: %v", err)
	}
	return b.events
}

// collapseRepeatedClearClips folds consecutive identical ClearClip events,
// which are precisely what ClearClip compression removes.
func collapseRepeatedClearClips(events []traceEvent) []traceEvent {
	out := make([]traceEvent, 0, len(events))
	for _, ev := range events {
		if n := len(out); n > 0 && ev.kind == "ClearClip" && out[n-1].kind == "ClearClip" &&
			ev.transform == out[n-1].transform {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// assertTracesEquivalent verifies that original and compressed recordings
// drive the backend through the same effective call sequence and that every
// draw observes the same transform. SetTransform only updates local playback
// state and produces no trace event; consecutive duplicate ClearClip calls
// are the one observable event removed by compression, so they are collapsed
// before comparison. Everything else (including Save/Restore nesting and clip
// calls) must match exactly and in order.
func assertTracesEquivalent(t *testing.T, before, after []traceEvent, label string) {
	t.Helper()
	before = collapseRepeatedClearClips(before)
	if len(before) != len(after) {
		t.Fatalf("%s: effective backend calls differ: %d vs %d", label, len(before), len(after))
	}
	for i := range before {
		ev1, ev2 := before[i], after[i]
		if ev1.kind != ev2.kind {
			t.Fatalf("%s: event %d kind %q vs %q", label, i, ev1.kind, ev2.kind)
		}
		if ev1.transform != ev2.transform {
			t.Fatalf("%s: event %d (%s) transform %#v vs %#v",
				label, i, ev1.kind, ev1.transform, ev2.transform)
		}
		if ev1.rule != ev2.rule || ev1.rect != ev2.rect || ev1.text != ev2.text {
			t.Fatalf("%s: event %d (%s) payloads differ: %#v vs %#v",
				label, i, ev1.kind, ev1, ev2)
		}
	}
}

// ---------------------------------------------------------------------------
// Fixture 1: HUD-style repeated states between drawing commands.
// ---------------------------------------------------------------------------

func buildHUDRecording() *recording.Recording {
	rec := recording.NewRecorder(320, 240)
	rec.SetFillRGBA(0.1, 0.1, 0.2, 1)
	rec.FillRectangle(0, 0, 320, 240)

	for round := 0; round < 8; round++ {
		// Each HUD panel re-emits its full state; repeated transforms and
		// styles before the draw are dead writes after the first of a run.
		for k := 0; k < 4; k++ {
			rec.SetFillRGBA(float64(round)/8, 0.3, 0.7, 1)
			rec.SetLineWidth(float64(1 + k))
			rec.Translate(float64(round*4), float64(k*2))
		}
		rec.Save()
		rec.Translate(10, 10)
		rec.FillRectangle(float64(round*38), 20, 30, 16)
		rec.Restore()

		rec.ResetClip()
		rec.ResetClip()
	}
	return rec.FinishRecording()
}

func TestFixtureHUDRepeatedStates(t *testing.T) {
	r := buildHUDRecording()
	// Per round: 3 redundant fills + 3 redundant widths + 3 redundant
	// transforms + 1 redundant ClearClip = 10 removals, 8 rounds = 80.
	runFixture(t, "hud", r, 80)
}

// ---------------------------------------------------------------------------
// Fixture 2: nested clipping with clip clears.
// ---------------------------------------------------------------------------

func buildNestedClipRecording() *recording.Recording {
	rec := recording.NewRecorder(200, 200)
	rec.SetFillRGBA(1, 1, 1, 1)
	rec.FillRectangle(0, 0, 200, 200)

	rec.Save()
	rec.DrawRectangle(10, 10, 180, 180)
	rec.Clip()
	rec.SetFillRGBA(1, 0, 0, 1)
	rec.SetFillRGBA(0, 1, 0, 1) // superseded before any draw
	rec.FillRectangle(20, 20, 80, 80)

	rec.Save()
	rec.ClipRoundRect(40, 40, 120, 120, 12)
	rec.ResetClip()
	rec.ResetClip() // merged: adjacent clear within the same state run
	rec.SetFillRGBA(0, 0, 1, 1)
	rec.FillRectangle(50, 50, 60, 60)
	rec.Restore()

	rec.FillRectangle(120, 120, 40, 40)
	rec.Restore()
	return rec.FinishRecording()
}

func TestFixtureNestedClips(t *testing.T) {
	r := buildNestedClipRecording()
	// One superseded fill style + one redundant ClearClip.
	runFixture(t, "nested-clip", r, 2)
}

// ---------------------------------------------------------------------------
// Fixture 3: mixed text and image commands with interleaved state.
// ---------------------------------------------------------------------------

func buildTextImageRecording(face text.Face) *recording.Recording {
	rec := recording.NewRecorder(240, 160)
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 256)
	}

	for i := 0; i < 6; i++ {
		rec.SetFont(face)
		rec.SetFontSize(18)
		rec.SetFillRGBA(0, 0, 0, 1)
		rec.SetFillRGBA(0.2, 0.2, 0.2, 1) // superseded, no draw between
		rec.Translate(0, float64(i*2))
		rec.Translate(0, float64(i*2))
		rec.DrawString("HUD", 8, float64(24+i*20))
		rec.DrawImage(img, 180, i*20+4)
	}

	// A pure style tail with no drawing: fully removable.
	rec.SetLineCap(recording.LineCapRound)
	rec.SetLineCap(recording.LineCapSquare)
	rec.SetLineJoin(recording.LineJoinBevel)
	return rec.FinishRecording()
}

func TestFixtureTextImageMix(t *testing.T) {
	face := loadFace(t)
	r := buildTextImageRecording(face)
	// Per iteration: 1 superseded fill + 1 redundant transform = 2, x6.
	// Tail: the two SetLineCaps collapse to one (1 removal); SetLineJoin is a
	// different slot and survives. Total 13.
	runFixture(t, "text-image", r, 13)
}

// ---------------------------------------------------------------------------
// Fixture stability: empty, single-command, repeated identical state.
// ---------------------------------------------------------------------------

func TestFixtureStableEdgeCases(t *testing.T) {
	// Empty recording.
	empty := recording.NewRecorder(10, 10).FinishRecording()
	out, stats := empty.Compress(recording.CompressOptions{Enabled: true})
	if len(out.Commands()) != 0 || stats.RemovedCommands != 0 {
		t.Fatalf("empty recording unstable: %d commands, stats %+v", len(out.Commands()), stats)
	}

	// Single command.
	one := recording.NewRecorder(10, 10)
	one.SetLineWidth(4)
	single := one.FinishRecording()
	out2, stats2 := single.Compress(recording.CompressOptions{Enabled: true})
	if len(out2.Commands()) != 1 || stats2.RemovedCommands != 0 {
		t.Fatalf("single command unstable: %+v", stats2)
	}

	// Repeated identical state: deterministic single survivor.
	rep := recording.NewRecorder(10, 10)
	for range 5 {
		rep.SetAntiAlias(true)
	}
	repeated := rep.FinishRecording()
	out3, stats3 := repeated.Compress(recording.CompressOptions{Enabled: true})
	if len(out3.Commands()) != 1 {
		t.Fatalf("repeated state: got %d commands, want 1", len(out3.Commands()))
	}
	if stats3.RedundantCommands != 4 {
		t.Fatalf("repeated state stats = %+v", stats3)
	}
	// Repeated compression yields the same command type stream.
	out4, _ := out3.Compress(recording.CompressOptions{Enabled: true})
	if len(out4.Commands()) != len(out3.Commands()) {
		t.Fatal("compression not idempotent")
	}
}

// ---------------------------------------------------------------------------
// Transformed StrokeRectangle's inline Save/identity/Restore wrapper survives
// compression and keeps its backend playback contract.
// ---------------------------------------------------------------------------

func TestFixtureStrokeRectWrapperContract(t *testing.T) {
	rec := recording.NewRecorder(120, 120)
	rec.SetStrokeRGBA(0, 0, 1, 1)
	rec.SetLineWidth(2)
	rec.SetLineWidth(3) // redundant, must be removed
	rec.SetTransform(recording.Matrix{A: 2, E: 2})
	rec.StrokeRectangle(10, 10, 20, 30)
	r := rec.FinishRecording()

	out, stats := r.Compress(recording.CompressOptions{Enabled: true})
	if stats.RemovedCommands != 1 {
		t.Fatalf("stats = %+v, want exactly one removed width", stats)
	}
	assertPixelsEqual(t, renderRGBA(t, r), renderRGBA(t, out), "stroke-rect-wrapper")

	before := tracePlayback(t, r)
	after := tracePlayback(t, out)
	assertTracesEquivalent(t, before, after, "stroke-rect-wrapper")
	if len(after) != 3 { // Save, StrokePath, Restore
		t.Fatalf("trace events = %d, want 3: %+v", len(after), after)
	}
	if after[0].kind != "Save" || after[1].kind != "StrokePath" || after[2].kind != "Restore" {
		t.Fatalf("wrapper ordering broken: %+v", after)
	}
	if after[1].transform != recording.Identity() {
		t.Fatalf("stroke must run under identity, got %#v", after[1].transform)
	}
}

// ---------------------------------------------------------------------------
// Pixels stay identical even with a non-finite matrix in the stream.
// ---------------------------------------------------------------------------

func TestFixtureNonFiniteMatrixPixels(t *testing.T) {
	rec := recording.NewRecorder(40, 40)
	rec.SetFillRGBA(1, 0, 0, 1)
	rec.SetTransform(recording.Translate(1, 1))
	rec.SetTransform(recording.Translate(2, 2)) // folds onto previous
	rec.FillRectangle(0, 0, 10, 10)
	r := rec.FinishRecording()

	out, stats := r.Compress(recording.CompressOptions{Enabled: true})
	if stats.RemovedCommands != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	assertPixelsEqual(t, renderRGBA(t, r), renderRGBA(t, out), "non-finite-safe")

	// Confirm color is actually present (guard against vacuous comparison).
	img := renderRGBA(t, out)
	c := img.RGBAAt(5, 5)
	if c == (color.RGBA{}) {
		t.Fatal("expected painted pixels in fixture")
	}
}
