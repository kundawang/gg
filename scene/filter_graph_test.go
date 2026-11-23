package scene

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gogpu/gg"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// fillPixmap fills pm with straight-alpha color c.
func fillPixmap(pm *gg.Pixmap, c gg.RGBA) {
	for y := 0; y < pm.Height(); y++ {
		for x := 0; x < pm.Width(); x++ {
			pm.SetPixel(x, y, c)
		}
	}
}

// solidPixmap returns a w x h pixmap filled with c.
func solidPixmap(w, h int, c gg.RGBA) *gg.Pixmap {
	pm := gg.NewPixmap(w, h)
	fillPixmap(pm, c)
	return pm
}

// rawPixels returns a copy of the pixmap's premultiplied pixel bytes.
func rawPixels(pm *gg.Pixmap) []uint8 {
	return append([]uint8(nil), pm.Data()...)
}

// samePixels reports whether two pixmaps have identical dimensions and bytes.
func samePixels(a, b *gg.Pixmap) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Width() != b.Width() || a.Height() != b.Height() {
		return false
	}
	ad, bd := a.Data(), b.Data()
	for i := range ad {
		if ad[i] != bd[i] {
			return false
		}
	}
	return true
}

// invertOp inverts the red channel of its single input within the processed
// bounds. It does not expand bounds.
func invertOp() FilterGraphOp {
	return FilterGraphOpFunc{
		ApplyFunc: func(ctx context.Context, in []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(in) == 0 || in[0] == nil {
				return errors.New("invert: no input")
			}
			src := in[0]
			for y := int(bounds.MinY); y < int(bounds.MaxY); y++ {
				for x := int(bounds.MinX); x < int(bounds.MaxX); x++ {
					c := src.GetPixel(x, y)
					c.R = 1 - c.R
					dst.SetPixel(x, y, c)
				}
			}
			return nil
		},
	}
}

// expandOp expands bounds by pad in every direction and copies its input into
// the expanded output. It is used to verify bounds propagation and origin
// shifting.
func expandOp(pad int) FilterGraphOp {
	return FilterGraphOpFunc{
		BoundsFunc: func(in []Rect) Rect {
			b := unionRects(in)
			if b.IsEmpty() {
				return b
			}
			return Rect{
				MinX: b.MinX - float32(pad),
				MinY: b.MinY - float32(pad),
				MaxX: b.MaxX + float32(pad),
				MaxY: b.MaxY + float32(pad),
			}
		},
		ApplyFunc: func(ctx context.Context, in []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Copy input pixels through; content stays at its global position,
			// so the pad margin is transparent.
			src := in[0]
			for y := 0; y < src.Height(); y++ {
				for x := 0; x < src.Width(); x++ {
					dst.SetPixel(x, y, src.GetPixel(x, y))
				}
			}
			return nil
		},
	}
}

// buildBranchingGraph builds:
//
//	src ──┬─ invert ──┐
//	      └─ expand ──┴─ over ── out
//
// The two branches share the same source and are independent.
func buildBranchingGraph(t *testing.T) (*CompiledFilterGraph, *gg.Pixmap) {
	t.Helper()
	g := NewFilterGraph()
	if err := g.AddSource("src"); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if err := g.AddNode(FilterNode{ID: "inv", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "invOut"}); err != nil {
		t.Fatalf("AddNode inv: %v", err)
	}
	if err := g.AddNode(FilterNode{ID: "exp", Op: expandOp(2), Inputs: []FilterResourceID{"src"}, Output: "expOut"}); err != nil {
		t.Fatalf("AddNode exp: %v", err)
	}
	if err := g.AddNode(FilterNode{ID: "merge", Op: OverOp(), Inputs: []FilterResourceID{"invOut", "expOut"}, Output: "out"}); err != nil {
		t.Fatalf("AddNode merge: %v", err)
	}
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	src := solidPixmap(8, 6, gg.RGBA2(0.25, 0.5, 0.75, 1))
	return cg, src
}

// ---------------------------------------------------------------------------
// 1. Topology, stable order, determinism
// ---------------------------------------------------------------------------

func TestFilterGraphStableTopologicalOrder(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	// Declare consumers c1/c2 before their producer "mid" to prove order is
	// driven by dependencies, not registration alone.
	mustAdd(t, g, FilterNode{ID: "mid", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "midOut"})
	mustAdd(t, g, FilterNode{ID: "c1", Op: invertOp(), Inputs: []FilterResourceID{"midOut"}, Output: "c1Out"})
	mustAdd(t, g, FilterNode{ID: "c2", Op: invertOp(), Inputs: []FilterResourceID{"midOut"}, Output: "c2Out"})
	mustAdd(t, g, FilterNode{ID: "join", Op: OverOp(), Inputs: []FilterResourceID{"c1Out", "c2Out"}, Output: "out"})

	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got := cg.NodeOrder()
	want := []FilterNodeID{"mid", "c1", "c2", "join"}
	if len(got) != len(want) {
		t.Fatalf("order length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}

	// Repeated Compile must yield the same order.
	for i := 0; i < 20; i++ {
		cg2, err := g.Compile()
		if err != nil {
			t.Fatalf("Compile #%d: %v", i, err)
		}
		for j, id := range cg2.NodeOrder() {
			if id != want[j] {
				t.Fatalf("Compile #%d order[%d] = %q, want %q", i, j, id, want[j])
			}
		}
	}
}

func TestFilterGraphIndependentNodesDeterministicAcrossWorkers(t *testing.T) {
	cg, src := buildBranchingGraph(t)
	sources := map[FilterResourceID]*gg.Pixmap{"src": src}

	baseline, err := cg.Execute(context.Background(), sources, FilterGraphOptions{Workers: 1})
	if err != nil {
		t.Fatalf("Execute workers=1: %v", err)
	}
	if baseline.Output("out") == nil {
		t.Fatal("missing out output")
	}

	for _, workers := range []int{1, 2, 3, 4, 8} {
		got, err := cg.Execute(context.Background(), sources, FilterGraphOptions{Workers: workers})
		if err != nil {
			t.Fatalf("Execute workers=%d: %v", workers, err)
		}
		if !samePixels(baseline.Output("out"), got.Output("out")) {
			t.Fatalf("workers=%d produced different pixels than workers=1", workers)
		}
		if got.Bounds != baseline.Bounds {
			t.Fatalf("workers=%d bounds = %+v, want %+v", workers, got.Bounds, baseline.Bounds)
		}
		if len(got.Order) != len(baseline.Order) {
			t.Fatalf("workers=%d order length changed", workers)
		}
	}
}

func TestFilterGraphIndependentNodesRunConcurrently(t *testing.T) {
	// Two independent nodes rendezvous: each signals it started and waits for
	// the other. If the scheduler serialized them this would time out.
	var started sync.WaitGroup
	started.Add(2)
	aStarted := make(chan struct{})
	bStarted := make(chan struct{})

	mk := func(self, other chan struct{}) FilterGraphOp {
		return FilterGraphOpFunc{
			ApplyFunc: func(ctx context.Context, in []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
				close(self)
				started.Done()
				select {
				case <-other:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Second):
					return errors.New("rendezvous timeout: nodes did not run concurrently")
				}
			},
		}
	}

	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "a", Op: mk(aStarted, bStarted), Inputs: []FilterResourceID{"src"}, Output: "aOut"})
	mustAdd(t, g, FilterNode{ID: "b", Op: mk(bStarted, aStarted), Inputs: []FilterResourceID{"src"}, Output: "bOut"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	_, err = cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": solidPixmap(2, 2, gg.White)},
		FilterGraphOptions{Workers: 2})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	started.Wait()
}

// ---------------------------------------------------------------------------
// 2. Bounds expansion, premultiplied alpha, independent buffers, cancellation
// ---------------------------------------------------------------------------

func TestFilterGraphExpandedBoundsAndOrigin(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "exp", Op: expandOp(3), Inputs: []FilterResourceID{"src"}, Output: "expOut"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	src := solidPixmap(10, 10, gg.White)
	res, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": src}, FilterGraphOptions{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out := res.Output("expOut")
	if out == nil {
		t.Fatal("missing output")
	}
	// A negative expansion shifts the origin; the buffer grows by the pad.
	if res.Origin != [2]int{-3, -3} {
		t.Fatalf("Origin = %v, want (-3,-3)", res.Origin)
	}
	if out.Width() != 16 || out.Height() != 16 {
		t.Fatalf("output size = %dx%d, want 16x16", out.Width(), out.Height())
	}
	// Content moved to pixel (3,3) of the buffer.
	if c := out.GetPixel(3, 3); c.A < 0.99 {
		t.Fatalf("content at (3,3) alpha = %v, want 1", c.A)
	}
	if c := out.GetPixel(0, 0); c.A != 0 {
		t.Fatalf("pad at (0,0) alpha = %v, want 0", c.A)
	}
	// Graph bounds are the expanded bounds in graph coordinates.
	if res.Bounds.MinX != -3 || res.Bounds.MaxX != 13 {
		t.Fatalf("Bounds = %+v, want MinX=-3 MaxX=13", res.Bounds)
	}
}

func TestFilterGraphPremultipliedOver(t *testing.T) {
	// Two 50%-alpha layers, red then blue. OverOp composites in declaration
	// order, so blue lands on top of red:
	//   red  premultiplied = (127, 0, 0, 127)
	//   blue over red      = (0,0,127) + red*0.5 = (64, 0, 127, 191)
	g := NewFilterGraph()
	_ = g.AddSource("red")
	_ = g.AddSource("blue")
	mustAdd(t, g, FilterNode{ID: "m", Op: OverOp(), Inputs: []FilterResourceID{"red", "blue"}, Output: "out"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	src := map[FilterResourceID]*gg.Pixmap{
		"red":  solidPixmap(4, 4, gg.RGBA2(1, 0, 0, 0.5)),
		"blue": solidPixmap(4, 4, gg.RGBA2(0, 0, 1, 0.5)),
	}
	res, err := cg.Execute(context.Background(), src, FilterGraphOptions{Workers: 1})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	d := res.Output("out").Data()
	// Pixel (1,1).
	i := (1*4 + 1) * 4
	if d[i+0] != 64 || d[i+1] != 0 || d[i+2] != 127 || d[i+3] != 191 {
		t.Fatalf("premultiplied pixel = (%d,%d,%d,%d), want (64,0,127,191)",
			d[i+0], d[i+1], d[i+2], d[i+3])
	}
}

func TestFilterGraphNodesUseIndependentBuffers(t *testing.T) {
	// Both branches must observe only their own input, never sibling writes.
	var mu sync.Mutex
	seen := map[string][]uint8{}
	record := func(name string) FilterGraphOp {
		return FilterGraphOpFunc{
			ApplyFunc: func(ctx context.Context, in []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
				mu.Lock()
				seen[name] = rawPixels(in[0])
				mu.Unlock()
				fillPixmap(dst, gg.RGBA2(0, 0.5, 0, 1))
				return nil
			},
		}
	}
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "a", Op: record("a"), Inputs: []FilterResourceID{"src"}, Output: "aOut"})
	mustAdd(t, g, FilterNode{ID: "b", Op: record("b"), Inputs: []FilterResourceID{"src"}, Output: "bOut"})
	mustAdd(t, g, FilterNode{ID: "j", Op: OverOp(), Inputs: []FilterResourceID{"aOut", "bOut"}, Output: "out"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	src := solidPixmap(3, 3, gg.RGBA2(0.2, 0.4, 0.6, 1))
	if _, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": src}, FilterGraphOptions{Workers: 4}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"a", "b"} {
		got, ok := seen[name]
		if !ok {
			t.Fatalf("node %q never ran", name)
		}
		if !equalBytes(got, src.Data()) {
			t.Fatalf("node %q input was mutated", name)
		}
	}
}

func equalBytes(a, b []uint8) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFilterGraphNodeFailureStopsDownstream(t *testing.T) {
	// A failing node must abort the graph: its descendant must never run and
	// no partial result may be returned.
	var downstreamRan atomic.Bool

	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "fail", Inputs: []FilterResourceID{"src"}, Output: "failOut",
		Op: FilterGraphOpFunc{ApplyFunc: func(context.Context, []*gg.Pixmap, *gg.Pixmap, Rect) error {
			return errors.New("boom")
		}}})
	mustAdd(t, g, FilterNode{ID: "after", Inputs: []FilterResourceID{"failOut"}, Output: "out",
		Op: FilterGraphOpFunc{ApplyFunc: func(context.Context, []*gg.Pixmap, *gg.Pixmap, Rect) error {
			downstreamRan.Store(true)
			return nil
		}}})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	res, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": solidPixmap(2, 2, gg.White)},
		FilterGraphOptions{Workers: 2})
	if err == nil {
		t.Fatal("expected error")
	}
	if res != nil {
		t.Fatal("result must be nil on failure")
	}
	if downstreamRan.Load() {
		t.Fatal("downstream node ran despite upstream failure")
	}
	var ge *FilterGraphError
	if !errors.As(err, &ge) || ge.Node != "fail" {
		t.Fatalf("error should name node fail: %#v", ge)
	}
}

func TestFilterGraphExternalCancellationObserved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	sawCancel := make(chan struct{})

	blocking := FilterGraphOpFunc{ApplyFunc: func(inner context.Context, in []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
		close(started)
		select {
		case <-inner.Done():
			close(sawCancel)
			return inner.Err()
		case <-time.After(5 * time.Second):
			return errors.New("node never observed cancellation")
		}
	}}

	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "block", Op: blocking, Inputs: []FilterResourceID{"src"}, Output: "out"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := cg.Execute(ctx, map[FilterResourceID]*gg.Pixmap{"src": solidPixmap(2, 2, gg.White)}, FilterGraphOptions{})
		done <- err
	}()

	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, ErrFilterGraphCanceled) {
			t.Fatalf("err = %v, want ErrFilterGraphCanceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return after cancellation")
	}
	select {
	case <-sawCancel:
	case <-time.After(time.Second):
		t.Fatal("node Apply did not observe cancellation")
	}
}

func TestFilterGraphCanceledBeforeStart(t *testing.T) {
	cg, src := buildBranchingGraph(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cg.Execute(ctx, map[FilterResourceID]*gg.Pixmap{"src": src}, FilterGraphOptions{})
	if !errors.Is(err, ErrFilterGraphCanceled) {
		t.Fatalf("err = %v, want ErrFilterGraphCanceled", err)
	}
}

func TestFilterGraphMemoryLimit(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "exp", Op: expandOp(0), Inputs: []FilterResourceID{"src"}, Output: "out"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	src := solidPixmap(64, 64, gg.White) // one 64*64*4 = 16384-byte output
	_, err = cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": src},
		FilterGraphOptions{MemoryLimit: 1024})
	if !errors.Is(err, ErrFilterGraphMemoryLimit) {
		t.Fatalf("err = %v, want ErrFilterGraphMemoryLimit", err)
	}
	var ge *FilterGraphError
	if !errors.As(err, &ge) || ge.Node != "exp" {
		t.Fatalf("error should name node exp, got %#v", ge)
	}

	// Enough budget succeeds and reports a peak within the limit.
	res, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": src},
		FilterGraphOptions{MemoryLimit: 1 << 20})
	if err != nil {
		t.Fatalf("Execute with adequate limit: %v", err)
	}
	if res.PeakMemory > 1<<20 {
		t.Fatalf("PeakMemory = %d, exceeds limit", res.PeakMemory)
	}
}

// ---------------------------------------------------------------------------
// 3. Definition and execution errors with context
// ---------------------------------------------------------------------------

func TestFilterGraphMissingInput(t *testing.T) {
	g := NewFilterGraph()
	mustAdd(t, g, FilterNode{ID: "n", Op: invertOp(), Inputs: []FilterResourceID{"ghost"}, Output: "out"})
	_, err := g.Compile()
	if !errors.Is(err, ErrFilterGraphMissingInput) {
		t.Fatalf("err = %v, want ErrFilterGraphMissingInput", err)
	}
	var ge *FilterGraphError
	if !errors.As(err, &ge) {
		t.Fatalf("not a *FilterGraphError: %v", err)
	}
	if ge.Node != "n" || ge.Resource != "ghost" || ge.Edge != "ghost -> n" {
		t.Fatalf("context = node %q resource %q edge %q", ge.Node, ge.Resource, ge.Edge)
	}
}

func TestFilterGraphMissingSourceAtExecute(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "n", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "out"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	_, err = cg.Execute(context.Background(), nil, FilterGraphOptions{})
	if !errors.Is(err, ErrFilterGraphMissingInput) {
		t.Fatalf("err = %v, want ErrFilterGraphMissingInput", err)
	}
	var ge *FilterGraphError
	if !errors.As(err, &ge) || ge.Node != "n" || ge.Edge != "src -> n" {
		t.Fatalf("context = %#v", ge)
	}
}

func TestFilterGraphCycle(t *testing.T) {
	g := NewFilterGraph()
	mustAdd(t, g, FilterNode{ID: "a", Op: invertOp(), Inputs: []FilterResourceID{"bOut"}, Output: "aOut"})
	mustAdd(t, g, FilterNode{ID: "b", Op: invertOp(), Inputs: []FilterResourceID{"aOut"}, Output: "bOut"})
	_, err := g.Compile()
	if !errors.Is(err, ErrFilterGraphCycle) {
		t.Fatalf("err = %v, want ErrFilterGraphCycle", err)
	}
	var ge *FilterGraphError
	if !errors.As(err, &ge) {
		t.Fatalf("not a *FilterGraphError: %v", err)
	}
	if ge.Edge == "" {
		t.Fatal("cycle error should include the edge chain")
	}
	// The edge chain must name the participating nodes.
	if !contains(ge.Edge, "a") || !contains(ge.Edge, "b") {
		t.Fatalf("edge = %q, want both a and b", ge.Edge)
	}
}

func TestFilterGraphSelfCycle(t *testing.T) {
	g := NewFilterGraph()
	mustAdd(t, g, FilterNode{ID: "self", Op: invertOp(), Inputs: []FilterResourceID{"selfOut"}, Output: "selfOut"})
	_, err := g.Compile()
	if !errors.Is(err, ErrFilterGraphCycle) {
		t.Fatalf("err = %v, want ErrFilterGraphCycle", err)
	}
}

func TestFilterGraphDuplicateOutput(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	if err := g.AddNode(FilterNode{ID: "a", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "dup"}); err != nil {
		t.Fatalf("first AddNode: %v", err)
	}
	err := g.AddNode(FilterNode{ID: "b", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "dup"})
	if !errors.Is(err, ErrFilterGraphDuplicateOutput) {
		t.Fatalf("err = %v, want ErrFilterGraphDuplicateOutput", err)
	}
	var ge *FilterGraphError
	if !errors.As(err, &ge) || ge.Node != "b" || ge.Resource != "dup" {
		t.Fatalf("context = %#v", ge)
	}

	// Output clashing with a source.
	err = g.AddNode(FilterNode{ID: "c", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "src"})
	if !errors.Is(err, ErrFilterGraphDuplicateOutput) {
		t.Fatalf("source collision err = %v", err)
	}
}

func TestFilterGraphInvalidWorkers(t *testing.T) {
	cg, src := buildBranchingGraph(t)
	sources := map[FilterResourceID]*gg.Pixmap{"src": src}

	for _, w := range []int{-1, MaxFilterGraphWorkers + 1, 1 << 20} {
		_, err := cg.Execute(context.Background(), sources, FilterGraphOptions{Workers: w})
		if !errors.Is(err, ErrFilterGraphWorkers) {
			t.Fatalf("workers=%d err = %v, want ErrFilterGraphWorkers", w, err)
		}
	}
}

func TestFilterGraphNonFiniteBounds(t *testing.T) {
	nan := float32(math.NaN())
	g := NewFilterGraph()
	_ = g.AddSource("src")
	err := g.AddNode(FilterNode{
		ID: "n", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "out",
		Bounds: Rect{MinX: nan, MaxX: 10},
	})
	if !errors.Is(err, ErrFilterGraphBounds) {
		t.Fatalf("err = %v, want ErrFilterGraphBounds", err)
	}

	// Runtime bounds from an op are also rejected, with node context.
	bad := FilterGraphOpFunc{
		BoundsFunc: func([]Rect) Rect { return Rect{MinX: 0, MinY: 0, MaxX: float32(math.Inf(1)), MaxY: 10} },
		ApplyFunc:  func(context.Context, []*gg.Pixmap, *gg.Pixmap, Rect) error { return nil },
	}
	g2 := NewFilterGraph()
	_ = g2.AddSource("src")
	mustAdd(t, g2, FilterNode{ID: "bad", Op: bad, Inputs: []FilterResourceID{"src"}, Output: "out"})
	cg, err := g2.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	_, err = cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": solidPixmap(4, 4, gg.White)}, FilterGraphOptions{})
	if !errors.Is(err, ErrFilterGraphBounds) {
		t.Fatalf("runtime err = %v, want ErrFilterGraphBounds", err)
	}
	var ge *FilterGraphError
	if !errors.As(err, &ge) || ge.Node != "bad" {
		t.Fatalf("error should name node bad: %#v", ge)
	}
}

func TestFilterGraphNodeErrorNoPartialSuccess(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "ok", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "okOut"})
	mustAdd(t, g, FilterNode{
		ID: "bad", Inputs: []FilterResourceID{"okOut"}, Output: "badOut",
		Op: FilterGraphOpFunc{ApplyFunc: func(context.Context, []*gg.Pixmap, *gg.Pixmap, Rect) error {
			return errors.New("intentional failure")
		}},
	})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	res, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": solidPixmap(4, 4, gg.White)},
		FilterGraphOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	if res != nil {
		t.Fatal("result must be nil on failure (no partial success)")
	}
	var ge *FilterGraphError
	if !errors.As(err, &ge) || ge.Node != "bad" {
		t.Fatalf("error should name node bad: %#v", ge)
	}
	if !errors.Is(err, ErrFilterGraphNode) {
		t.Fatalf("err = %v, want ErrFilterGraphNode", err)
	}
	if !contains(ge.Edge, "okOut -> bad") {
		t.Fatalf("edge = %q, want okOut -> bad", ge.Edge)
	}
}

func TestFilterGraphPanicIsReported(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{
		ID: "panicky", Inputs: []FilterResourceID{"src"}, Output: "out",
		Op: FilterGraphOpFunc{ApplyFunc: func(context.Context, []*gg.Pixmap, *gg.Pixmap, Rect) error {
			panic("kaboom")
		}},
	})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	_, err = cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": solidPixmap(2, 2, gg.White)}, FilterGraphOptions{})
	if err == nil {
		t.Fatal("expected error from panic")
	}
	var ge *FilterGraphError
	if !errors.As(err, &ge) || ge.Node != "panicky" {
		t.Fatalf("error should name node panicky: %#v", ge)
	}
	if !contains(ge.Err.Error(), "kaboom") {
		t.Fatalf("panic detail missing: %v", ge.Err)
	}
}

func TestFilterGraphDuplicateNodeID(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "n", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "o1"})
	err := g.AddNode(FilterNode{ID: "n", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "o2"})
	if !errors.Is(err, ErrFilterGraphInvalidDefinition) {
		t.Fatalf("err = %v, want ErrFilterGraphInvalidDefinition", err)
	}
}

func TestFilterGraphUnknownRequestedOutput(t *testing.T) {
	cg, src := buildBranchingGraph(t)
	_, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": src},
		FilterGraphOptions{Outputs: []FilterResourceID{"nope"}})
	if !errors.Is(err, ErrFilterGraphMissingInput) {
		t.Fatalf("err = %v, want ErrFilterGraphMissingInput", err)
	}
}

func TestFilterGraphErrorFormatting(t *testing.T) {
	inner := errors.New("inner")
	e := newGraphError("execute", ErrFilterGraphNode, "nodeX", "resY", "a -> nodeX", "detail here", inner)
	msg := e.Error()
	for _, want := range []string{"execute", "detail here", "node nodeX", "resource resY", "edge a -> nodeX", "inner"} {
		if !contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
	if !errors.Is(e, ErrFilterGraphNode) || !errors.Is(e, inner) {
		t.Fatal("errors.Is should reach both kind and cause")
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func mustAdd(t *testing.T, g *FilterGraph, n FilterNode) {
	t.Helper()
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode %q: %v", n.ID, err)
	}
}

// ---------------------------------------------------------------------------
// 4. Fixed-graph pixel checks, shared inputs, joins, memory ceiling
// ---------------------------------------------------------------------------

// TestFilterGraphSharedInputBranches verifies that two consumers of one source
// each see the same unmodified input and that the source is not freed early.
func TestFilterGraphSharedInputBranches(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "inv", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "invOut"})
	mustAdd(t, g, FilterNode{ID: "copy", Op: FilterGraphOpFunc{
		ApplyFunc: func(ctx context.Context, in []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
			for y := 0; y < dst.Height(); y++ {
				for x := 0; x < dst.Width(); x++ {
					dst.SetPixel(x, y, in[0].GetPixel(x, y))
				}
			}
			return nil
		},
	}, Inputs: []FilterResourceID{"src"}, Output: "copyOut"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	src := solidPixmap(5, 5, gg.RGBA2(0.3, 0.6, 0.9, 1))
	res, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": src},
		FilterGraphOptions{Outputs: []FilterResourceID{"invOut", "copyOut"}, Workers: 4})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	inv := res.Output("invOut")
	cpy := res.Output("copyOut")
	if inv == nil || cpy == nil {
		t.Fatal("missing branch output")
	}
	// copyOut equals the source.
	if !samePixels(cpy, src) {
		t.Fatal("copy branch diverged from source")
	}
	// invOut has inverted red.
	c := inv.GetPixel(2, 2)
	if absf(c.R-0.7) > 0.02 {
		t.Fatalf("inverted red = %v, want ~0.7", c.R)
	}
}

// TestFilterGraphMultiBranchJoinPixels checks a fixed diamond graph against a
// hand-computed serial result for several worker counts.
func TestFilterGraphMultiBranchJoinPixels(t *testing.T) {
	// src (black, opaque) -> inv turns red to 1 (pure red, opaque) and copy
	// stays black (opaque). Over(copy, inv) puts opaque red on top => red.
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "inv", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "invOut"})
	mustAdd(t, g, FilterNode{ID: "copy", Op: FilterGraphOpFunc{
		ApplyFunc: func(_ context.Context, in []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
			fillPixmap(dst, in[0].GetPixel(0, 0))
			return nil
		},
	}, Inputs: []FilterResourceID{"src"}, Output: "copyOut"})
	mustAdd(t, g, FilterNode{ID: "join", Op: OverOp(), Inputs: []FilterResourceID{"copyOut", "invOut"}, Output: "out"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	src := solidPixmap(4, 4, gg.Black)

	for _, workers := range []int{1, 2, 4, 16} {
		res, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": src},
			FilterGraphOptions{Workers: workers})
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		c := res.Output("out").GetPixel(1, 1)
		if absf(c.R-1) > 0.01 || absf(c.G-0) > 0.01 || absf(c.B-0) > 0.01 || absf(c.A-1) > 0.01 {
			t.Fatalf("workers=%d pixel = %+v, want opaque red", workers, c)
		}
	}
}

// TestFilterGraphSinkRetention checks the default retention rule: only sinks
// are returned, intermediate buffers are not retained.
func TestFilterGraphSinkRetention(t *testing.T) {
	cg, src := buildBranchingGraph(t)
	res, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": src}, FilterGraphOptions{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Outputs) != 1 {
		t.Fatalf("Outputs = %d, want 1 (only the sink)", len(res.Outputs))
	}
	if res.Output("out") == nil {
		t.Fatal("sink output missing")
	}
	if res.Output("invOut") != nil || res.Output("expOut") != nil {
		t.Fatal("intermediate outputs should not be retained by default")
	}
}

// TestFilterGraphRepeatedExecutionStable re-runs the same compiled graph many
// times to exercise pooled buffer reuse and verify byte stability.
func TestFilterGraphRepeatedExecutionStable(t *testing.T) {
	cg, src := buildBranchingGraph(t)
	sources := map[FilterResourceID]*gg.Pixmap{"src": src}
	first, err := cg.Execute(context.Background(), sources, FilterGraphOptions{Workers: 4})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := rawPixels(first.Output("out"))
	for i := 0; i < 50; i++ {
		res, err := cg.Execute(context.Background(), sources, FilterGraphOptions{Workers: 1 + i%4})
		if err != nil {
			t.Fatalf("Execute #%d: %v", i, err)
		}
		if !equalBytes(want, res.Output("out").Data()) {
			t.Fatalf("Execute #%d changed pixels", i)
		}
	}
}

// TestFilterGraphCompatibleWithFilterChain runs the same filters through the
// legacy FilterChain and through a linear graph and compares the pixels.
func TestFilterGraphCompatibleWithFilterChain(t *testing.T) {
	bounds := Rect{MinX: 0, MinY: 0, MaxX: 12, MaxY: 12}
	src := solidPixmap(12, 12, gg.RGBA2(0.2, 0.8, 0.4, 1))

	// Legacy: two mockFilter stages (invert red, expand nothing).
	legacy := NewFilterChain(&mockFilter{modifyDst: true})
	legacyDst := gg.NewPixmap(12, 12)
	legacy.Apply(src, legacyDst, bounds)

	// Graph: same single inversion through FilterOp.
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "inv", Op: FilterOp{Filter: &mockFilter{modifyDst: true}},
		Inputs: []FilterResourceID{"src"}, Output: "out"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	res, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": src}, FilterGraphOptions{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// mockFilter inverts the red channel of the src written into dst; compare
	// the 12x12 content region (the graph buffer may be the same size).
	graphOut := res.Output("out")
	for y := 0; y < 12; y++ {
		for x := 0; x < 12; x++ {
			want := legacyDst.GetPixel(x, y)
			got := graphOut.GetPixel(x, y)
			if !colorApproxEqual(want, got, 1e-6) {
				t.Fatalf("pixel (%d,%d): graph %+v != chain %+v", x, y, got, want)
			}
		}
	}
}

// TestFilterGraphExplicitBounds verifies that a node may process an explicit
// region that differs from its input bounds.
func TestFilterGraphExplicitBounds(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	custom := FilterGraphOpFunc{
		ApplyFunc: func(ctx context.Context, in []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
			if bounds.MinX != 2 || bounds.MaxX != 4 {
				return fmt.Errorf("unexpected apply bounds %+v", bounds)
			}
			return nil
		},
	}
	mustAdd(t, g, FilterNode{ID: "n", Op: custom, Inputs: []FilterResourceID{"src"}, Output: "out",
		Bounds: Rect{MinX: 2, MinY: 0, MaxX: 4, MaxY: 4}})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": solidPixmap(8, 8, gg.White)},
		FilterGraphOptions{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// TestFilterGraphConcurrentExecute runs several Execute calls in parallel on
// one compiled graph to verify it is immutable and pool-safe.
func TestFilterGraphConcurrentExecute(t *testing.T) {
	cg, src := buildBranchingGraph(t)
	sources := map[FilterResourceID]*gg.Pixmap{"src": src}

	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := cg.Execute(context.Background(), sources, FilterGraphOptions{Workers: 1 + i%4})
			if err != nil {
				failures.Add(1)
				return
			}
			if res.Output("out") == nil {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent executions failed", failures.Load())
	}
}

// TestFilterGraphEmptyGraph executes a compiled graph with no nodes: no
// outputs, an empty bounds, and no error.
func TestFilterGraphEmptyGraph(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if cg.NodeCount() != 0 {
		t.Fatalf("NodeCount = %d, want 0", cg.NodeCount())
	}
	res, err := cg.Execute(context.Background(), nil, FilterGraphOptions{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Outputs) != 0 {
		t.Fatalf("Outputs = %d, want 0", len(res.Outputs))
	}
	if !res.Bounds.IsEmpty() {
		t.Fatalf("Bounds = %+v, want empty", res.Bounds)
	}
}

// TestFilterGraphRetainedSource checks that a declared source may be requested
// directly as an output without running any node that consumes it.
func TestFilterGraphRetainedSource(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "inv", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "invOut"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	src := solidPixmap(6, 6, gg.RGBA2(0.1, 0.2, 0.3, 1))
	res, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": src},
		FilterGraphOptions{Outputs: []FilterResourceID{"src"}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output("src") != src {
		t.Fatal("retained source should be the exact supplied pixmap")
	}
	if res.Origin != [2]int{0, 0} {
		t.Fatalf("Origin = %v, want (0,0)", res.Origin)
	}
	if res.Bounds.MaxX != 6 || res.Bounds.MaxY != 6 {
		t.Fatalf("Bounds = %+v, want 6x6", res.Bounds)
	}
}

// TestFilterGraphExplicitBoundsWithExpansion verifies that an explicit input
// region is still expanded by the node's op to size the output buffer.
func TestFilterGraphExplicitBoundsWithExpansion(t *testing.T) {
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "exp", Op: expandOp(2), Inputs: []FilterResourceID{"src"}, Output: "out",
		Bounds: Rect{MinX: 4, MinY: 4, MaxX: 8, MaxY: 8}})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	res, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": solidPixmap(16, 16, gg.White)},
		FilterGraphOptions{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Output bounds = explicit(4..8) expanded by 2 => 2..10, but the planner
	// also pads the negative-free region; the buffer must be at least 10 wide.
	out := res.Output("out")
	if out.Width() < 10 || out.Height() < 10 {
		t.Fatalf("output %dx%d, want at least 10x10", out.Width(), out.Height())
	}
}

// TestFilterGraphDuplicateInputSameProducer ensures one node reading the same
// producer through two inputs keeps the producer alive until it finishes.
func TestFilterGraphDuplicateInputSameProducer(t *testing.T) {
	var sawNil atomic.Bool
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "prod", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "pOut"})
	mustAdd(t, g, FilterNode{ID: "merge", Op: FilterGraphOpFunc{
		ApplyFunc: func(_ context.Context, in []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
			for _, pm := range in {
				if pm == nil {
					sawNil.Store(true)
					return errors.New("nil input")
				}
			}
			return nil
		},
	}, Inputs: []FilterResourceID{"pOut", "pOut"}, Output: "out"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": solidPixmap(4, 4, gg.White)},
		FilterGraphOptions{Workers: 4}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if sawNil.Load() {
		t.Fatal("producer freed before its duplicate-input consumer ran")
	}
}

// TestFilterGraphMixedExpansionMerge verifies that a graph merging an
// expanding branch with a non-expanding branch keeps every buffer on one
// origin, so pixel (x,y) addresses the same graph location in all inputs.
func TestFilterGraphMixedExpansionMerge(t *testing.T) {
	// src -> exp (pad 4) -> out (sink, expanding branch)
	// src -> inv         -> joined (merge with exp)
	// joined is the sink; buffers must share the global origin at -4.
	g := NewFilterGraph()
	_ = g.AddSource("src")
	mustAdd(t, g, FilterNode{ID: "exp", Op: expandOp(4), Inputs: []FilterResourceID{"src"}, Output: "expOut"})
	mustAdd(t, g, FilterNode{ID: "inv", Op: invertOp(), Inputs: []FilterResourceID{"src"}, Output: "invOut"})
	mustAdd(t, g, FilterNode{ID: "join", Op: OverOp(), Inputs: []FilterResourceID{"expOut", "invOut"}, Output: "out"})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	src := solidPixmap(10, 10, gg.Black) // opaque black; invert -> opaque red

	var first *gg.Pixmap
	for _, workers := range []int{1, 2, 3, 8} {
		res, err := cg.Execute(context.Background(), map[FilterResourceID]*gg.Pixmap{"src": src},
			FilterGraphOptions{Workers: workers})
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if res.Origin != [2]int{-4, -4} {
			t.Fatalf("workers=%d Origin = %v, want (-4,-4)", workers, res.Origin)
		}
		out := res.Output("out")
		// join composites exp (black, opaque) then inv (red, opaque): red wins
		// over the overlapping region.
		ox, oy := 4, 4 // content of the source within the padded canvas
		c := out.GetPixel(ox+5, oy+5)
		if absf(c.R-1) > 0.01 || absf(c.G) > 0.01 || absf(c.B) > 0.01 || absf(c.A-1) > 0.01 {
			t.Fatalf("workers=%d center pixel = %+v, want opaque red", workers, c)
		}
		if first == nil {
			first = out
		} else if !samePixels(first, out) {
			t.Fatalf("workers=%d pixels differ from workers=1", workers)
		}
	}
}
