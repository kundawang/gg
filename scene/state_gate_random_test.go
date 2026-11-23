package scene_test

// Randomized state-balance scenarios.
//
// Each test builds a random but bounded scene (depth-limited recursion over
// paths, images, text, filters, save/restore, clips and layers), replays it
// through the state gate, and asserts that the gate neither panics nor loops
// forever and that a correct scene stays balanced.
//
// Every scenario is driven by a deterministic seed. On failure the report is
// rendered with the seed and the full event trace, so any failure can be
// reproduced exactly by passing that seed to the same test.
//
// Requirement: no panics, no infinite loops, reproducible seed in the report.

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/gogpu/gg"
	filterpkg "github.com/gogpu/gg/internal/filter"
	"github.com/gogpu/gg/recording"
	"github.com/gogpu/gg/scene"
	"github.com/gogpu/gg/text"
)

// gateRNG wraps math/rand with the bounded helpers the generators need.
type gateRNG struct{ *rand.Rand }

func newGateRNG(seed int64) *gateRNG {
	return &gateRNG{Rand: rand.New(rand.NewSource(seed))} //nolint:gosec // deterministic test RNG
}

// f32 returns a float32 in [-limit, limit].
func (r *gateRNG) f32(limit float32) float32 {
	return (r.Float32()*2 - 1) * limit
}

// coord returns a coordinate in [-limit, limit), used for draw positions.
func (r *gateRNG) coord(limit float32) float32 {
	return r.Float32() * limit
}

// pick returns a random element of xs.
func (r *gateRNG) pick(xs []string) string {
	return xs[r.Intn(len(xs))]
}

// gateTextFace loads the bundled test font once. Tests that need text skip
// when the font is unavailable rather than failing the whole gate.
func gateTextFace(t *testing.T, size float64) (text.Face, bool) {
	t.Helper()
	src, err := text.NewFontSourceFromFile("../text/testdata/ahem.ttf")
	if err != nil {
		return nil, false
	}
	t.Cleanup(func() { _ = src.Close() })
	return src.Face(size), true
}

// gateScenarioConfig bounds a generated scenario.
type gateScenarioConfig struct {
	depth        int // maximum nesting depth
	ops          int // number of top-level operations
	allowText    bool
	allowImages  bool
	allowLayers  bool
	allowFilters bool
}

// randomScene builds a bounded scene from the config.
func randomScene(r *gateRNG, cfg gateScenarioConfig, face text.Face) *scene.Scene {
	s := scene.NewScene()
	randomOps(r, s, cfg, face, 0)
	return s
}

// randomOps emits up to cfg.ops operations, recursing at most cfg.depth deep.
func randomOps(r *gateRNG, s *scene.Scene, cfg gateScenarioConfig, face text.Face, depth int) {
	n := 1 + r.Intn(cfg.ops)
	for range n {
		emitRandomOp(r, s, cfg, face, depth)
	}
}

// emitRandomOp emits one operation, recursing when a nesting op is chosen.
func emitRandomOp(r *gateRNG, s *scene.Scene, cfg gateScenarioConfig, face text.Face, depth int) {
	canRecurse := depth < cfg.depth
	switch r.Intn(10) {
	case 0:
		emitRandomPath(r, s)
	case 1:
		emitRandomImage(r, s, cfg)
	case 2:
		if cfg.allowText && face != nil {
			emitRandomText(r, s, face)
		}
	case 3:
		if cfg.allowFilters {
			emitRandomFilter(r, s, cfg, face, depth)
		}
	case 4:
		if cfg.allowLayers && canRecurse {
			emitRandomLayer(r, s, cfg, face, depth)
		}
	case 5:
		if canRecurse {
			emitRandomClip(r, s, cfg, face, depth)
		}
	case 6:
		emitRandomTransform(r, s)
	case 7:
		if canRecurse {
			emitRandomSaveScope(r, s, cfg, face, depth)
		}
	default:
		emitRandomPath(r, s)
	}
}

// emitRandomPath fills or strokes a random shape.
func emitRandomPath(r *gateRNG, s *scene.Scene) {
	brush := scene.SolidBrush(gg.RGBA{
		R: float64(r.Float32()), G: float64(r.Float32()), B: float64(r.Float32()), A: 1,
	})
	shape := randomShape(r)
	if r.Intn(2) == 0 {
		s.Fill(scene.FillNonZero, scene.IdentityAffine(), brush, shape)
		return
	}
	style := &scene.StrokeStyle{Width: 1 + r.Float32()*4, MiterLimit: 10}
	s.Stroke(style, scene.IdentityAffine(), brush, shape)
}

// randomShape returns one of the shape kinds.
func randomShape(r *gateRNG) scene.Shape {
	cx, cy := r.coord(80), r.coord(80)
	switch r.Intn(5) {
	case 0:
		return scene.NewRectShape(cx, cy, 1+r.Float32()*40, 1+r.Float32()*40)
	case 1:
		return scene.NewCircleShape(cx, cy, 1+r.Float32()*30)
	case 2:
		return scene.NewEllipseShape(cx, cy, 1+r.Float32()*30, 1+r.Float32()*20)
	case 3:
		return scene.NewRoundRectShape(
			scene.Rect{MinX: cx, MinY: cy, MaxX: cx + 20, MaxY: cy + 20}, 4, 4)
	default:
		p := scene.NewPath()
		p.MoveTo(cx, cy)
		p.LineTo(cx+10, cy+5)
		p.QuadTo(cx+5, cy+15, cx, cy+10)
		p.Close()
		return scene.NewPathShape(p)
	}
}

// emitRandomImage draws an image with a random transform.
func emitRandomImage(r *gateRNG, s *scene.Scene, cfg gateScenarioConfig) {
	if !cfg.allowImages {
		return
	}
	img := scene.NewImage(2+r.Intn(8), 2+r.Intn(8))
	img.Data = make([]byte, img.Width*img.Height*4)
	for i := range img.Data {
		img.Data[i] = uint8(r.Intn(256))
	}
	s.DrawImage(img, randomAffine(r))
}

// emitRandomText draws a short string with the test face.
func emitRandomText(r *gateRNG, s *scene.Scene, face text.Face) {
	str := r.pick([]string{"A", "Hi", "gg", "frame", "W"})
	brush := scene.SolidBrush(gg.RGBA{R: 1, G: 1, B: 1, A: 1})
	_ = s.DrawText(str, face, r.coord(80), r.coord(80), brush)
}

// emitRandomLayer pushes a layer (optionally with a clip), emits content, and
// pops the layer. The Push/Pop always pair, so the frame stays balanced.
func emitRandomLayer(r *gateRNG, s *scene.Scene, cfg gateScenarioConfig, face text.Face, depth int) {
	var clip scene.Shape
	if r.Intn(2) == 0 {
		clip = randomShape(r)
	}
	s.PushLayer(scene.BlendMode(r.Intn(3)), r.Float32(), clip)
	randomOps(r, s, cfg, face, depth+1)
	s.PopLayer()
}

// emitRandomClip wraps content in a clip.
func emitRandomClip(r *gateRNG, s *scene.Scene, cfg gateScenarioConfig, face text.Face, depth int) {
	s.PushClip(randomShape(r))
	randomOps(r, s, cfg, face, depth+1)
	s.PopClip()
}

// emitRandomTransform pushes a transform, emits content, and pops it.
func emitRandomTransform(r *gateRNG, s *scene.Scene) {
	s.PushTransform(randomAffine(r))
	s.Fill(scene.FillNonZero, scene.IdentityAffine(),
		scene.SolidBrush(gg.RGBA{B: 1, A: 1}), randomShape(r))
	s.PopTransform()
}

// emitRandomSaveScope models a caller that brackets work with a transform
// push/pop pair — the scene-level analog of a backend Save/Restore.
func emitRandomSaveScope(r *gateRNG, s *scene.Scene, cfg gateScenarioConfig, face text.Face, depth int) {
	s.PushTransform(randomAffine(r))
	s.SetAntiAlias(r.Intn(2) == 0)
	randomOps(r, s, cfg, face, depth+1)
	s.PopTransform()
}

// emitRandomFilter runs a filter over a small pixmap. Filters are stateless,
// but exercising them keeps the random scenario close to real frame work and
// guards against panics on degenerate bounds.
func emitRandomFilter(r *gateRNG, s *scene.Scene, _ gateScenarioConfig, _ text.Face, _ int) {
	w, h := 4+r.Intn(16), 4+r.Intn(16)
	src := gg.NewPixmap(w, h)
	dst := gg.NewPixmap(w, h)
	for y := range h {
		for x := range w {
			src.SetPixel(x, y, gg.RGBA{R: float64(r.Float32()), G: float64(r.Float32()), B: float64(r.Float32()), A: 1})
		}
	}
	bounds := scene.Rect{MinX: 0, MinY: 0, MaxX: float32(w), MaxY: float32(h)}
	switch r.Intn(3) {
	case 0:
		gateBlurFilter(float64(r.Intn(4))).Apply(src, dst, bounds)
	case 1:
		gateColorMatrixFilter(float32(r.NormFloat64())).Apply(src, dst, bounds)
	default:
		gateDropShadowFilter(float64(r.Intn(3))).Apply(src, dst, bounds)
	}
	// Drawing a filtered result as an image keeps the op visible to the data
	// model without requiring a renderer.
	filtered := scene.NewImage(w, h)
	filtered.Data = make([]byte, w*h*4)
	copy(filtered.Data, dst.Data())
	s.DrawImage(filtered, randomAffine(r))
}

// randomAffine returns a bounded affine transform.
func randomAffine(r *gateRNG) scene.Affine {
	return scene.NewAffine(
		0.5+r.f32(0.5), r.f32(0.1), r.coord(100),
		r.f32(0.1), 0.5+r.f32(0.5), r.coord(100),
	)
}

// gateRunScenario replays one scenario through the gate and checks it.
func gateRunScenario(t *testing.T, label string, seed int64, s *scene.Scene) gateReport {
	t.Helper()
	g := newGateRecorder(label, seed)
	r := runWithTimeout(t, seed, func() gateReport {
		return g.ReplayScene(label, s)
	})
	if !r.ok() {
		t.Fatalf("random scenario produced an unbalanced frame (seed=%d)\n%s", seed, r)
	}
	return r
}

// runWithTimeout runs fn under a deadline so a generated scenario that loops
// forever fails the test instead of hanging the suite. The worker goroutine
// never touches *testing.T: it reports panics and results through a channel,
// so a timed-out test cannot race the still-running scenario.
func runWithTimeout(t *testing.T, seed int64, fn func() gateReport) gateReport {
	t.Helper()
	type outcome struct {
		report gateReport
		panic  any
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- outcome{panic: p}
			}
		}()
		done <- outcome{report: fn()}
	}()
	select {
	case res := <-done:
		if res.panic != nil {
			t.Fatalf("random scenario panicked (seed=%d): %v", seed, res.panic)
		}
		if res.report.Frame == 0 {
			t.Fatalf("random scenario returned no frame (seed=%d)", seed)
		}
		return res.report
	case <-time.After(10 * time.Second):
		t.Fatalf("random scenario did not terminate within 10s (seed=%d)", seed)
		return gateReport{}
	}
}

// TestStateGateRandomScenes runs a bounded family of random scenes through the
// gate. A failure names the seed; rerunning with -seed or by narrowing the
// loop reproduces it.
func TestStateGateRandomScenes(t *testing.T) {
	face, haveFace := gateTextFace(t, 14)
	cfg := gateScenarioConfig{
		depth:        3,
		ops:          4,
		allowText:    haveFace,
		allowImages:  true,
		allowLayers:  true,
		allowFilters: true,
	}

	const iterations = 200
	baseSeed := int64(20261123)
	for i := range iterations {
		seed := baseSeed + int64(i)
		r := newGateRNG(seed)
		s := randomScene(r, cfg, face)
		report := gateRunScenario(t, fmt.Sprintf("random-%d", i), seed, s)

		// A generated scene must never leave state open. Category is either
		// balanced or legal-nested depending on how deep the generator nested.
		if report.SaveDepth != 0 || report.ClipDepth != 0 || report.LayerDepth != 0 {
			t.Fatalf("generated scene left state open (seed=%d): %s", seed, report.summary())
		}
	}
}

// TestStateGateRandomDeepNesting stresses the gate with deeper, wider scenes
// and confirms the category stays legal-nested rather than degrading.
func TestStateGateRandomDeepNesting(t *testing.T) {
	face, haveFace := gateTextFace(t, 12)
	cfg := gateScenarioConfig{
		depth:        6,
		ops:          6,
		allowText:    haveFace,
		allowImages:  true,
		allowLayers:  true,
		allowFilters: true,
	}

	baseSeed := int64(77123)
	for i := range 60 {
		seed := baseSeed + int64(i)
		r := newGateRNG(seed)
		s := randomScene(r, cfg, face)
		report := gateRunScenario(t, fmt.Sprintf("deep-%d", i), seed, s)
		if !report.Category.ok() {
			t.Fatalf("seed=%d deep scenario category=%q\n%s", seed, report.Category, report)
		}
	}
}

// TestStateGateRandomCancellation feeds every random scene through the real
// cancellable renderer with an already-canceled context. The renderer must
// return ctx.Err() promptly without panicking, and the gate must still accept
// the (complete) encoding — cancellation aborts a frame, it does not corrupt it.
func TestStateGateRandomCancellation(t *testing.T) {
	cfg := gateScenarioConfig{depth: 2, ops: 3, allowImages: true}
	baseSeed := int64(31337)

	for i := range 20 {
		seed := baseSeed + int64(i)
		r := newGateRNG(seed)
		s := randomScene(r, cfg, nil)

		// The encoding is fixed before cancellation; the gate audits the full
		// frame irrespective of whether rendering was canceled.
		g := newGateRecorder("cancel-encoding", seed)
		report := g.ReplayScene("cancel-encoding", s)
		if !report.ok() {
			t.Fatalf("seed=%d encoding unbalanced under cancellation scenario\n%s", seed, report)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		renderer := scene.NewRenderer(48, 48)
		if renderer == nil {
			t.Fatal("NewRenderer returned nil")
		}
		target := gg.NewPixmap(48, 48)
		err := renderer.RenderWithContext(ctx, target, s)
		renderer.Close()
		if err == nil {
			t.Fatalf("seed=%d: canceled render returned nil error", seed)
		}
	}
}

// --- filter constructors ---------------------------------------------------
//
// Thin wrappers keep the scenario generator free of the internal/filter import
// details while still exercising the real filter implementations.

// gateBlurFilter constructs a Gaussian blur filter.
func gateBlurFilter(radius float64) scene.Filter {
	return filterpkg.NewBlurFilter(radius)
}

// gateColorMatrixFilter constructs a brightness color-matrix filter.
func gateColorMatrixFilter(factor float32) scene.Filter {
	return filterpkg.NewBrightnessFilter(factor)
}

// gateDropShadowFilter constructs a drop-shadow filter.
func gateDropShadowFilter(blur float64) scene.Filter {
	return filterpkg.NewSimpleDropShadow(1, 1, blur)
}

// --- multi-frame and recording scenarios -----------------------------------

// TestGateRandomFrames audits a sequence of random frames on the SAME gate so
// that any state a frame fails to close is caught on the following frame as
// cross-frame contamination, not silently reset between frames.
func TestGateRandomFrames(t *testing.T) {
	face, haveFace := gateTextFace(t, 12)
	cfg := gateScenarioConfig{
		depth:        2,
		ops:          3,
		allowText:    haveFace,
		allowImages:  true,
		allowLayers:  true,
		allowFilters: true,
	}

	const frames = 40
	baseSeed := int64(90001)
	g := newGateRecorder("multi-frame", baseSeed)
	for i := range frames {
		seed := baseSeed + int64(i)
		r := newGateRNG(seed)
		s := randomScene(r, cfg, face)
		report := runWithTimeout(t, seed, func() gateReport {
			return g.ReplayScene(fmt.Sprintf("frame-%d", i), s)
		})
		if !report.ok() {
			t.Fatalf("multi-frame scenario failed at frame %d (seed=%d)\n%s", i, seed, report)
		}
	}
}

// TestGateRandomRecording plays random recordings into the recording backend
// gate. The recording API is a separate state surface (Save/Restore/SetClip/
// ClearClip), so it gets its own randomized sweep.
func TestGateRandomRecording(t *testing.T) {
	const iterations = 80
	baseSeed := int64(55001)
	for i := range iterations {
		seed := baseSeed + int64(i)
		r := newGateRNG(seed)
		report := runWithTimeout(t, seed, func() gateReport {
			return gateRandomRecordingFrame(t, r, fmt.Sprintf("rec-%d", i), seed)
		})
		if !report.ok() {
			t.Fatalf("random recording unbalanced (seed=%d)\n%s", seed, report)
		}
	}
}

// gateRandomRecordingFrame records random balanced clips/saves and plays them
// into a gate recording backend.
func gateRandomRecordingFrame(t *testing.T, r *gateRNG, label string, seed int64) gateReport {
	t.Helper()
	rec := recording.NewRecorder(64, 64)
	depth := 0
	ops := 1 + r.Intn(6)
	for range ops {
		switch r.Intn(6) {
		case 0:
			rec.Save()
			depth++
			rec.Translate(float64(r.Intn(10)), float64(r.Intn(10)))
		case 1:
			if depth > 0 {
				rec.Restore()
				depth--
			}
		case 2:
			rec.DrawRectangle(float64(r.Intn(40)), float64(r.Intn(40)),
				float64(1+r.Intn(20)), float64(1+r.Intn(20)))
			rec.Clip()
			rec.ResetClip()
		case 3:
			rec.SetRGB(r.Float64(), r.Float64(), r.Float64())
			rec.FillRectangle(float64(r.Intn(40)), float64(r.Intn(40)),
				float64(1+r.Intn(20)), float64(1+r.Intn(20)))
		case 4:
			rec.FillRectangle(0, 0, float64(1+r.Intn(40)), float64(1+r.Intn(40)))
		default:
			rec.SetTransform(recording.Translate(float64(r.Intn(10)), float64(r.Intn(10))))
		}
	}
	// Close every scope the generator opened so the recording is balanced.
	for depth > 0 {
		rec.Restore()
		depth--
	}

	g := newGateRecorder(label, seed)
	backend := newGateRecordingBackend(g)
	g.BeginFrame(label)
	if err := rec.FinishRecording().Playback(backend); err != nil {
		g.BackendError("playback: " + err.Error())
	}
	return g.EndFrame()
}
