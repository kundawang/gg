package scene

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync/atomic"

	"github.com/gogpu/gg"
)

// FilterGraphOptions configures one [CompiledFilterGraph.Execute] call.
type FilterGraphOptions struct {
	// Workers is the maximum number of nodes executing at once.
	// Zero selects GOMAXPROCS; negative or greater than MaxFilterGraphWorkers
	// is rejected with ErrFilterGraphWorkers.
	Workers int

	// MemoryLimit is the maximum number of live buffer bytes allowed during
	// execution. Zero selects DefaultFilterGraphMemoryLimit. A negative value
	// disables the limit.
	MemoryLimit int64

	// Outputs lists the resources to retain in the result. When empty, every
	// sink output (a node output no other node consumes) is retained.
	Outputs []FilterResourceID
}

// DefaultFilterGraphMemoryLimit is the per-execution live-buffer byte budget
// used when [FilterGraphOptions.MemoryLimit] is zero.
const DefaultFilterGraphMemoryLimit int64 = 512 << 20 // 512 MiB

// FilterGraphResult holds the retained outputs of a graph execution.
//
// All node outputs share one coordinate space. Pixel (0, 0) of every returned
// pixmap corresponds to the graph coordinate Origin, so a pixel at global
// coordinate g lives at index g-Origin. Origin is (0, 0) unless a filter
// expands bounds to the left or above the graph origin.
type FilterGraphResult struct {
	// Outputs maps each retained resource to its pixmap.
	Outputs map[FilterResourceID]*gg.Pixmap

	// Bounds is the union of the retained outputs in graph coordinates.
	Bounds Rect

	// Origin is the graph coordinate of pixel (0, 0) in every output pixmap.
	Origin [2]int

	// Order is the stable topological order the graph executed in.
	Order []FilterNodeID

	// PeakMemory is the peak number of live buffer bytes observed.
	PeakMemory int64
}

// Output returns the retained pixmap for a resource, or nil.
func (r *FilterGraphResult) Output(id FilterResourceID) *gg.Pixmap {
	if r == nil {
		return nil
	}
	return r.Outputs[id]
}

// Execute runs the compiled graph against the supplied sources.
//
// Every source referenced by a node must be present in sources; a missing one
// fails with ErrFilterGraphMissingInput and the consuming node's edge context.
// Execution is all-or-nothing: on any error the result is nil, all in-flight
// nodes are canceled, and every temporary buffer is returned.
//
// Nodes that are simultaneously ready run concurrently, up to
// FilterGraphOptions.Workers. Results are byte-identical for every positive
// worker count because a node only reads its declared inputs and writes its
// own private buffer.
func (cg *CompiledFilterGraph) Execute(ctx context.Context, sources map[FilterResourceID]*gg.Pixmap, opts FilterGraphOptions) (*FilterGraphResult, error) {
	if cg == nil {
		return nil, newGraphError("execute", ErrFilterGraphInvalidDefinition, "", "", "", "nil compiled graph", nil)
	}
	workers, err := resolveGraphWorkers(opts.Workers)
	if err != nil {
		return nil, err
	}
	limit := opts.MemoryLimit
	if limit == 0 {
		limit = DefaultFilterGraphMemoryLimit
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, newGraphError("execute", ErrFilterGraphCanceled, "", "", "", "context canceled before execution", err)
	}

	plan, err := cg.plan(sources)
	if err != nil {
		return nil, err
	}

	retain, retainedSources, err := cg.resolveRetained(opts.Outputs)
	if err != nil {
		return nil, err
	}
	if err := validateRetainedSources(sources, retainedSources); err != nil {
		return nil, err
	}
	if err := cg.chargeShiftedSources(plan, limit); err != nil {
		return nil, err
	}

	run := &graphRun{
		cg:          cg,
		sources:     sources,
		applyBounds: plan.applyBounds,
		outBounds:   plan.outBounds,
		pix:         make([]*gg.Pixmap, len(cg.nodes)),
		shifted:     plan.shifted,
		retain:      retain,
		limit:       limit,
		pool:        cg.pool,
	}
	for i := range cg.nodes {
		refs := int32(cg.nodes[i].consumers)
		if retain[i] {
			refs++
		}
		run.refs = append(run.refs, refs)
	}

	if err := run.execute(ctx, workers); err != nil {
		return nil, err
	}

	out := &FilterGraphResult{
		Outputs:    make(map[FilterResourceID]*gg.Pixmap, len(retain)+len(retainedSources)),
		Origin:     [2]int{plan.originX, plan.originY},
		Order:      cg.NodeOrder(),
		PeakMemory: run.peak.Load(),
	}
	var bounds Rect
	set := false
	for i := range cg.nodes {
		if !retain[i] {
			continue
		}
		out.Outputs[cg.nodes[i].output] = run.pix[i]
		bounds = unionInto(bounds, plan.toGraph(plan.outBounds[i]), &set)
	}
	for id := range retainedSources {
		pm := plan.sourcePixmap(id, sources)
		if pm == nil {
			return nil, newGraphError("execute", ErrFilterGraphSource, "", id, "",
				"requested source output was not supplied", nil)
		}
		out.Outputs[id] = pm
		bounds = unionInto(bounds, plan.toGraph(plan.retainedSourceBounds(id)), &set)
	}
	if !set {
		bounds = EmptyRect()
	}
	out.Bounds = bounds
	return out, nil
}

func unionInto(acc, r Rect, set *bool) Rect {
	if *set {
		return acc.Union(r)
	}
	*set = true
	return r
}

// validateRetainedSources rejects a request that retains a declared source
// that was not supplied, before any execution begins.
func validateRetainedSources(sources map[FilterResourceID]*gg.Pixmap, retained map[FilterResourceID]struct{}) error {
	for id := range retained {
		if sources[id] == nil {
			return newGraphError("execute", ErrFilterGraphSource, "", id, "",
				"requested source output was not supplied", nil)
		}
	}
	return nil
}

// chargeShiftedSources verifies that the permanently-live origin-aligned
// source copies fit within the memory limit before any node allocates.
func (cg *CompiledFilterGraph) chargeShiftedSources(p *graphPlan, limit int64) error {
	if limit < 0 {
		return nil
	}
	var total int64
	for _, pm := range p.shifted {
		total += pixmapBytes(pm)
	}
	if total > limit {
		return newGraphError("execute", ErrFilterGraphMemoryLimit, "", "",
			"", fmt.Sprintf("origin-aligned sources need %d bytes, limit %d", total, limit), nil)
	}
	return nil
}

// graphPlan is the immutable per-execution geometry computed before running.
type graphPlan struct {
	// applyBounds and outBounds are in origin-local coordinates.
	applyBounds []Rect
	outBounds   []Rect

	// srcBounds maps each source to its bounds in origin-local coordinates.
	srcBounds map[FilterResourceID]Rect

	originX, originY int
	padX, padY       int

	// shifted holds origin-aligned copies of sources when the origin moved.
	shifted map[FilterResourceID]*gg.Pixmap
}

// retainedSourceBounds returns the origin-local bounds for a retained source.
// The bounds are always content bounds; when the origin moved, the retained
// pixmap is an origin-aligned copy that carries transparent padding before
// the content.
func (p *graphPlan) retainedSourceBounds(id FilterResourceID) Rect {
	return p.srcBounds[id]
}

// toGraph converts origin-local bounds back to graph coordinates.
func (p *graphPlan) toGraph(r Rect) Rect {
	if r.IsEmpty() {
		return r
	}
	return translateRect(r, p.originX, p.originY)
}

// sourcePixmap returns the origin-aligned source pixmap for the plan.
func (p *graphPlan) sourcePixmap(id FilterResourceID, sources map[FilterResourceID]*gg.Pixmap) *gg.Pixmap {
	if pm, ok := p.shifted[id]; ok {
		return pm
	}
	return sources[id]
}

// plan validates sources, propagates bounds in topological order, picks a
// global origin so negative expansions are preserved, and localizes all
// bounds for buffer indexing.
func (cg *CompiledFilterGraph) plan(sources map[FilterResourceID]*gg.Pixmap) (*graphPlan, error) {
	p := &graphPlan{
		applyBounds: make([]Rect, len(cg.nodes)),
		outBounds:   make([]Rect, len(cg.nodes)),
		srcBounds:   make(map[FilterResourceID]Rect, len(cg.sources)),
	}
	if err := p.collectSourceBounds(cg, sources); err != nil {
		return nil, err
	}
	if err := cg.propagateBounds(p); err != nil {
		return nil, err
	}
	if err := cg.assignOrigin(p); err != nil {
		return nil, err
	}
	cg.alignSources(p, sources)
	return p, nil
}

// collectSourceBounds validates the supplied sources and records their bounds.
// Optional sources (not consumed by any node) are recorded only when present.
func (p *graphPlan) collectSourceBounds(cg *CompiledFilterGraph, sources map[FilterResourceID]*gg.Pixmap) error {
	for _, id := range cg.sources {
		pm := sources[id]
		required := cg.sourceRequired(id)
		if pm == nil {
			if !required {
				continue
			}
			node, edge := cg.inputConsumer(id)
			return newGraphError("execute", ErrFilterGraphMissingInput,
				node, id, edge, "required source pixmap was not supplied", nil)
		}
		if pm.Width() <= 0 || pm.Height() <= 0 {
			if !required {
				continue
			}
			return newGraphError("execute", ErrFilterGraphSource, "", id, "",
				fmt.Sprintf("source has invalid size %dx%d", pm.Width(), pm.Height()), nil)
		}
		p.srcBounds[id] = Rect{MinX: 0, MinY: 0, MaxX: float32(pm.Width()), MaxY: float32(pm.Height())}
	}
	return nil
}

// propagateBounds computes every node's input and output bounds by walking the
// stable topological order.
func (cg *CompiledFilterGraph) propagateBounds(p *graphPlan) error {
	resBounds := make(map[FilterResourceID]Rect, len(cg.nodes)+len(cg.sources))
	for id, b := range p.srcBounds {
		resBounds[id] = b
	}
	for _, idx := range cg.order {
		n := &cg.nodes[idx]
		inB := make([]Rect, len(n.inputs))
		for j, in := range n.inputs {
			inB[j] = resBounds[in]
		}
		base := unionRects(inB)
		if n.hasExplicit {
			base = n.explicitBounds
		}
		if err := validateGraphBounds(base, "node input bounds", n); err != nil {
			return err
		}
		out := n.op.ExpandBounds(effectiveInputBounds(inB, n))
		if err := validateGraphBounds(out, "node output bounds", n); err != nil {
			return err
		}
		p.applyBounds[idx] = base
		p.outBounds[idx] = out
		resBounds[n.output] = out
	}
	return nil
}

// assignOrigin shifts all bounds so the most negative coordinate becomes 0,
// preserving left/top expansion, then validates each localized buffer size.
func (cg *CompiledFilterGraph) assignOrigin(p *graphPlan) error {
	originX, originY := 0, 0
	consider := func(r Rect) {
		if r.IsEmpty() {
			return
		}
		if fx := int(math.Floor(float64(r.MinX))); fx < originX {
			originX = fx
		}
		if fy := int(math.Floor(float64(r.MinY))); fy < originY {
			originY = fy
		}
	}
	for _, r := range p.srcBounds {
		consider(r)
	}
	for i := range cg.nodes {
		consider(p.applyBounds[i])
		consider(p.outBounds[i])
	}

	// Bound the padded canvas so a pathological blur cannot demand an
	// unbounded buffer.
	if originX < -maxFilterGraphDimension || originY < -maxFilterGraphDimension {
		return newGraphError("execute", ErrFilterGraphBounds, "", "", "",
			fmt.Sprintf("expanded origin (%d, %d) exceeds maximum dimension %d",
				originX, originY, maxFilterGraphDimension), nil)
	}
	p.originX, p.originY = originX, originY
	p.padX = -originX
	p.padY = -originY

	for i := range cg.nodes {
		p.applyBounds[i] = translateRect(p.applyBounds[i], -originX, -originY)
		p.outBounds[i] = translateRect(p.outBounds[i], -originX, -originY)
		if err := validateBufferDims(p.outBounds[i], cg.nodes[i].id); err != nil {
			return err
		}
	}
	return nil
}

// alignSources builds origin-aligned copies of every supplied source when the
// origin moved. Sources that were not supplied are skipped; Execute re-checks
// them only if they are explicitly requested as outputs.
func (cg *CompiledFilterGraph) alignSources(p *graphPlan, sources map[FilterResourceID]*gg.Pixmap) {
	if p.padX <= 0 && p.padY <= 0 {
		return
	}
	p.shifted = make(map[FilterResourceID]*gg.Pixmap, len(p.srcBounds))
	for id := range p.srcBounds {
		src := sources[id]
		if src == nil {
			continue
		}
		p.shifted[id] = shiftSource(src, p.padX, p.padY)
		p.srcBounds[id] = translateRect(p.srcBounds[id], p.padX, p.padY)
	}
}

// translateRect offsets a rect by integer deltas.
func translateRect(r Rect, dx, dy int) Rect {
	if r.IsEmpty() {
		return r
	}
	return Rect{
		MinX: r.MinX + float32(dx),
		MinY: r.MinY + float32(dy),
		MaxX: r.MaxX + float32(dx),
		MaxY: r.MaxY + float32(dy),
	}
}

// shiftSource returns an origin-aligned copy of src with transparent padding
// on the left and top.
func shiftSource(src *gg.Pixmap, padX, padY int) *gg.Pixmap {
	w := src.Width() + padX
	h := src.Height() + padY
	dst := gg.NewPixmap(w, h)
	srcData := src.Data()
	dstData := dst.Data()
	srcW := src.Width()
	for y := 0; y < src.Height(); y++ {
		di := ((y+padY)*w + padX) * 4
		si := y * srcW * 4
		copy(dstData[di:di+srcW*4], srcData[si:si+srcW*4])
	}
	return dst
}

// effectiveInputBounds substitutes explicit node bounds for every input.
func effectiveInputBounds(in []Rect, n *compiledFilterNode) []Rect {
	if !n.hasExplicit {
		return in
	}
	out := make([]Rect, len(in))
	for i := range out {
		out[i] = n.explicitBounds
	}
	return out
}

// validateGraphBounds rejects non-finite or oversized derived bounds and
// attaches node context to the error.
func validateGraphBounds(r Rect, what string, n *compiledFilterNode) error {
	if err := validateGraphRect(r, what); err != nil {
		return newGraphError("execute", ErrFilterGraphBounds, n.id, n.output, "", err.Error(), err)
	}
	return nil
}

// validateBufferDims checks that localized output bounds fit a buffer.
func validateBufferDims(b Rect, node FilterNodeID) error {
	if b.IsEmpty() {
		return nil
	}
	w, h := int(math.Ceil(float64(b.MaxX))), int(math.Ceil(float64(b.MaxY)))
	if w > maxFilterGraphDimension || h > maxFilterGraphDimension {
		return newGraphError("execute", ErrFilterGraphBounds, node, "",
			"", fmt.Sprintf("buffer %dx%d exceeds maximum dimension %d", w, h, maxFilterGraphDimension), nil)
	}
	return nil
}

// resolveRetained computes which node outputs to keep and which extra sources
// to expose in the result.
func (cg *CompiledFilterGraph) resolveRetained(requested []FilterResourceID) ([]bool, map[FilterResourceID]struct{}, error) {
	retain := make([]bool, len(cg.nodes))
	extra := make(map[FilterResourceID]struct{})
	if len(requested) == 0 {
		for i := range cg.nodes {
			if cg.nodes[i].consumers == 0 {
				retain[i] = true
			}
		}
		return retain, extra, nil
	}
	for _, id := range requested {
		if idx, ok := cg.outputs[id]; ok {
			retain[idx] = true
			continue
		}
		if cg.hasSource(id) {
			extra[id] = struct{}{}
			continue
		}
		return nil, nil, newGraphError("execute", ErrFilterGraphMissingInput,
			"", id, "", "requested output is neither a node output nor a declared source", nil)
	}
	return retain, extra, nil
}

// sourceRequired reports whether any node consumes the source.
func (cg *CompiledFilterGraph) sourceRequired(id FilterResourceID) bool {
	for i := range cg.nodes {
		for _, in := range cg.nodes[i].inputs {
			if in == id {
				return true
			}
		}
	}
	return false
}

// hasSource reports whether id is a declared source.
func (cg *CompiledFilterGraph) hasSource(id FilterResourceID) bool {
	for _, s := range cg.sources {
		if s == id {
			return true
		}
	}
	return false
}

// inputConsumer returns the first node and edge that consumes the resource.
func (cg *CompiledFilterGraph) inputConsumer(id FilterResourceID) (FilterNodeID, string) {
	for i := range cg.nodes {
		for _, in := range cg.nodes[i].inputs {
			if in == id {
				return cg.nodes[i].id, fmt.Sprintf("%s -> %s", in, cg.nodes[i].id)
			}
		}
	}
	return "", ""
}

// resolveGraphWorkers validates the worker count.
func resolveGraphWorkers(w int) (int, error) {
	if w == 0 {
		w = runtime.GOMAXPROCS(0)
		if w <= 0 {
			w = 1
		}
		if w > MaxFilterGraphWorkers {
			w = MaxFilterGraphWorkers
		}
	}
	if w < 0 || w > MaxFilterGraphWorkers {
		return 0, newGraphError("execute", ErrFilterGraphWorkers, "", "",
			"", fmt.Sprintf("worker count %d outside [1, %d]", w, MaxFilterGraphWorkers), nil)
	}
	return w, nil
}

// graphRun holds mutable execution state for a single Execute call.
type graphRun struct {
	cg          *CompiledFilterGraph
	sources     map[FilterResourceID]*gg.Pixmap
	applyBounds []Rect
	outBounds   []Rect
	pix         []*gg.Pixmap
	refs        []int32
	shifted     map[FilterResourceID]*gg.Pixmap
	retain      []bool
	limit       int64
	pool        *filterGraphPool

	live atomic.Int64
	peak atomic.Int64

	errs []error // written by each node's goroutine before it reports on doneCh
}

// execute runs the graph with a dependency-driven ready queue. A node starts
// as soon as all of its producers have completed and a worker slot is free;
// dispatch order among simultaneously-ready nodes is declaration order, so the
// execution order is deterministic. Results are independent of the worker
// count because a node reads only its declared inputs and writes its own
// private buffer.
//
// The first failure cancels the shared context, stops new dispatches, waits
// for in-flight nodes, returns every live buffer to the pool, and reports the
// most specific error. No partial result is produced.
func (r *graphRun) execute(parent context.Context, workers int) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	n := len(r.cg.nodes)
	r.errs = make([]error, n)

	// Charge the origin-aligned source copies up front.
	for _, pm := range r.shifted {
		atomicMax64(&r.peak, r.live.Add(pixmapBytes(pm)))
	}
	defer func() {
		for _, pm := range r.shifted {
			r.live.Add(-pixmapBytes(pm))
		}
	}()

	aborted, completed := r.dispatch(ctx, cancel, workers)

	switch {
	case aborted:
		r.releaseAll()
		return r.firstError()
	case completed != n:
		r.releaseAll()
		return newGraphError("execute", ErrFilterGraphCycle, "", "", "", "no runnable node remains", nil)
	}

	// Any output that is neither consumed nor retained is dead on success;
	// return it to the pool so repeated executions stay allocation-light.
	for i := range r.pix {
		if r.pix[i] != nil && !r.retain[i] {
			r.freeBuffer(i)
		}
	}
	return nil
}

// dispatch runs the ready-queue loop, starting nodes as soon as a producer
// completes and a worker slot is free. It reports whether execution aborted
// and how many nodes completed.
func (r *graphRun) dispatch(ctx context.Context, cancel context.CancelFunc, workers int) (aborted bool, completed int) {
	n := len(r.cg.nodes)
	indeg := make([]int, n)
	ready := make([]int, 0, n)
	for i := range r.cg.nodes {
		for _, p := range r.cg.nodes[i].producers {
			if p >= 0 {
				indeg[i]++
			}
		}
	}
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			ready = insertSortedInt(ready, i)
		}
	}

	doneCh := make(chan int, n)
	running := 0

	for {
		// Dispatch every ready node that fits in the worker budget.
		for !aborted && len(ready) > 0 && running < workers {
			idx := ready[0]
			ready = ready[1:]
			running++
			go func(idx int) {
				defer func() { doneCh <- idx }()
				r.runNode(ctx, idx)
			}(idx)
		}
		if running == 0 {
			return aborted, completed
		}

		idx := <-doneCh
		running--
		completed++
		if r.errs[idx] != nil && !aborted {
			aborted = true
			cancel()
		}
		ready = r.releaseConsumers(idx, indeg, ready)
	}
}

// releaseConsumers decrements the in-degree of every consumer of idx and
// returns the ready queue updated with any that became runnable.
func (r *graphRun) releaseConsumers(idx int, indeg []int, ready []int) []int {
	for j := range r.cg.nodes {
		for _, p := range r.cg.nodes[j].producers {
			if p != idx {
				continue
			}
			indeg[j]--
			if indeg[j] == 0 {
				ready = insertSortedInt(ready, j)
			}
		}
	}
	return ready
}

// firstError selects the most specific node error in declaration order,
// preferring real failures over the cancellations they induce so that the
// reported cause is the one that actually started the abort.
func (r *graphRun) firstError() error {
	var fallback error
	for _, err := range r.errs {
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrFilterGraphCanceled) {
			return err
		}
		if fallback == nil {
			fallback = err
		}
	}
	if fallback != nil {
		return fallback
	}
	return newGraphError("execute", ErrFilterGraphCanceled, "", "", "", "graph canceled", nil)
}

// releaseAll returns every still-live node buffer to the pool. It runs on the
// failure path so a failed execution never leaks pooled or heap memory.
func (r *graphRun) releaseAll() {
	for i := range r.pix {
		if r.pix[i] != nil {
			r.freeBuffer(i)
		}
	}
}

// runNode executes one node, captures panics, and releases consumed inputs.
func (r *graphRun) runNode(ctx context.Context, idx int) {
	n := &r.cg.nodes[idx]
	if err := r.executeNode(ctx, idx); err != nil {
		r.errs[idx] = r.wrapNodeError(n, err)
		return
	}
	r.releaseInputs(n)
}

// wrapNodeError annotates a node failure with node, output and input edge
// context without losing the original cause.
func (r *graphRun) wrapNodeError(n *compiledFilterNode, err error) error {
	kind := ErrFilterGraphNode
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		kind = ErrFilterGraphCanceled
	}
	edge := ""
	if len(n.inputs) > 0 {
		edge = fmt.Sprintf("%s -> %s", n.inputs[0], n.id)
	}
	return newGraphError("execute", kind, n.id, n.output, edge, "node failed", err)
}

// executeNode renders a single node into its private buffer.
//
// Each node gets its own derived context so a filter that inspects
// ctx.Done()/ctx.Err() observes only its own cancellation, while the parent
// graph context still propagates failures and external cancellation.
func (r *graphRun) executeNode(parent context.Context, idx int) (err error) {
	nodeCtx, cancel := context.WithCancel(parent)
	defer cancel()

	n := &r.cg.nodes[idx]
	if err := nodeCtx.Err(); err != nil {
		return err
	}

	w, h := bufferDims(r.outBounds[idx])
	pm, err := r.allocPixmap(w, h, n)
	if err != nil {
		return err
	}
	r.pix[idx] = pm

	inputs := make([]*gg.Pixmap, len(n.inputs))
	for j, p := range n.producers {
		if p < 0 {
			inputs[j] = r.sourcePixmap(n.inputs[j])
			continue
		}
		inputs[j] = r.pix[p]
		if inputs[j] == nil {
			return fmt.Errorf("producer %q released its buffer early", r.cg.nodes[p].id)
		}
	}

	bounds := clipRectToBuffer(r.applyBounds[idx], w, h)

	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	if err := n.op.Apply(nodeCtx, inputs, pm, bounds); err != nil {
		return err
	}
	return nodeCtx.Err()
}

// sourcePixmap returns the origin-aligned source for a node input.
func (r *graphRun) sourcePixmap(id FilterResourceID) *gg.Pixmap {
	if pm, ok := r.shifted[id]; ok {
		return pm
	}
	return r.sources[id]
}

// allocPixmap acquires a buffer and charges its bytes against the limit.
func (r *graphRun) allocPixmap(w, h int, n *compiledFilterNode) (*gg.Pixmap, error) {
	bytes := int64(w) * int64(h) * 4
	if r.limit >= 0 {
		newLive := r.live.Add(bytes)
		atomicMax64(&r.peak, newLive)
		if newLive > r.limit {
			r.live.Add(-bytes)
			return nil, newGraphError("execute", ErrFilterGraphMemoryLimit, n.id, n.output, "",
				fmt.Sprintf("live buffers would reach %d bytes, limit %d", newLive, r.limit), nil)
		}
	} else {
		atomicMax64(&r.peak, r.live.Add(bytes))
	}
	return r.pool.get(w, h), nil
}

// releaseInputs decrements producer reference counts and frees buffers that
// are no longer needed.
func (r *graphRun) releaseInputs(n *compiledFilterNode) {
	seen := make(map[int]struct{}, len(n.producers))
	for _, p := range n.producers {
		if p < 0 {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		if atomic.AddInt32(&r.refs[p], -1) == 0 {
			r.freeBuffer(p)
		}
	}
}

// freeBuffer returns a non-retained node buffer to the pool.
func (r *graphRun) freeBuffer(idx int) {
	pm := r.pix[idx]
	if pm == nil {
		return
	}
	r.pix[idx] = nil
	r.live.Add(-pixmapBytes(pm))
	r.pool.put(pm)
}

// pixmapBytes returns the byte size of a pixmap's pixel buffer.
func pixmapBytes(pm *gg.Pixmap) int64 {
	if pm == nil {
		return 0
	}
	return int64(pm.Width()) * int64(pm.Height()) * 4
}

// bufferDims converts localized output bounds to buffer dimensions.
func bufferDims(b Rect) (int, int) {
	if b.IsEmpty() {
		return 1, 1
	}
	w := int(math.Ceil(float64(b.MaxX)))
	h := int(math.Ceil(float64(b.MaxY)))
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}

// clipRectToBuffer intersects bounds with the buffer's pixel rectangle.
func clipRectToBuffer(b Rect, w, h int) Rect {
	if b.IsEmpty() {
		return EmptyRect()
	}
	clipped := Rect{
		MinX: max32(b.MinX, 0),
		MinY: max32(b.MinY, 0),
		MaxX: min32(b.MaxX, float32(w)),
		MaxY: min32(b.MaxY, float32(h)),
	}
	if clipped.IsEmpty() {
		return EmptyRect()
	}
	return clipped
}

// compositePremultipliedOver composites src over dst within bounds using
// premultiplied source-over: out = src + dst*(1 - srcA).
func compositePremultipliedOver(src, dst *gg.Pixmap, bounds Rect) {
	if src == nil || dst == nil || bounds.IsEmpty() {
		return
	}
	minX := clampGraphCoord(int(math.Floor(float64(bounds.MinX))), dst.Width())
	maxX := clampGraphCoord(int(math.Ceil(float64(bounds.MaxX))), dst.Width())
	minY := clampGraphCoord(int(math.Floor(float64(bounds.MinY))), dst.Height())
	maxY := clampGraphCoord(int(math.Ceil(float64(bounds.MaxY))), dst.Height())

	srcData := src.Data()
	dstData := dst.Data()
	srcW := src.Width()
	srcH := src.Height()
	dstW := dst.Width()

	for y := minY; y < maxY; y++ {
		if y >= srcH {
			continue
		}
		for x := minX; x < maxX; x++ {
			if x >= srcW {
				continue
			}
			si := (y*srcW + x) * 4
			di := (y*dstW + x) * 4

			sa := float64(srcData[si+3]) / 255
			inv := 1 - sa
			dstData[di+0] = clampGraphByte(float64(srcData[si+0]) + float64(dstData[di+0])*inv)
			dstData[di+1] = clampGraphByte(float64(srcData[si+1]) + float64(dstData[di+1])*inv)
			dstData[di+2] = clampGraphByte(float64(srcData[si+2]) + float64(dstData[di+2])*inv)
			dstData[di+3] = clampGraphByte(float64(srcData[si+3]) + float64(dstData[di+3])*inv)
		}
	}
}

// clampGraphCoord clamps v to [0, hi].
func clampGraphCoord(v, hi int) int {
	if v < 0 {
		return 0
	}
	if v > hi {
		return hi
	}
	return v
}

// clampGraphByte clamps v to [0, 255] and rounds to nearest.
func clampGraphByte(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// atomicMax64 stores v if larger than the current value.
func atomicMax64(dst *atomic.Int64, v int64) {
	for {
		cur := dst.Load()
		if v <= cur || dst.CompareAndSwap(cur, v) {
			return
		}
	}
}
