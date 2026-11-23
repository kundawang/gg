package gg

import (
	"context"
	"math"
	"math/rand"
	"testing"
	"time"
)

// This file covers the budget-constrained adaptive flattener:
//
//   - Hausdorff error vs the true curve for thin ellipses, cusps,
//     continuous cubics, rotation/scale transforms;
//   - direction preservation (lines, closed contours, reversed paths);
//   - subpath isolation (no cross-contour segments);
//   - budget cap, tolerance fallback, non-finite/extreme coordinates,
//     singular transforms and cancellation;
//   - buffer reuse and zero-allocation callback;
//   - parity with the legacy Flatten and opt-in Fill/Stroke rendering.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func thinEllipsePath() *Path {
	p := NewPath()
	p.Ellipse(0, 0, 200, 0.5)
	return p
}

func cuspPath() *Path {
	// Cubics whose control polygon folds back create a sharp cusp at the
	// meeting point; curvature is effectively unbounded there.
	p := NewPath()
	p.MoveTo(0, 0)
	p.CubicTo(40, 40, -40, 40, 0, 0)
	p.CubicTo(-40, -40, 40, -40, 0, 0)
	p.Close()
	return p
}

func sharpCornerPath() *Path {
	// Two arcs meeting at a near-zero-radius turn.
	p := NewPath()
	p.MoveTo(0, 0)
	p.CubicTo(50, 0, 100, 0, 100, 50)
	p.CubicTo(100, 50.05, 50, 50.05, 50, 50)
	p.LineTo(0, 50)
	p.Close()
	return p
}

func continuousCubicsPath(n int) *Path {
	r := rand.New(rand.NewSource(42))
	p := NewPath()
	x, y := 0.0, 0.0
	p.MoveTo(x, y)
	for i := 0; i < n; i++ {
		c1x := x + r.Float64()*60 - 10
		c1y := y + r.Float64()*80 - 40
		c2x := x + r.Float64()*60 + 10
		c2y := y + r.Float64()*80 - 40
		x += 20 + r.Float64()*30
		y = r.Float64()*100 - 50
		p.CubicTo(c1x, c1y, c2x, c2y, x, y)
	}
	return p
}

func circlePath(r float64) *Path {
	p := NewPath()
	p.Circle(0, 0, r)
	return p
}

func twoContourPath() *Path {
	p := NewPath()
	p.Circle(0, 0, 20)
	p.MoveTo(100, 100)
	p.CubicTo(140, 100, 140, 140, 100, 140)
	p.Close()
	return p
}

// sampleDense evaluates the exact path into densely sampled points,
// preserving subpath order. Used as the ground-truth curve.
func sampleDense(p *Path, perCurve int) [][]Point {
	var contours [][]Point
	var cur []Point
	var start, current Point
	started := false

	p.Iterate(func(verb PathVerb, coords []float64) {
		switch verb {
		case MoveTo:
			if len(cur) > 0 {
				contours = append(contours, cur)
			}
			pt := Pt(coords[0], coords[1])
			cur = []Point{pt}
			start, current, started = pt, pt, true
		case LineTo:
			pt := Pt(coords[0], coords[1])
			for i := 1; i <= perCurve; i++ {
				cur = append(cur, current.Lerp(pt, float64(i)/float64(perCurve)))
			}
			current = pt
		case QuadTo:
			q := QuadBez{P0: current, P1: Pt(coords[0], coords[1]), P2: Pt(coords[2], coords[3])}
			for i := 1; i <= perCurve; i++ {
				cur = append(cur, q.Eval(float64(i)/float64(perCurve)))
			}
			current = q.P2
		case CubicTo:
			c := CubicBez{P0: current, P1: Pt(coords[0], coords[1]), P2: Pt(coords[2], coords[3]), P3: Pt(coords[4], coords[5])}
			for i := 1; i <= perCurve; i++ {
				cur = append(cur, c.Eval(float64(i)/float64(perCurve)))
			}
			current = c.P3
		case Close:
			if started && current != start {
				for i := 1; i <= perCurve; i++ {
					cur = append(cur, current.Lerp(start, float64(i)/float64(perCurve)))
				}
			}
			current = start
		}
	})
	if len(cur) > 0 {
		contours = append(contours, cur)
	}
	return contours
}

// pointToSegmentDistance returns the distance from q to segment a-b.
func pointToSegmentDistance(q, a, b Point) float64 {
	vx, vy := b.X-a.X, b.Y-a.Y
	wx, wy := q.X-a.X, q.Y-a.Y
	lenSq := vx*vx + vy*vy
	if lenSq < 1e-30 {
		return q.Distance(a)
	}
	t := (wx*vx + wy*vy) / lenSq
	t = math.Max(0, math.Min(1, t))
	px, py := a.X+t*vx, a.Y+t*vy
	return math.Hypot(q.X-px, q.Y-py)
}

// hausdorffToPolyline returns max over truth samples of the min distance to
// the polyline segments.
func hausdorffToPolyline(truth, poly []Point) float64 {
	worst := 0.0
	for _, q := range truth {
		best := math.MaxFloat64
		for i := 0; i+1 < len(poly); i++ {
			d := pointToSegmentDistance(q, poly[i], poly[i+1])
			if d < best {
				best = d
			}
		}
		if len(poly) == 1 {
			best = q.Distance(poly[0])
		}
		if best > worst {
			worst = best
		}
	}
	return worst
}

func transformPoints(m Matrix, pts []Point) []Point {
	out := make([]Point, len(pts))
	for i, p := range pts {
		out[i] = m.TransformPoint(p)
	}
	return out
}

// flattenToContours runs the adaptive flattener and groups output into
// polylines keyed by FlattenMove.
func flattenToContours(p *Path, opts FlattenOptions) ([][]Point, FlattenStats) {
	var contours [][]Point
	var cur []Point
	stats := p.FlattenAdaptiveCallback(opts, func(ev FlattenEvent, pt Point) {
		if ev == FlattenMove {
			if len(cur) > 0 {
				contours = append(contours, cur)
			}
			cur = []Point{pt}
			return
		}
		cur = append(cur, pt)
	})
	if len(cur) > 0 {
		contours = append(contours, cur)
	}
	return contours, stats
}

// ---------------------------------------------------------------------------
// Hausdorff error fixtures
// ---------------------------------------------------------------------------

func TestAdaptiveFlatten_HausdorffFixtures(t *testing.T) {
	mats := []struct {
		name string
		m    Matrix
	}{
		{"identity", Identity()},
		{"scale4", Scale(4, 4)},
		{"scaleDown", Scale(0.07, 0.07)},
		{"anisotropic", Scale(3.5, 0.25)},
		{"rotateScale", Rotate(0.7).Multiply(Scale(2.2, 0.4))},
		{"shear", Shear(0.6, 0.2)},
	}
	fixtures := []struct {
		name string
		p    *Path
	}{
		{"thin ellipse", thinEllipsePath()},
		{"cusp", cuspPath()},
		{"sharp corner", sharpCornerPath()},
		{"continuous cubics", continuousCubicsPath(40)},
		{"circle r120", circlePath(120)},
		{"circle r1", circlePath(1)},
	}

	const tol = 0.25
	for _, fx := range fixtures {
		for _, tm := range mats {
			name := fx.name + "/" + tm.name
			t.Run(name, func(t *testing.T) {
				opts := FlattenOptions{Tolerance: tol, Transform: &tm.m, MaxPoints: defaultMaxFlattenPoints}
				contours, stats := flattenToContours(fx.p, opts)
				if stats.Canceled {
					t.Fatal("unexpected cancellation")
				}
				if stats.BudgetExceeded {
					t.Fatalf("budget exceeded at default cap: %d points", stats.Points)
				}
				truth := sampleDense(fx.p, 120)
				if len(contours) != len(truth) {
					t.Fatalf("contour count = %d, want %d", len(contours), len(truth))
				}
				for i, poly := range contours {
					got := hausdorffToPolyline(transformPoints(tm.m, truth[i]), transformPoints(tm.m, poly))
					// Convex-hull flatness bounds Hausdorff error in device
					// space by the tolerance; allow 5% slack for sampling
					// discretization of the ground truth.
					if got > tol*1.05 {
						t.Errorf("Hausdorff = %.4f device px, want <= %.4f (poly verts=%d)", got, tol, len(poly))
					}
				}
			})
		}
	}
}

// TestAdaptiveFlatten_ErrorMonotonic checks that tighter tolerance strictly
// reduces (or keeps) both error and that looser tolerance produces fewer
// points.
func TestAdaptiveFlatten_ErrorMonotonic(t *testing.T) {
	p := continuousCubicsPath(20)
	tols := []float64{4.0, 1.0, 0.25, 0.05}
	prevPts := 0
	prevErr := math.MaxFloat64
	for _, tol := range tols {
		contours, stats := flattenToContours(p, FlattenOptions{Tolerance: tol})
		if stats.BudgetExceeded {
			t.Fatalf("tol=%v unexpectedly exceeded budget", tol)
		}
		truth := sampleDense(p, 200)
		err := 0.0
		for i, poly := range contours {
			err = math.Max(err, hausdorffToPolyline(truth[i], poly))
		}
		if err > tol*1.05 {
			t.Errorf("tol=%v Hausdorff=%.4f exceeds tolerance", tol, err)
		}
		// Tighter tolerance must use at least as many points.
		if stats.Points < prevPts {
			t.Errorf("tol=%v points=%d < previous %d (expected monotonic growth)", tol, stats.Points, prevPts)
		}
		// Tighter tolerance must not worsen the measured error.
		if err > prevErr+1e-9 {
			t.Errorf("tol=%v err=%.5f worse than previous %.5f", tol, err, prevErr)
		}
		prevPts, prevErr = stats.Points, err
	}
}

// ---------------------------------------------------------------------------
// Direction and contour preservation
// ---------------------------------------------------------------------------

func TestAdaptiveFlatten_DirectionPreserved(t *testing.T) {
	// Line-only path: output must equal the input vertices in order.
	p := NewPath()
	p.MoveTo(0, 0)
	p.LineTo(10, 5)
	p.LineTo(20, -3)
	p.LineTo(30, 8)

	var pts []Point
	var moves int
	p.FlattenAdaptiveCallback(FlattenOptions{Tolerance: 0.1}, func(ev FlattenEvent, pt Point) {
		if ev == FlattenMove {
			moves++
		}
		pts = append(pts, pt)
	})
	want := []Point{{0, 0}, {10, 5}, {20, -3}, {30, 8}}
	if moves != 1 || len(pts) != len(want) {
		t.Fatalf("moves=%d pts=%d, want 1 move and %d pts", moves, len(want), len(want))
	}
	for i := range want {
		if pts[i] != want[i] {
			t.Errorf("pt[%d]=%v, want %v", i, pts[i], want[i])
		}
	}
}

func TestAdaptiveFlatten_ClosedContourReturnsToStart(t *testing.T) {
	p := NewPath()
	p.MoveTo(5, 7)
	p.CubicTo(20, -5, 40, 25, 55, 7)
	p.Close()

	contours, _ := flattenToContours(p, FlattenOptions{Tolerance: 0.1})
	if len(contours) != 1 {
		t.Fatalf("contours=%d, want 1", len(contours))
	}
	poly := contours[0]
	first, last := poly[0], poly[len(poly)-1]
	if first.Distance(Pt(5, 7)) > 1e-9 || last.Distance(Pt(5, 7)) > 1e-9 {
		t.Errorf("closed contour first=%v last=%v, want both at (5,7)", first, last)
	}
}

func TestAdaptiveFlatten_NoCrossContourConnection(t *testing.T) {
	p := twoContourPath()
	contours, stats := flattenToContours(p, FlattenOptions{Tolerance: 0.1})
	if len(contours) != 2 {
		t.Fatalf("contours=%d, want 2", len(contours))
	}
	if stats.Moves != 2 {
		t.Errorf("moves=%d, want 2", stats.Moves)
	}
	// No vertex of contour 2 may equal a vertex of contour 1 and the two
	// polylines must stay near their own anchor regions.
	for _, q := range contours[1] {
		for _, r := range contours[0] {
			if q == r {
				t.Errorf("contours share vertex %v", q)
			}
		}
	}
}

func TestAdaptiveFlatten_ReversedPathDirection(t *testing.T) {
	p := continuousCubicsPath(8)
	rev := p.Reversed()

	a, _ := flattenToContours(p, FlattenOptions{Tolerance: 0.1})
	b, _ := flattenToContours(rev, FlattenOptions{Tolerance: 0.1})
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("contour counts a=%d b=%d", len(a), len(b))
	}
	fwd, back := a[0], b[0]
	if len(fwd) != len(back) {
		t.Fatalf("vertex count fwd=%d back=%d", len(fwd), len(back))
	}
	for i := range fwd {
		if fwd[i].Distance(back[len(back)-1-i]) > 1e-9 {
			t.Fatalf("reversed vertex %d mismatch: %v vs %v", i, fwd[i], back[len(back)-1-i])
		}
	}
}

// TestAdaptiveFlatten_StableOutput checks determinism: identical options must
// yield identical geometry across repeated calls and reused buffers.
func TestAdaptiveFlatten_StableOutput(t *testing.T) {
	p := continuousCubicsPath(12)
	opts := FlattenOptions{Tolerance: 0.13, MaxPoints: 500}
	first, s1 := flattenToContours(p, opts)
	state := NewFlattenState()
	out2, s2 := p.FlattenAdaptive(state, opts)
	out3, s3 := p.FlattenAdaptive(state, opts)
	if s1.Points != s2.Points || s2.Points != s3.Points {
		t.Fatalf("unstable point counts %d %d %d", s1.Points, s2.Points, s3.Points)
	}
	got := collectPathPoints(out2)
	if len(got) != len(first[0]) {
		t.Fatalf("reused buffer vertices=%d, callback=%d", len(got), len(first[0]))
	}
	for i := range got {
		if got[i].Distance(first[0][i]) > 1e-12 {
			t.Fatalf("vertex %d diverges: %v vs %v", i, got[i], first[0][i])
		}
	}
	if out2 != out3 {
		t.Fatal("FlattenState must reuse the same *Path")
	}
}

func collectPathPoints(p *Path) []Point {
	var pts []Point
	p.Iterate(func(verb PathVerb, coords []float64) {
		if verb == MoveTo || verb == LineTo {
			pts = append(pts, Pt(coords[0], coords[1]))
		}
	})
	return pts
}

// ---------------------------------------------------------------------------
// Budget and tolerance degradation
// ---------------------------------------------------------------------------

func TestAdaptiveFlatten_BudgetCapRelaxesTolerance(t *testing.T) {
	p := circlePath(400)
	const budget = 64
	_, stats := p.FlattenAdaptive(nil, FlattenOptions{Tolerance: 1e-9, MaxPoints: budget})
	if !stats.BudgetExceeded {
		t.Error("expected BudgetExceeded=true")
	}
	if stats.Points > budget {
		t.Errorf("emitted %d points, budget was %d", stats.Points, budget)
	}
	if stats.AchievedTolerance <= 1e-9 {
		t.Errorf("tolerance not relaxed: %g", stats.AchievedTolerance)
	}
}

// TestAdaptiveFlatten_ExactBudgetFit guards the boundary where the natural
// segment count equals the budget exactly: that must count as fitting, not as
// an overshoot that relaxes the tolerance.
func TestAdaptiveFlatten_ExactBudgetFit(t *testing.T) {
	p := circlePath(20)
	_, free := p.FlattenAdaptive(nil, FlattenOptions{Tolerance: 0.3})
	if free.BudgetExceeded {
		t.Fatal("free run should not exceed the default budget")
	}
	exact, stats := p.FlattenAdaptive(nil, FlattenOptions{Tolerance: 0.3, MaxPoints: free.Points})
	if stats.BudgetExceeded {
		t.Errorf("exact-fit budget wrongly relaxed tolerance to %g", stats.AchievedTolerance)
	}
	if exact.NumVerbs() != free.Points {
		t.Errorf("exact-fit verbs=%d, want %d", exact.NumVerbs(), free.Points)
	}
}

func TestAdaptiveFlatten_MillionPointCapFixture(t *testing.T) {
	// A long chain of cubics with a microscopic tolerance would run away
	// without the cap; assert the cap holds and output stays finite.
	p := continuousCubicsPath(500)
	const budget = 1_000_000
	start := time.Now()
	contours, stats := flattenToContours(p, FlattenOptions{Tolerance: 1e-12, MaxPoints: budget})
	elapsed := time.Since(start)

	if stats.Points > budget {
		t.Errorf("points=%d exceed million cap", stats.Points)
	}
	if !stats.BudgetExceeded {
		t.Error("microscopic tolerance should force budget relaxation")
	}
	if elapsed > 30*time.Second {
		t.Errorf("cap fixture took too long: %v", elapsed)
	}
	for _, c := range contours {
		for _, q := range c {
			if !isFinite(q.X) || !isFinite(q.Y) {
				t.Fatalf("non-finite emitted point %v", q)
			}
		}
	}
}

func TestAdaptiveFlatten_ToleranceFallbacks(t *testing.T) {
	p := circlePath(10)
	for _, tol := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, stats := p.FlattenAdaptive(nil, FlattenOptions{Tolerance: tol})
		if !stats.ToleranceFallback {
			t.Errorf("tol=%g: expected ToleranceFallback", tol)
		}
		if stats.AchievedTolerance != defaultFlattenTolerance {
			t.Errorf("tol=%g: achieved=%g, want %g", tol, stats.AchievedTolerance, defaultFlattenTolerance)
		}
		if stats.Points < 4 {
			t.Errorf("tol=%g: degenerate output of %d points", tol, stats.Points)
		}
	}
}

func TestAdaptiveFlatten_NonFiniteAndExtremeCoords(t *testing.T) {
	cases := []struct {
		name  string
		build func() *Path
	}{
		{"NaN quad control", func() *Path {
			q := NewPath()
			q.MoveTo(0, 0)
			q.QuadraticTo(math.NaN(), 5, 10, 0)
			return q
		}},
		{"Inf cubic control", func() *Path {
			q := NewPath()
			q.MoveTo(0, 0)
			q.CubicTo(math.Inf(1), 0, 5, 5, 10, 0)
			return q
		}},
		{"NaN endpoint", func() *Path {
			q := NewPath()
			q.MoveTo(0, 0)
			q.CubicTo(1, 1, 2, 2, math.NaN(), math.NaN())
			return q
		}},
		{"extreme magnitude", func() *Path {
			q := NewPath()
			q.MoveTo(0, 0)
			q.CubicTo(1e200, 1e200, 2e200, -1e200, 3e200, 1e200)
			return q
		}},
		{"NaN move anchor", func() *Path {
			q := NewPath()
			q.MoveTo(math.NaN(), 0)
			q.LineTo(10, 10)
			q.MoveTo(0, 0)
			q.LineTo(5, 5)
			return q
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.build()
			contours, stats := flattenToContours(p, FlattenOptions{Tolerance: 0.25})
			// Must not panic and must not emit non-finite vertices.
			for _, c := range contours {
				for _, q := range c {
					if !isFinite(q.X) || !isFinite(q.Y) {
						t.Fatalf("emitted non-finite point %v", q)
					}
				}
			}
			// Each fixture contains a non-finite/extreme verb: either it
			// is counted as a degenerate curve (recoverable control
			// points), or the whole verb is skipped when its anchor or
			// endpoint is non-finite. Both are documented degradations.
			if stats.DegenerateCurves == 0 && stats.Curves > 0 {
				t.Errorf("curves processed but none marked degenerate: %+v", stats)
			}
		})
	}
}

func TestAdaptiveFlatten_SingularTransformDegradesToChords(t *testing.T) {
	p := circlePath(50)
	singular := Scale(0, 1)
	contours, stats := flattenToContours(p, FlattenOptions{Tolerance: 0.01, Transform: &singular})
	if len(contours) != 1 {
		t.Fatalf("contours=%d", len(contours))
	}
	// Every cubic collapses to its endpoint. The circle's final endpoint
	// equals its start so Close adds no vertex: move + 4 endpoints = 5.
	if len(contours[0]) != 5 {
		t.Errorf("singular transform produced %d verts, want 5 (chords only)", len(contours[0]))
	}
	if stats.Splits != 0 {
		t.Errorf("splits=%d, want 0 under singular transform", stats.Splits)
	}
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

func TestAdaptiveFlatten_ContextCancellation(t *testing.T) {
	p := continuousCubicsPath(200)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled
	_, stats := p.FlattenAdaptive(nil, FlattenOptions{Tolerance: 0.01, Context: ctx})
	if !stats.Canceled {
		t.Error("expected Canceled=true")
	}
}

func TestAdaptiveFlatten_CancelMidway(t *testing.T) {
	p := continuousCubicsPath(400)
	ctx, cancel := context.WithCancel(context.Background())
	var emitted int
	stats := p.FlattenAdaptiveCallback(FlattenOptions{Tolerance: 0.01, Context: ctx}, func(ev FlattenEvent, pt Point) {
		emitted++
		if emitted == 8 {
			cancel()
		}
	})
	if !stats.Canceled {
		t.Error("expected cancellation during emit pass")
	}
}

// ---------------------------------------------------------------------------
// Allocation behavior
// ---------------------------------------------------------------------------

func TestAdaptiveFlatten_CallbackZeroAllocation(t *testing.T) {
	p := continuousCubicsPath(30)
	opts := FlattenOptions{Tolerance: 0.1}
	var sink float64
	// Hoist the closure: a closure capturing by reference is heap-allocated
	// on every construction, which would mask the engine's own allocations.
	fn := func(ev FlattenEvent, pt Point) {
		_ = ev
		sink += pt.X + pt.Y
	}
	p.FlattenAdaptiveCallback(opts, fn) // prime
	allocs := testing.AllocsPerRun(5, func() {
		p.FlattenAdaptiveCallback(opts, fn)
	})
	if allocs != 0 {
		t.Errorf("callback path allocated %v times per run, want 0 (sink=%v)", allocs, sink)
	}
}

func TestAdaptiveFlatten_ReusedStateZeroAllocation(t *testing.T) {
	p := continuousCubicsPath(30)
	state := NewFlattenState()
	opts := FlattenOptions{Tolerance: 0.1}
	// Prime the buffers.
	p.FlattenAdaptive(state, opts)
	allocs := testing.AllocsPerRun(5, func() {
		p.FlattenAdaptive(state, opts)
	})
	if allocs != 0 {
		t.Errorf("reused state allocated %v times per run, want 0", allocs)
	}
}

// ---------------------------------------------------------------------------
// Parity with legacy Flatten
// ---------------------------------------------------------------------------

// TestAdaptiveFlatten_LegacyParity asserts both flatteners approximate the
// same true curve within their declared tolerances and that the adaptive
// output never explodes relative to the legacy point count.
func TestAdaptiveFlatten_LegacyParity(t *testing.T) {
	fixtures := []*Path{circlePath(60), thinEllipsePath(), cuspPath(), continuousCubicsPath(25)}
	for _, p := range fixtures {
		legacy := p.Flatten(0.25)
		contours, stats := flattenToContours(p, FlattenOptions{Tolerance: 0.25})
		if len(contours) != 1 {
			t.Fatalf("expected single contour, got %d", len(contours))
		}
		adaptive := contours[0]
		truth := sampleDense(p, 200)[0]

		legacyErr := hausdorffToPolyline(truth, legacy)
		adaptiveErr := hausdorffToPolyline(truth, adaptive)
		if adaptiveErr > 0.25*1.05 {
			t.Errorf("adaptive Hausdorff %.4f exceeds 0.25", adaptiveErr)
		}
		if legacyErr > 0.25*3 {
			// Legacy cubic metric is looser; document rather than fail on
			// its weaker bound — but catch gross regressions.
			t.Errorf("legacy Hausdorff %.4f unexpectedly large", legacyErr)
		}
		if len(adaptive) > len(legacy)*4 {
			t.Errorf("adaptive pts=%d >> legacy pts=%d", len(adaptive), len(legacy))
		}
		if stats.Points != len(adaptive) {
			t.Errorf("stats.Points=%d != contour verts=%d", stats.Points, len(adaptive))
		}
	}
}

// TestAdaptiveFlatten_EmptyAndNilCallback covers the trivial entry paths.
func TestAdaptiveFlatten_EmptyAndNilCallback(t *testing.T) {
	p := NewPath()
	out, stats := p.FlattenAdaptive(nil, FlattenOptions{Tolerance: 0.1})
	if len(out.Verbs()) != 0 || stats.Points != 0 {
		t.Errorf("empty path produced verbs=%d points=%d", len(out.Verbs()), stats.Points)
	}
	stats = p.FlattenAdaptiveCallback(FlattenOptions{Tolerance: 0.1}, nil)
	if stats.Points != 0 || stats.AchievedTolerance != defaultFlattenTolerance {
		t.Errorf("nil callback stats=%+v", stats)
	}
}

// ---------------------------------------------------------------------------
// FlattenedBounds and FlattenedContains
// ---------------------------------------------------------------------------

func TestFlattenedBounds_TighterThanControlPointBounds(t *testing.T) {
	// A curve whose control point bulges far outside the true curve.
	p := NewPath()
	p.MoveTo(0, 0)
	p.QuadraticTo(50, 100, 100, 0)
	p.Close()

	loose := p.BoundingBox()
	flat, stats := p.FlattenedBounds(FlattenOptions{Tolerance: 0.01})
	if stats.Canceled {
		t.Fatal("canceled")
	}
	// The flattened bbox must sit inside the control-point bbox.
	if flat.Min.X < loose.Min.X-1e-9 || flat.Min.Y < loose.Min.Y-1e-9 ||
		flat.Max.X > loose.Max.X+1e-9 || flat.Max.Y > loose.Max.Y+1e-9 {
		t.Errorf("flat bbox %v extends outside control bbox %v", flat, loose)
	}
	// And the control point (50,100) must be outside the flat bbox.
	if flat.Contains(Pt(50, 100)) {
		t.Errorf("flat bbox %v wrongly contains control point", flat)
	}
}

func TestFlattenedContains_MatchesLegacyContains(t *testing.T) {
	cases := []*Path{circlePath(50), thinEllipsePath(), cuspPath()}
	probes := []Point{{0, 0}, {49, 0}, {51, 0}, {0, 49}, {100, 100}, {-60, 20}, {0.5, 0.5}}
	for _, p := range cases {
		for _, q := range probes {
			got, stats := p.FlattenedContains(q, FlattenOptions{Tolerance: 0.05})
			if stats.Canceled {
				t.Fatal("canceled")
			}
			want := p.Contains(q)
			if got != want {
				t.Errorf("%v contains %v: adaptive=%v legacy=%v", p.Bounds(), q, got, want)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Fill/Stroke opt-in
// ---------------------------------------------------------------------------

func TestContext_AdaptiveFlattenFillParity(t *testing.T) {
	build := func(dc *Context) {
		dc.SetRGB(0, 0, 0)
		dc.DrawCircle(40, 40, 30)
		dc.Fill()
		dc.MoveTo(10, 80)
		dc.CubicTo(30, 20, 60, 120, 90, 60)
		dc.SetLineWidth(2)
		dc.Stroke()
	}

	ref := NewContext(100, 100)
	build(ref)
	opt := NewContext(100, 100, WithAdaptiveFlatten(true))
	if !opt.AdaptiveFlatten() {
		t.Fatal("AdaptiveFlatten flag not set by option")
	}
	build(opt)

	diffPixels, maxDiff := pixmapDiff(ref.pixmap, opt.pixmap)
	total := 100 * 100
	if float64(diffPixels)/float64(total) > 0.02 {
		t.Errorf("%.3f%% pixels differ (max channel diff=%d)", 100*float64(diffPixels)/float64(total), maxDiff)
	}
}

func TestContext_AdaptiveFlattenDefaultUnchanged(t *testing.T) {
	dc := NewContext(20, 20)
	if dc.AdaptiveFlatten() {
		t.Fatal("adaptive flatten must default off")
	}
	dc.SetAdaptiveFlatten(true)
	if !dc.AdaptiveFlatten() {
		t.Fatal("setter did not enable adaptive flatten")
	}
}

func pixmapDiff(a, b *Pixmap) (int, int) {
	if a.width != b.width || a.height != b.height {
		return -1, -1
	}
	diffPixels := 0
	maxDiff := 0
	for i := 0; i < len(a.data); i += 4 {
		// Compare RGB only; alpha stays binary in both paths.
		localMax := 0
		for k := 0; k < 3; k++ {
			av, bv := int(a.data[i+k]), int(b.data[i+k])
			d := av - bv
			if d < 0 {
				d = -d
			}
			if d > localMax {
				localMax = d
			}
		}
		if localMax > 0 {
			diffPixels++
			if localMax > maxDiff {
				maxDiff = localMax
			}
		}
	}
	return diffPixels, maxDiff
}
