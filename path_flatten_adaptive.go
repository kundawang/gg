package gg

import (
	"context"
	"math"
)

// Budget-constrained adaptive curve flattening.
//
// This file adds a second-generation flattener next to the legacy recursive
// flattener in path_ops.go. Unlike the legacy one it:
//
//   - measures flatness in DEVICE space, so rotation/scale of the CTM is
//     accounted for before any segment is accepted as a straight line;
//   - allocates segments by curvature (sharp bends get more, near-linear
//     curves get a single chord) inside an explicit point/error budget —
//     when the budget cannot hold the requested tolerance, the tolerance is
//     relaxed globally and FlattenStats.BudgetExceeded reports it;
//   - preserves subpath structure exactly: MoveTo/Close boundaries are
//     emitted as events and no segment ever bridges two contours;
//   - supports context cancellation, reusable buffers (FlattenState) and a
//     zero-allocation callback entry;
//   - degrades deterministically for zero/negative/non-finite tolerance,
//     exhausted budgets, non-finite coordinates and extreme magnitudes.
//
// The legacy Flatten/FlattenCallback API is unchanged; the new entry points
// are FlattenAdaptive and FlattenAdaptiveCallback.

const (
	// defaultFlattenTolerance is used when the requested tolerance is not a
	// finite, positive number. It matches the legacy FlattenCallback default.
	defaultFlattenTolerance = 0.1

	// maxFlattenDepth bounds de Casteljau recursion. At depth 24 a curve is
	// split into at most 2^24 pieces. The depth cap is the guaranteed
	// termination path for non-finite and extreme-magnitude coordinates
	// whose flatness test never converges.
	maxFlattenDepth = 24

	// defaultMaxFlattenPoints is the hard point budget used when the caller
	// supplies none.
	defaultMaxFlattenPoints = 1_000_000

	// hardMaxFlattenPoints is the absolute ceiling regardless of caller
	// input; protects the renderer from pathological requests.
	hardMaxFlattenPoints = 100_000_000

	// flattenBudgetBinarySteps bounds tolerance refinement iterations.
	flattenBudgetBinarySteps = 32

	// flattenCancelCheckVerbs is how many verbs are processed between
	// context.Err() checks. Err() is an interface call, so amortizing it
	// keeps the hot loop allocation-free and fast.
	flattenCancelCheckVerbs = 32

	// flattenSafeCoord bounds the finite coordinate range inside which
	// de Casteljau arithmetic is trusted. Beyond this, curves degrade to
	// chords instead of risking overflow/intermediate Inf.
	flattenSafeCoord = 1e150

	// flattenChordMinLenSq guards the degenerate zero-length chord case in
	// the cubic perpendicular-distance test.
	flattenChordMinLenSq = 1e-30
)

// backgroundCtx is reused across passes that do not carry an explicit
// context, avoiding a per-call allocation in the hot flatten loop.
var backgroundCtx = context.Background()

// FlattenEvent identifies the kind of point delivered by
// FlattenAdaptiveCallback.
type FlattenEvent byte

const (
	// FlattenLine is a regular polyline vertex. The first vertex of every
	// subpath is always preceded by a FlattenMove event.
	FlattenLine FlattenEvent = iota
	// FlattenMove starts a new subpath. A segment must never connect the
	// previous point to this one.
	FlattenMove
)

// FlattenOptions configures a budget-constrained adaptive flatten.
type FlattenOptions struct {
	// Tolerance is the maximum permitted deviation in DEVICE-space units
	// (typically pixels). Zero, negative or non-finite values fall back to
	// defaultFlattenTolerance (0.1).
	Tolerance float64

	// Transform is the device transform applied while measuring flatness.
	// Only its linear part affects the error metric; translation does not
	// change distances. nil means identity. Output points stay in the
	// path's own (user) coordinate system.
	Transform *Matrix

	// MaxPoints bounds the number of emitted line vertices (moves count
	// toward the budget). When the requested tolerance needs more, the
	// tolerance is relaxed globally until the output fits, and
	// FlattenStats.BudgetExceeded is set. Values <= 0 select
	// defaultMaxFlattenPoints (1,000,000); the effective budget is always
	// capped at hardMaxFlattenPoints.
	MaxPoints int

	// Context allows the flatten to be canceled. nil means background.
	Context context.Context
}

// FlattenStats reports what a flatten pass actually did.
type FlattenStats struct {
	// Points is the number of FlattenLine vertices emitted in the final
	// pass.
	Points int

	// Moves is the number of FlattenMove events (one per emitted subpath).
	Moves int

	// Curves is the number of quadratic/cubic verbs processed.
	Curves int

	// Splits is the number of de Casteljau subdivisions performed.
	Splits int

	// AchievedTolerance is the tolerance used for the final pass. Equals
	// the requested tolerance when the budget sufficed, larger after
	// budget-driven relaxation.
	AchievedTolerance float64

	// BudgetExceeded is true when MaxPoints could not hold the requested
	// tolerance and the tolerance had to be relaxed.
	BudgetExceeded bool

	// ToleranceFallback is true when the requested tolerance was not a
	// finite positive number and the default was substituted.
	ToleranceFallback bool

	// DegenerateCurves counts curves that degraded to a chord (or were
	// skipped) because of non-finite or extreme coordinates.
	DegenerateCurves int

	// Canceled is true when the context ended the pass early.
	Canceled bool
}

// FlattenState holds reusable scratch storage across flatten calls.
// The zero value is ready to use; NewFlattenState pre-allocates the
// backing buffers. A FlattenState must not be used concurrently.
type FlattenState struct {
	out *Path // reused output buffer for FlattenAdaptive
}

// NewFlattenState creates a FlattenState with pre-allocated buffers.
func NewFlattenState() *FlattenState {
	return &FlattenState{out: NewPath()}
}

// Reset clears the scratch contents while keeping allocated capacity.
func (s *FlattenState) Reset() {
	if s == nil || s.out == nil {
		return
	}
	s.out.Reset()
}

// FlattenAdaptive flattens p into a line-only path under the given error and
// point budget.
//
// The returned path preserves subpath boundaries (MoveTo/Close) and never
// connects two contours. When state is non-nil the returned path is owned by
// the state and reused on the next call with that state; when state is nil a
// fresh path is allocated.
func (p *Path) FlattenAdaptive(state *FlattenState, opts FlattenOptions) (*Path, FlattenStats) {
	var out *Path
	if state != nil {
		out = state.out
	}
	if out == nil {
		out = NewPath()
	} else {
		out.Reset()
	}

	// Drive the engine with the sink path directly instead of a closure
	// capturing out, so a reused state costs zero allocations.
	stats := p.flattenAdaptiveRun(opts, nil, out)

	if state != nil {
		state.out = out
	}
	return out, stats
}

// FlattenAdaptiveCallback is the zero-allocation flatten entry: all output is
// delivered through fn and no path or point slice is allocated. The first
// callback of each subpath uses FlattenMove; subsequent vertices use
// FlattenLine. For cancellation pass a cancellable context in opts; fn must
// not retain the point between calls.
func (p *Path) FlattenAdaptiveCallback(opts FlattenOptions, fn func(ev FlattenEvent, pt Point)) FlattenStats {
	if len(p.verbs) == 0 || fn == nil {
		return FlattenStats{AchievedTolerance: defaultFlattenTolerance}
	}
	return p.flattenAdaptiveRun(opts, fn, nil)
}

// flattenAdaptiveRun contains the two-pass budget algorithm. Exactly one of
// fn (callback mode) or sink (path mode) is used for output.
func (p *Path) flattenAdaptiveRun(opts FlattenOptions, fn func(ev FlattenEvent, pt Point), sink *Path) FlattenStats {
	e, devTol, fallback := opts.normalize()
	e.fn = fn
	e.sink = sink
	e.stats.ToleranceFallback = fallback
	e.stats.AchievedTolerance = devTol
	budget := effectivePointBudget(opts.MaxPoints)
	e.cap = budget

	// Pass 1: count the vertices the requested tolerance needs. The count
	// drive short-circuits at the budget, so an unaffordable tolerance
	// costs O(budget) work instead of O(2^depth) recursion.
	e.drive(p)

	// A capped drive cannot tell us the true segment count, only that the
	// requested tolerance exceeds the budget. Hit is sticky within drive().
	tol := devTol
	if e.hitCap && !e.aborted {
		tol = e.resolveBudgetTolerance(p, devTol, budget)
	}

	if e.aborted {
		e.stats.Canceled = true
		e.stats.AchievedTolerance = tol
		return e.stats
	}

	if tol > devTol*(1+1e-12) {
		e.stats.BudgetExceeded = true
	}
	e.stats.AchievedTolerance = tol

	// Pass 2: emit with the settled tolerance. Reset per-pass counters so
	// the returned stats describe exactly the emitted geometry while
	// keeping option-derived flags.
	keepFallback := e.stats.ToleranceFallback
	keepExceeded := e.stats.BudgetExceeded
	e.resetPassStats()
	e.stats.ToleranceFallback = keepFallback
	e.stats.BudgetExceeded = keepExceeded
	e.stats.Canceled = false
	e.emit = true
	e.tol = tol
	e.cap = 0 // the emit pass never short-circuits: the settled tolerance
	// fits the budget, and the recursion depth cap bounds the
	// pathological non-finite tail.
	e.drive(p)
	if e.aborted {
		e.stats.Canceled = true
	}
	e.stats.AchievedTolerance = tol
	return e.stats
}

func effectivePointBudget(maxPoints int) int {
	if maxPoints <= 0 {
		return defaultMaxFlattenPoints
	}
	if maxPoints > hardMaxFlattenPoints {
		return hardMaxFlattenPoints
	}
	return maxPoints
}

// flattenEngine carries the immutable per-pass configuration and mutable
// counters. The same engine drives both the counting pass and the emitting
// pass, guaranteeing that counted and emitted segments agree. Stats are
// embedded (not pointed at) so the whole engine stays on the caller's stack
// in the zero-allocation callback path.
type flattenEngine struct {
	tol     float64 // device-space flatness budget
	lin     [4]float64
	hasLin  bool
	stats   FlattenStats
	emit    bool
	fn      func(ev FlattenEvent, pt Point)
	sink    *Path // path-mode output; used when fn is nil
	ctx     context.Context
	verbN   int // verbs seen since last cancellation check
	aborted bool
	// cap > 0 makes count drives stop once stats.Points reaches cap,
	// bounding the work of budget probing. cap == 0 means no short-circuit.
	cap int
	// hitCap is set when a count drive stops because the cap was reached,
	// distinguishing "exactly budget points" from "fits within budget".
	hitCap bool
}

// fitsBudget runs one capped count drive at tol and reports whether the
// resulting geometry stays within budget.
func (e *flattenEngine) fitsBudget(p *Path, tol float64, budget int) bool {
	e.tol = tol
	e.resetPassStats()
	c := e.drive(p)
	return !e.hitCap && c <= budget
}

// resolveBudgetTolerance finds the tightest tolerance whose output fits the
// budget, starting from an analytical estimate. It first grows the tolerance
// until a capped drive fits, then binary-searches back toward the request.
// Every drive is capped, so total work is budget times a small constant.
func (e *flattenEngine) resolveBudgetTolerance(p *Path, devTol float64, budget int) float64 {
	hi := e.estimateToleranceForBudget(p, devTol, budget)
	lo := devTol
	found := false
	for step := 0; step < flattenBudgetBinarySteps && !e.aborted; step++ {
		if e.fitsBudget(p, hi, budget) {
			found = true
			break
		}
		lo = hi
		hi *= 2
		if !isFinite(hi) || hi == math.MaxFloat64 {
			// MaxFloat64 makes every curve accept its chord, which always
			// fits (one vertex per verb + moves + closes).
			hi = math.MaxFloat64
			found = e.fitsBudget(p, hi, budget)
			break
		}
	}
	if !found {
		// Cancellation, or an estimator edge case: guarantee the
		// invariant by falling back to chord-only output.
		return math.MaxFloat64
	}
	// Binary refinement toward the tightest tolerance that fits.
	tol := hi
	for step := 0; step < flattenBudgetBinarySteps && hi-lo > lo*1e-12 && !e.aborted; step++ {
		mid := math.Max(lo*1.01, 0.5*(lo+hi))
		if !isFinite(mid) || mid >= hi {
			break
		}
		if e.fitsBudget(p, mid, budget) {
			hi, tol = mid, mid
		} else {
			lo = mid
		}
	}
	return tol
}

// normalize validates the options and returns a configured engine. The
// fallback return value reports whether the tolerance was substituted; the
// caller owns the stats struct, which keeps it on the stack in the
// zero-allocation callback path.
func (opts FlattenOptions) normalize() (e flattenEngine, devTol float64, fallback bool) {
	devTol = opts.Tolerance
	if !(devTol > 0) || math.IsInf(devTol, 0) || math.IsNaN(devTol) {
		devTol = defaultFlattenTolerance
		fallback = true
	}

	ctx := opts.Context
	if ctx == nil {
		ctx = backgroundCtx
	}

	e = flattenEngine{tol: devTol, ctx: ctx}
	if opts.Transform == nil {
		// No transform: flatness measured directly in user space.
		e.lin = [4]float64{1, 0, 0, 1}
		e.hasLin = true
	} else {
		m := *opts.Transform
		det := m.A*m.E - m.B*m.D
		// A non-finite or singular linear part collapses distances: every
		// curve is accepted as a chord rather than dividing by zero.
		if !finiteMatrixLinear(m) || math.IsNaN(det) || math.IsInf(det, 0) || det == 0 {
			e.hasLin = false
		} else {
			e.lin = [4]float64{m.A, m.B, m.D, m.E}
			e.hasLin = true
		}
	}
	return e, devTol, fallback
}

func finiteMatrixLinear(m Matrix) bool {
	return isFinite(m.A) && isFinite(m.B) && isFinite(m.D) && isFinite(m.E)
}

// resetPassStats zeroes per-pass counters before another count iteration or
// the emit pass. Option-derived flags are restored by the caller.
func (e *flattenEngine) resetPassStats() {
	e.stats.Points = 0
	e.stats.Moves = 0
	e.stats.Curves = 0
	e.stats.Splits = 0
	e.stats.DegenerateCurves = 0
}

// devNorm returns |M*v| for the linear part of the device transform. When
// the transform is singular it returns 0, meaning every deviation is
// considered flat (the documented singular-transform degradation).
func (e *flattenEngine) devNorm(vx, vy float64) float64 {
	if !e.hasLin {
		return 0
	}
	dx := e.lin[0]*vx + e.lin[1]*vy
	dy := e.lin[2]*vx + e.lin[3]*vy
	return math.Hypot(dx, dy)
}

// drive walks the verb stream once, either counting output vertices or
// emitting them, and returns the number of vertices produced.
func (e *flattenEngine) drive(p *Path) int {
	cur := driveCursor{}
	ci := 0
	e.verbN = 0
	e.aborted = false
	e.hitCap = false

	for _, v := range p.verbs {
		e.verbN++
		if e.verbN >= flattenCancelCheckVerbs {
			e.verbN = 0
			if err := e.ctx.Err(); err != nil {
				e.aborted = true
				return e.stats.Points
			}
		}
		// Allow the count to reach cap+1 so a path that fits the budget
		// exactly is distinguished from one that overshoots: recursion
		// stops only once more than cap vertices would be produced.
		if e.budgetFull() {
			return e.stats.Points
		}

		n := verbCoordCount(v)
		var coords []float64
		if n > 0 {
			coords = p.coords[ci : ci+n]
		}
		ci += n
		cur.process(e, v, coords)
	}
	return e.stats.Points
}

// driveCursor tracks the current subpath anchor and pen position while
// walking a verb stream.
type driveCursor struct {
	start, current Point
	started        bool
}

// process handles a single verb, updating the cursor and the engine stats.
func (c *driveCursor) process(e *flattenEngine, v PathVerb, coords []float64) {
	switch v {
	case MoveTo:
		pt := Pt(coords[0], coords[1])
		if !isFinite(pt.X) || !isFinite(pt.Y) {
			// A non-finite move cannot anchor a contour; skip it but keep
			// stream bookkeeping consistent.
			c.start, c.current, c.started = pt, pt, false
			return
		}
		if e.emit {
			e.output(FlattenMove, pt)
		}
		e.stats.Moves++
		e.stats.Points++
		c.start, c.current, c.started = pt, pt, true
	case LineTo:
		pt := Pt(coords[0], coords[1])
		if !c.started || !isFinite(pt.X) || !isFinite(pt.Y) {
			c.current = pt
			return
		}
		e.emitVertex(pt)
		c.current = pt
	case QuadTo:
		c.handleQuad(e, coords)
	case CubicTo:
		c.handleCubic(e, coords)
	case Close:
		// Emit the closing vertex only when it differs from the current
		// point. Subpath identity is preserved: nothing is emitted when
		// the contour was never anchored.
		if c.started && c.current != c.start &&
			isFinite(c.start.X) && isFinite(c.start.Y) {
			e.emitVertex(c.start)
		}
		c.current = c.start
	}
}

func (c *driveCursor) handleQuad(e *flattenEngine, coords []float64) {
	ctrl := Pt(coords[0], coords[1])
	pt := Pt(coords[2], coords[3])
	if !c.started || !isFinite(pt.X) || !isFinite(pt.Y) {
		c.current = pt
		return
	}
	e.stats.Curves++
	if !isFinite(ctrl.X) || !isFinite(ctrl.Y) ||
		extremePoint(c.current) || extremePoint(ctrl) || extremePoint(pt) {
		e.stats.DegenerateCurves++
		e.emitVertex(pt)
	} else {
		e.flattenQuad(QuadBez{P0: c.current, P1: ctrl, P2: pt}, 0)
	}
	c.current = pt
}

func (c *driveCursor) handleCubic(e *flattenEngine, coords []float64) {
	c1 := Pt(coords[0], coords[1])
	c2 := Pt(coords[2], coords[3])
	pt := Pt(coords[4], coords[5])
	if !c.started || !isFinite(pt.X) || !isFinite(pt.Y) {
		c.current = pt
		return
	}
	e.stats.Curves++
	if !isFinite(c1.X) || !isFinite(c1.Y) || !isFinite(c2.X) || !isFinite(c2.Y) ||
		extremePoint(c.current) || extremePoint(c1) || extremePoint(c2) || extremePoint(pt) {
		e.stats.DegenerateCurves++
		e.emitVertex(pt)
	} else {
		e.flattenCubic(CubicBez{P0: c.current, P1: c1, P2: c2, P3: pt}, 0)
	}
	c.current = pt
}

func extremePoint(p Point) bool {
	return math.Abs(p.X) > flattenSafeCoord || math.Abs(p.Y) > flattenSafeCoord
}

// emitVertex delivers one polyline vertex in both passes (the count pass
// only increments). Count passes allow one vertex past the cap before
// stopping, so exact-fit and overshoot are distinguishable.
func (e *flattenEngine) emitVertex(pt Point) {
	if !e.emit && e.cap > 0 && e.stats.Points > e.cap {
		return
	}
	if e.emit {
		e.output(FlattenLine, pt)
	}
	e.stats.Points++
}

// output dispatches a vertex to the callback or the sink path.
func (e *flattenEngine) output(ev FlattenEvent, pt Point) {
	if e.fn != nil {
		e.fn(ev, pt)
		return
	}
	if ev == FlattenMove {
		e.sink.MoveTo(pt.X, pt.Y)
	} else {
		e.sink.LineTo(pt.X, pt.Y)
	}
}

// budgetFull reports whether a count drive has already produced more than
// cap vertices. Recursion checks this at every entry, guaranteeing a single
// curve cannot blow past cap+1 between verb-level checks. hitCap is set only
// on a genuine overshoot, so a path using exactly cap vertices is reported as
// fitting.
func (e *flattenEngine) budgetFull() bool {
	if !e.emit && e.cap > 0 && e.stats.Points > e.cap {
		e.hitCap = true
		return true
	}
	return false
}

// flattenQuad recursively flattens a quadratic. The flatness metric is the
// device-space maximum deviation of the curve from the P0-P2 chord: for a
// quadratic the curve's midpoint deviates from the chord by exactly
// 2*(P1 - chordMid), i.e. the convex hull bound is exact, so accepting
// |M*(2*dev)| <= tol provably keeps Hausdorff error within tol in device
// space.
func (e *flattenEngine) flattenQuad(q QuadBez, depth int) {
	if depth >= maxFlattenDepth {
		e.emitVertex(q.P2)
		return
	}
	if e.budgetFull() {
		return
	}

	dev := q.P1.Sub(q.P0.Lerp(q.P2, 0.5))
	if e.devNorm(2*dev.X, 2*dev.Y) <= e.tol {
		e.emitVertex(q.P2)
		return
	}

	e.stats.Splits++
	q1, q2 := q.Subdivide()
	e.flattenQuad(q1, depth+1)
	if e.budgetFull() {
		return
	}
	e.flattenQuad(q2, depth+1)
}

// flattenCubic recursively flattens a cubic. The flatness metric is the
// maximum device-space perpendicular distance of the two interior control
// points from the P0-P3 chord line. The Bézier convex hull guarantees the
// whole curve stays inside the control-point hull, so this bounds the
// curve-to-chord Hausdorff distance conservatively with no magic scale
// factors.
func (e *flattenEngine) flattenCubic(c CubicBez, depth int) {
	if depth >= maxFlattenDepth {
		e.emitVertex(c.P3)
		return
	}
	if e.budgetFull() {
		return
	}

	d1 := e.chordPerpDistance(c.P0, c.P3, c.P1)
	d2 := e.chordPerpDistance(c.P0, c.P3, c.P2)
	if math.Max(d1, d2) <= e.tol {
		e.emitVertex(c.P3)
		return
	}

	e.stats.Splits++
	a, b := c.Subdivide()
	e.flattenCubic(a, depth+1)
	if e.budgetFull() {
		return
	}
	e.flattenCubic(b, depth+1)
}

// chordPerpDistance returns the device-space length of the perpendicular
// displacement from point q to the line through p0 and p1.
func (e *flattenEngine) chordPerpDistance(p0, p1, q Point) float64 {
	dx := p1.X - p0.X
	dy := p1.Y - p0.Y
	lenSq := dx*dx + dy*dy
	if lenSq < flattenChordMinLenSq {
		// Degenerate chord: use the direct control-point displacement.
		return e.devNorm(q.X-p0.X, q.Y-p0.Y)
	}
	// Signed perpendicular scalar s = cross(chord, q-p0) / |chord|;
	// the perpendicular vector in user space is s * (-dy, dx)/|chord|.
	invLen := 1 / math.Sqrt(lenSq)
	s := (dx*(q.Y-p0.Y) - dy*(q.X-p0.X)) * invLen
	return e.devNorm(-dy*invLen*s, dx*invLen*s)
}

// estimateToleranceForBudget computes a tolerance whose expected segment
// count fits the budget, directly from each curve's flatness metric.
// Flatness shrinks by a factor of 4 per subdivision level, so a curve whose
// current deviation is d needs d/4^k <= tol pieces 2^k, i.e.
// k = ceil(log4(d/tol)); solving for tol at a per-curve share of the budget
// gives the estimate below. The result is verified by an actual count
// drive, so estimation error only costs extra refinement iterations.
func (e *flattenEngine) estimateToleranceForBudget(p *Path, devTol float64, budget int) float64 {
	m := &flatnessMeasure{engine: e}
	m.walk(p)

	if m.curves == 0 || m.worstD <= devTol {
		return devTol
	}
	// Per-curve segment share of the remaining budget.
	share := float64(budget-m.basePts) / float64(m.curves)
	if share < 1 {
		share = 1
	}
	// A subdivision level quarters the flatness metric and doubles the
	// segment count, hence pieces ~= sqrt(worstD/tol). Solving
	// sqrt(worstD/tol) <= share for the tolerance gives:
	est := m.worstD / (share * share)
	if est <= devTol || !isFinite(est) {
		// Estimator says the requested tolerance already fits; return a
		// value above it so the verify drive actually probes a relaxed
		// tolerance and the growth loop can take over if the estimate was
		// optimistic.
		return devTol * 4
	}
	// Safety margin so the verification drive usually fits at once.
	return est * 1.1
}

// flatnessMeasure accumulates the base (non-subdivided) vertex count and the
// roughest curve deviation across a verb stream for the budget estimator.
type flatnessMeasure struct {
	engine  *flattenEngine
	start   Point
	current Point
	started bool
	basePts int
	curves  int
	worstD  float64
}

func (m *flatnessMeasure) walk(p *Path) {
	ci := 0
	for _, v := range p.verbs {
		n := verbCoordCount(v)
		var coords []float64
		if n > 0 {
			coords = p.coords[ci : ci+n]
		}
		ci += n
		m.process(v, coords)
	}
}

func (m *flatnessMeasure) process(v PathVerb, coords []float64) {
	switch v {
	case MoveTo:
		pt := Pt(coords[0], coords[1])
		if isFinite(pt.X) && isFinite(pt.Y) {
			m.start, m.current, m.started = pt, pt, true
			m.basePts++
		} else {
			m.started = false
		}
	case LineTo:
		pt := Pt(coords[0], coords[1])
		if m.started && isFinite(pt.X) && isFinite(pt.Y) {
			m.basePts++
		}
		m.current = pt
	case QuadTo:
		m.measureQuad(coords)
	case CubicTo:
		m.measureCubic(coords)
	case Close:
		if m.started && m.current != m.start &&
			isFinite(m.start.X) && isFinite(m.start.Y) {
			m.basePts++
		}
		m.current = m.start
	}
}

func (m *flatnessMeasure) measureQuad(coords []float64) {
	ctrl := Pt(coords[0], coords[1])
	pt := Pt(coords[2], coords[3])
	if !m.started || !isFinite(pt.X) || !isFinite(pt.Y) {
		m.current = pt
		return
	}
	m.curves++
	if isFinite(ctrl.X) && isFinite(ctrl.Y) {
		dev := ctrl.Sub(m.current.Lerp(pt, 0.5))
		m.worstD = math.Max(m.worstD, m.engine.devNorm(2*dev.X, 2*dev.Y))
	}
	m.basePts++
	m.current = pt
}

func (m *flatnessMeasure) measureCubic(coords []float64) {
	c1 := Pt(coords[0], coords[1])
	c2 := Pt(coords[2], coords[3])
	pt := Pt(coords[4], coords[5])
	if !m.started || !isFinite(pt.X) || !isFinite(pt.Y) {
		m.current = pt
		return
	}
	m.curves++
	if isFinite(c1.X) && isFinite(c1.Y) && isFinite(c2.X) && isFinite(c2.Y) {
		d1 := m.engine.chordPerpDistance(m.current, pt, c1)
		d2 := m.engine.chordPerpDistance(m.current, pt, c2)
		m.worstD = math.Max(m.worstD, math.Max(d1, d2))
	}
	m.basePts++
	m.current = pt
}

// FlattenedBounds returns the bounding rectangle of the adaptive,
// budget-constrained line flattening of p (curve control points that lie
// outside the actual curve are excluded, unlike BoundingBox() which includes
// them). The rectangle covers exactly the vertices that FlattenAdaptive
// would emit for the same options.
func (p *Path) FlattenedBounds(opts FlattenOptions) (Rect, FlattenStats) {
	bbox := Rect{}
	have := false
	stats := p.FlattenAdaptiveCallback(opts, func(ev FlattenEvent, pt Point) {
		if ev != FlattenLine && ev != FlattenMove {
			return
		}
		if !have {
			bbox = NewRect(pt, pt)
			have = true
			return
		}
		bbox = bbox.Union(NewRect(pt, pt))
	})
	return bbox, stats
}

// FlattenedContains reports whether pt is inside the flattened path using
// the non-zero fill rule. It is a hit-testing entry built entirely on the
// adaptive flattener: the same budget, transform and tolerance semantics
// apply, and contours remain independent. When the pass is canceled before
// completion the result is false and stats.Canceled is set.
//
// Ray casting runs on the callback stream, so the test allocates nothing
// beyond the engine's scalar recursion.
func (p *Path) FlattenedContains(pt Point, opts FlattenOptions) (bool, FlattenStats) {
	var winding int
	var subStart, prev Point
	havePrev := false
	closed := false
	stats := p.FlattenAdaptiveCallback(opts, func(ev FlattenEvent, q Point) {
		if ev == FlattenMove {
			// An open previous contour is closed implicitly for the test.
			if havePrev && !closed && prev != subStart {
				winding += lineWinding(prev, subStart, pt)
			}
			subStart, prev, havePrev = q, q, true
			closed = false
			return
		}
		if havePrev {
			winding += lineWinding(prev, q, pt)
		}
		prev = q
		// The engine emits the start point as a regular line vertex only
		// for an explicit Close verb.
		if q == subStart {
			closed = true
		}
	})
	if havePrev && !closed && prev != subStart {
		winding += lineWinding(prev, subStart, pt)
	}
	return winding != 0, stats
}
