package scene

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/gogpu/gg"
)

// FilterGraph sentinel errors. Every graph failure wraps one of these so that
// callers can classify it with errors.Is while still receiving the node name,
// resource and edge context carried by [FilterGraphError].
var (
	// ErrFilterGraphInvalidDefinition reports a malformed graph definition:
	// empty identifiers, nil operations, duplicate node IDs, and so on.
	ErrFilterGraphInvalidDefinition = errors.New("filter graph: invalid definition")

	// ErrFilterGraphMissingInput reports a node input that is neither a
	// declared source nor produced by another node, or a required source
	// pixmap that was not supplied at execution time.
	ErrFilterGraphMissingInput = errors.New("filter graph: missing input resource")

	// ErrFilterGraphDuplicateOutput reports two nodes producing the same
	// resource name, or a node output clashing with a declared source.
	ErrFilterGraphDuplicateOutput = errors.New("filter graph: duplicate output resource")

	// ErrFilterGraphCycle reports a dependency cycle.
	ErrFilterGraphCycle = errors.New("filter graph: cycle detected")

	// ErrFilterGraphBounds reports non-finite, inverted or unrepresentably
	// large bounds.
	ErrFilterGraphBounds = errors.New("filter graph: invalid bounds")

	// ErrFilterGraphWorkers reports a worker count outside the supported range.
	ErrFilterGraphWorkers = errors.New("filter graph: invalid worker count")

	// ErrFilterGraphMemoryLimit reports that the planned buffers would exceed
	// the configured memory limit. The limit is enforced before and during
	// allocation, so no partial result is returned.
	ErrFilterGraphMemoryLimit = errors.New("filter graph: memory limit exceeded")

	// ErrFilterGraphNode reports a filter that returned an error or panicked.
	ErrFilterGraphNode = errors.New("filter graph: node execution failed")

	// ErrFilterGraphSource reports a missing or unusable source pixmap.
	ErrFilterGraphSource = errors.New("filter graph: invalid source")

	// ErrFilterGraphCanceled reports cancellation of an execution, whether by
	// the caller's context or because a sibling node failed.
	ErrFilterGraphCanceled = errors.New("filter graph: canceled")
)

// MaxFilterGraphWorkers is the largest accepted worker count for a single
// graph execution. Anything above it is rejected with ErrFilterGraphWorkers
// instead of spawning an unreasonable number of goroutines.
const MaxFilterGraphWorkers = 256

// maxFilterGraphDimension caps a single buffer dimension. It keeps width and
// height within int range on 32-bit platforms and prevents accidental
// multi-terabyte allocations from bogus bounds.
const maxFilterGraphDimension = 1 << 26

// FilterNodeID identifies a node inside a [FilterGraph].
type FilterNodeID string

// FilterResourceID identifies an image resource flowing between graph nodes.
// Resource names share a single namespace: a name is either a declared source
// or the output of exactly one node.
type FilterResourceID string

// FilterGraphError describes a graph definition or execution failure.
// It always carries the operation and a sentinel Kind, and adds node,
// resource and edge context whenever one is available.
type FilterGraphError struct {
	// Op is "compile" or "execute".
	Op string

	// Kind is one of the ErrFilterGraph* sentinels.
	Kind error

	// Node is the node the failure is attributed to, if any.
	Node FilterNodeID

	// Resource is the resource involved, if any.
	Resource FilterResourceID

	// Edge describes the dependency involved, e.g. "a -> merge".
	Edge string

	// Detail is a human-readable explanation.
	Detail string

	// Err is the underlying cause (a filter error, a panic, ...).
	Err error
}

// Error implements the error interface.
func (e *FilterGraphError) Error() string {
	var b strings.Builder
	b.WriteString("filter graph")
	if e.Op != "" {
		b.WriteString(" ")
		b.WriteString(e.Op)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Node != "" {
		b.WriteString(" [node ")
		b.WriteString(string(e.Node))
		b.WriteString("]")
	}
	if e.Resource != "" {
		b.WriteString(" [resource ")
		b.WriteString(string(e.Resource))
		b.WriteString("]")
	}
	if e.Edge != "" {
		b.WriteString(" [edge ")
		b.WriteString(e.Edge)
		b.WriteString("]")
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes both the sentinel kind and the underlying cause so that
// errors.Is and errors.As can reach either.
func (e *FilterGraphError) Unwrap() []error {
	out := make([]error, 0, 2)
	if e.Kind != nil {
		out = append(out, e.Kind)
	}
	if e.Err != nil && !errors.Is(e.Err, e.Kind) {
		out = append(out, e.Err)
	}
	return out
}

func newGraphError(op string, kind error, node FilterNodeID, res FilterResourceID, edge, detail string, cause error) *FilterGraphError {
	return &FilterGraphError{
		Op:       op,
		Kind:     kind,
		Node:     node,
		Resource: res,
		Edge:     edge,
		Detail:   detail,
		Err:      cause,
	}
}

// FilterGraphOp is the unit of work executed by a [FilterGraph] node.
//
// Implementations receive their inputs in the order declared by
// [FilterNode.Inputs]. All pixmaps are premultiplied RGBA and share one
// coordinate space: pixel (0, 0) of every buffer is the graph origin, and
// buffers only grow toward positive coordinates. Implementations must not
// retain references to inputs or dst after Apply returns.
//
// ExpandBounds must be pure and deterministic: the scheduler calls it during
// planning to size buffers and to propagate bounds down the graph.
type FilterGraphOp interface {
	// ExpandBounds returns the output bounds given the ordered input bounds.
	// It may be called with zero inputs, which must yield an empty rect.
	ExpandBounds(inputs []Rect) Rect

	// Apply renders the inputs into dst, touching only pixels inside bounds.
	// Returning an error fails the whole graph; the returned result is then
	// nil and every in-flight node is canceled.
	Apply(ctx context.Context, inputs []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error
}

// FilterOp adapts a single-input [Filter] to a graph operation.
// The filter is applied to the first declared input.
type FilterOp struct {
	Filter Filter
}

// ExpandBounds implements [FilterGraphOp].
func (o FilterOp) ExpandBounds(inputs []Rect) Rect {
	if o.Filter == nil || len(inputs) == 0 {
		return EmptyRect()
	}
	return o.Filter.ExpandBounds(inputs[0])
}

// Apply implements [FilterGraphOp].
func (o FilterOp) Apply(ctx context.Context, inputs []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
	if o.Filter == nil {
		return errors.New("filter op: nil filter")
	}
	if len(inputs) == 0 || inputs[0] == nil {
		return errors.New("filter op: missing input pixmap")
	}
	if dst == nil {
		return errors.New("filter op: missing destination pixmap")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o.Filter.Apply(inputs[0], dst, bounds)
	return ctx.Err()
}

// FilterGraphOpFunc adapts functions to [FilterGraphOp], the simplest way to
// build custom branching or merging nodes.
type FilterGraphOpFunc struct {
	// BoundsFunc computes the output bounds. When nil, the union of the input
	// bounds is used.
	BoundsFunc func(inputs []Rect) Rect

	// ApplyFunc renders the inputs. It is required.
	ApplyFunc func(ctx context.Context, inputs []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error
}

// ExpandBounds implements [FilterGraphOp].
func (o FilterGraphOpFunc) ExpandBounds(inputs []Rect) Rect {
	if o.BoundsFunc != nil {
		return o.BoundsFunc(inputs)
	}
	return unionRects(inputs)
}

// Apply implements [FilterGraphOp].
func (o FilterGraphOpFunc) Apply(ctx context.Context, inputs []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
	if o.ApplyFunc == nil {
		return errors.New("filter op func: nil ApplyFunc")
	}
	return o.ApplyFunc(ctx, inputs, dst, bounds)
}

// OverOp composites all of its inputs in declaration order using
// premultiplied source-over, and keeps the union of the input bounds. Use it
// to merge parallel branches into a single resource.
//
// The scheduler guarantees every input and the destination share one origin,
// so compositing a left/top-expanded branch with a non-expanded branch aligns
// their pixels correctly without per-op offset math.
func OverOp() FilterGraphOp { return overOp{} }

type overOp struct{}

func (overOp) ExpandBounds(inputs []Rect) Rect { return unionRects(inputs) }

func (overOp) Apply(ctx context.Context, inputs []*gg.Pixmap, dst *gg.Pixmap, bounds Rect) error {
	if dst == nil {
		return errors.New("over op: missing destination pixmap")
	}
	for _, src := range inputs {
		if src == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		compositePremultipliedOver(src, dst, bounds)
	}
	return ctx.Err()
}

// FilterNode declares one node of a [FilterGraph].
type FilterNode struct {
	// ID is the unique node name used in errors and ordering reports.
	ID FilterNodeID

	// Op is the work to perform. Required.
	Op FilterGraphOp

	// Inputs lists the resources consumed by the node, in Op input order.
	// At least one input is required.
	Inputs []FilterResourceID

	// Output is the resource produced by the node. It must be unique in the
	// graph and must not collide with a declared source.
	Output FilterResourceID

	// Bounds optionally overrides the region processed by this node. When it
	// is the zero Rect, the node processes the union of its input bounds.
	// Explicit bounds let a node depend on a region that is larger or smaller
	// than its inputs; Op.ExpandBounds then derives the output bounds.
	Bounds Rect
}

// hasExplicitBounds reports whether n.Bounds was set; the zero Rect means
// "derive from inputs".
func (n FilterNode) hasExplicitBounds() bool {
	return n.Bounds != (Rect{})
}

// FilterGraph is a declarative DAG of filter operations.
//
// Sources and nodes are appended in declaration order. [FilterGraph.Compile]
// validates the graph (missing inputs, duplicate outputs, cycles, bounds) and
// produces an immutable [CompiledFilterGraph] that can be executed repeatedly
// and concurrently with different worker counts.
type FilterGraph struct {
	sources     map[FilterResourceID]struct{}
	sourceOrder []FilterResourceID
	nodes       []FilterNode
	nodeIDs     map[FilterNodeID]struct{}
	outputs     map[FilterResourceID]FilterNodeID
}

// NewFilterGraph creates an empty filter graph.
func NewFilterGraph() *FilterGraph {
	return &FilterGraph{
		sources: make(map[FilterResourceID]struct{}),
		nodeIDs: make(map[FilterNodeID]struct{}),
		outputs: make(map[FilterResourceID]FilterNodeID),
	}
}

// AddSource declares an externally supplied input resource.
// Sources may be declared before or after the nodes that consume them, but
// always before [FilterGraph.Compile].
func (g *FilterGraph) AddSource(id FilterResourceID) error {
	if id == "" {
		return newGraphError("compile", ErrFilterGraphInvalidDefinition,
			"", "", "", "empty source resource name", nil)
	}
	if _, ok := g.sources[id]; ok {
		return newGraphError("compile", ErrFilterGraphInvalidDefinition,
			"", id, "", "source declared more than once", nil)
	}
	if owner, ok := g.outputs[id]; ok {
		return newGraphError("compile", ErrFilterGraphDuplicateOutput,
			owner, id, "", "source name already used as a node output", nil)
	}
	g.sources[id] = struct{}{}
	g.sourceOrder = append(g.sourceOrder, id)
	return nil
}

// AddNode appends a node to the graph and performs immediate structural
// validation. Cross-node validation (missing resources, cycles) happens in
// [FilterGraph.Compile].
func (g *FilterGraph) AddNode(n FilterNode) error {
	if n.ID == "" {
		return newGraphError("compile", ErrFilterGraphInvalidDefinition,
			"", "", "", "empty node ID", nil)
	}
	if n.Op == nil {
		return newGraphError("compile", ErrFilterGraphInvalidDefinition,
			n.ID, "", "", "node has nil Op", nil)
	}
	if len(n.Inputs) == 0 {
		return newGraphError("compile", ErrFilterGraphInvalidDefinition,
			n.ID, "", "", "node declares no inputs", nil)
	}
	if n.Output == "" {
		return newGraphError("compile", ErrFilterGraphInvalidDefinition,
			n.ID, "", "", "node declares no output", nil)
	}
	if _, ok := g.nodeIDs[n.ID]; ok {
		return newGraphError("compile", ErrFilterGraphInvalidDefinition,
			n.ID, "", "", "duplicate node ID", nil)
	}
	if owner, ok := g.outputs[n.Output]; ok {
		return newGraphError("compile", ErrFilterGraphDuplicateOutput,
			n.ID, n.Output, fmt.Sprintf("%s -> %s", owner, n.ID),
			"output resource already produced by node "+string(owner), nil)
	}
	if _, ok := g.sources[n.Output]; ok {
		return newGraphError("compile", ErrFilterGraphDuplicateOutput,
			n.ID, n.Output, "", "output resource collides with a declared source", nil)
	}
	if n.hasExplicitBounds() {
		if err := validateGraphRect(n.Bounds, "node bounds"); err != nil {
			return newGraphError("compile", ErrFilterGraphBounds,
				n.ID, "", "", err.Error(), err)
		}
	}

	g.nodeIDs[n.ID] = struct{}{}
	g.outputs[n.Output] = n.ID
	g.nodes = append(g.nodes, cloneFilterNode(n))
	return nil
}

// NodeCount returns the number of nodes added so far.
func (g *FilterGraph) NodeCount() int { return len(g.nodes) }

// SourceCount returns the number of declared sources.
func (g *FilterGraph) SourceCount() int { return len(g.sources) }

// cloneFilterNode defensively copies the input slice so later caller mutation
// cannot invalidate the compiled graph.
func cloneFilterNode(n FilterNode) FilterNode {
	n.Inputs = append([]FilterResourceID(nil), n.Inputs...)
	return n
}

// compiledFilterNode is the immutable per-node plan.
type compiledFilterNode struct {
	id        FilterNodeID
	op        FilterGraphOp
	inputs    []FilterResourceID
	producers []int // index of the producing node, or -1 for a declared source
	output    FilterResourceID

	explicitBounds Rect
	hasExplicit    bool

	// consumers counts how many distinct nodes read this node's output.
	consumers int
}

// CompiledFilterGraph is an immutable, validated filter DAG.
// It is safe for concurrent Execute calls.
type CompiledFilterGraph struct {
	nodes   []compiledFilterNode
	order   []int
	sources []FilterResourceID
	outputs map[FilterResourceID]int
	pool    *filterGraphPool
}

// Compile validates the graph and returns an immutable execution plan.
//
// Structural errors are reported in declaration order, followed by missing
// inputs, duplicate outputs and cycles. The returned topological order is
// stable: among nodes that become ready together, declaration order wins.
func (g *FilterGraph) Compile() (*CompiledFilterGraph, error) {
	cg := &CompiledFilterGraph{
		nodes:   make([]compiledFilterNode, len(g.nodes)),
		sources: append([]FilterResourceID(nil), g.sourceOrder...),
		outputs: make(map[FilterResourceID]int, len(g.nodes)),
		pool:    newFilterGraphPool(),
	}
	for i, n := range g.nodes {
		cg.nodes[i] = compiledFilterNode{
			id:             n.ID,
			op:             n.Op,
			inputs:         append([]FilterResourceID(nil), n.Inputs...),
			producers:      make([]int, len(n.Inputs)),
			output:         n.Output,
			explicitBounds: n.Bounds,
			hasExplicit:    n.hasExplicitBounds(),
		}
		cg.outputs[n.Output] = i
	}

	if err := cg.resolveInputs(); err != nil {
		return nil, err
	}

	order, err := cg.topologicalOrder()
	if err != nil {
		return nil, err
	}
	cg.order = order

	// Count distinct consumer nodes so buffer reference counting is exact
	// even when one node reads the same producer through several inputs.
	for i := range cg.nodes {
		seen := make(map[int]struct{}, len(cg.nodes[i].producers))
		for _, p := range cg.nodes[i].producers {
			if p < 0 {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			cg.nodes[p].consumers++
		}
	}
	return cg, nil
}

// resolveInputs maps every input to its producer and rejects missing inputs.
func (cg *CompiledFilterGraph) resolveInputs() error {
	declared := make(map[FilterResourceID]struct{}, len(cg.sources))
	for _, s := range cg.sources {
		declared[s] = struct{}{}
	}
	for i := range cg.nodes {
		n := &cg.nodes[i]
		for j, in := range n.inputs {
			if p, ok := cg.outputs[in]; ok {
				n.producers[j] = p
				continue
			}
			if _, ok := declared[in]; ok {
				n.producers[j] = -1
				continue
			}
			edge := fmt.Sprintf("%s -> %s", in, n.id)
			return newGraphError("compile", ErrFilterGraphMissingInput,
				n.id, in, edge, "input is neither a declared source nor a node output", nil)
		}
	}
	return nil
}

// topologicalOrder returns a stable topological ordering and reports cycles
// with the participating edge chain.
func (cg *CompiledFilterGraph) topologicalOrder() ([]int, error) {
	n := len(cg.nodes)
	indeg := make([]int, n)
	for i := range cg.nodes {
		for _, p := range cg.nodes[i].producers {
			if p >= 0 {
				indeg[i]++
			}
		}
	}

	ready := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	// ready starts sorted by declaration index and is kept sorted on
	// insertion, so the emitted order is stable.
	order := make([]int, 0, n)
	for len(ready) > 0 {
		idx := ready[0]
		ready = ready[1:]
		order = append(order, idx)
		for j := 0; j < n; j++ {
			for _, p := range cg.nodes[j].producers {
				if p != idx {
					continue
				}
				indeg[j]--
				if indeg[j] == 0 {
					ready = insertSortedInt(ready, j)
				}
			}
		}
	}

	if len(order) == n {
		return order, nil
	}
	return nil, cg.cycleError(indeg)
}

// insertSortedInt inserts v into ascending slice s, preserving order.
func insertSortedInt(s []int, v int) []int {
	i := len(s)
	for i > 0 && s[i-1] > v {
		i--
	}
	s = append(s, 0)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

// cycleError locates one concrete cycle among the nodes left with a non-zero
// indegree and builds an error containing the edge chain.
func (cg *CompiledFilterGraph) cycleError(indeg []int) error {
	remaining := make([]bool, len(cg.nodes))
	for i, d := range indeg {
		remaining[i] = d > 0
	}
	path := cg.findCycle(remaining)
	if len(path) == 0 {
		return newGraphError("compile", ErrFilterGraphCycle, "", "", "",
			"dependency cycle detected", nil)
	}
	names := make([]string, len(path))
	for i, idx := range path {
		names[i] = string(cg.nodes[idx].id)
	}
	// Close the loop so the message reads a -> b -> a.
	edge := strings.Join(names, " -> ") + " -> " + string(cg.nodes[path[0]].id)
	return newGraphError("compile", ErrFilterGraphCycle,
		cg.nodes[path[0]].id, cg.nodes[path[0]].output, edge,
		"dependency cycle detected", nil)
}

// findCycle performs a depth-first search limited to nodes that Kahn's
// algorithm could not order, returning the cycle as node indices.
func (cg *CompiledFilterGraph) findCycle(remaining []bool) []int {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make([]int, len(cg.nodes))
	stack := make([]int, 0, len(cg.nodes))
	pos := make([]int, len(cg.nodes))
	for i := range pos {
		pos[i] = -1
	}

	var dfs func(idx int) []int
	dfs = func(idx int) []int {
		color[idx] = gray
		pos[idx] = len(stack)
		stack = append(stack, idx)
		for _, p := range cg.nodes[idx].producers {
			if p < 0 || !remaining[p] {
				continue
			}
			switch color[p] {
			case white:
				if c := dfs(p); c != nil {
					return c
				}
			case gray:
				return append([]int(nil), stack[pos[p]:]...)
			}
		}
		stack = stack[:len(stack)-1]
		pos[idx] = -1
		color[idx] = black
		return nil
	}

	for i := range cg.nodes {
		if !remaining[i] || color[i] != white {
			continue
		}
		if c := dfs(i); c != nil {
			return c
		}
	}
	return nil
}

// NodeOrder returns the stable topological order of node IDs.
func (cg *CompiledFilterGraph) NodeOrder() []FilterNodeID {
	out := make([]FilterNodeID, len(cg.order))
	for i, idx := range cg.order {
		out[i] = cg.nodes[idx].id
	}
	return out
}

// NodeCount returns the number of nodes in the compiled graph.
func (cg *CompiledFilterGraph) NodeCount() int { return len(cg.nodes) }

// unionRects returns the smallest rect containing all non-empty inputs, or the
// canonical empty rect when there are none.
func unionRects(rects []Rect) Rect {
	var out Rect
	set := false
	for _, r := range rects {
		if r.IsEmpty() {
			continue
		}
		if !set {
			out = r
			set = true
			continue
		}
		out = out.Union(r)
	}
	if !set {
		return EmptyRect()
	}
	return out
}

// isFiniteFloat32 reports whether v is neither NaN nor ±Inf.
func isFiniteFloat32(v float32) bool {
	f := float64(v)
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// validateGraphRect rejects non-finite, inverted and absurdly large rects.
// The canonical empty rect is accepted: an empty region is a valid plan and
// yields a 1x1 scratch buffer with no pixels touched.
func validateGraphRect(r Rect, what string) error {
	if r == EmptyRect() {
		return nil
	}
	if !isFiniteFloat32(r.MinX) || !isFiniteFloat32(r.MinY) ||
		!isFiniteFloat32(r.MaxX) || !isFiniteFloat32(r.MaxY) {
		return fmt.Errorf("%s contains a non-finite coordinate: %+v", what, r)
	}
	if r.MinX > r.MaxX || r.MinY > r.MaxY {
		return fmt.Errorf("%s is inverted: %+v", what, r)
	}
	if float64(r.MaxX) > maxFilterGraphDimension || float64(r.MaxY) > maxFilterGraphDimension {
		return fmt.Errorf("%s exceeds maximum dimension %d: %+v", what, maxFilterGraphDimension, r)
	}
	return nil
}
