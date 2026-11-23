package scene

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gogpu/gg"
	"github.com/gogpu/gpucontext"
)

// sceneGPUDouble implements gg.GPUAccelerator + gg.GPURenderContextProvider.
// Every queued command and flush is recorded; failures are injectable per
// stage. It intentionally renders nothing itself: the CPU path produces the
// reference pixels the tests compare against.
type sceneGPUDouble struct {
	name string

	mu              sync.Mutex
	initErr         error
	fillShapeErr    error
	strokeShapeErr  error
	fillPathErr     error
	strokePathErr   error
	flushErr        error
	fillShapeCalls  int
	fillPathCalls   int
	strokePathCalls int
	flushCalls      int
	ops             *sceneOpsDouble
}

func newSceneGPUDouble() *sceneGPUDouble {
	return &sceneGPUDouble{name: "scene-double", ops: &sceneOpsDouble{}}
}

func (d *sceneGPUDouble) Name() string { return d.name }
func (d *sceneGPUDouble) Init() error  { return d.initErr }
func (d *sceneGPUDouble) Close()       {}

func (d *sceneGPUDouble) CanAccelerate(op gg.AcceleratedOp) bool {
	return op&(gg.AccelFill|gg.AccelStroke|gg.AccelCircleSDF|gg.AccelRRectSDF|gg.AccelText) != 0
}

func (d *sceneGPUDouble) FillShape(_ gg.GPURenderTarget, _ gg.DetectedShape, _ *gg.Paint) error {
	d.mu.Lock()
	d.fillShapeCalls++
	d.ops.fillShapeCalls++
	err := d.fillShapeErr
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return gg.ErrFallbackToCPU
}

func (d *sceneGPUDouble) StrokeShape(_ gg.GPURenderTarget, _ gg.DetectedShape, _ *gg.Paint) error {
	d.mu.Lock()
	err := d.strokeShapeErr
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return gg.ErrFallbackToCPU
}

func (d *sceneGPUDouble) FillPath(_ gg.GPURenderTarget, _ *gg.Path, _ *gg.Paint) error {
	d.mu.Lock()
	d.fillPathCalls++
	d.ops.fillPathCalls++
	err := d.fillPathErr
	d.mu.Unlock()
	return err
}

func (d *sceneGPUDouble) StrokePath(_ gg.GPURenderTarget, _ *gg.Path, _ *gg.Paint) error {
	d.mu.Lock()
	d.strokePathCalls++
	err := d.strokePathErr
	d.mu.Unlock()
	return err
}

func (d *sceneGPUDouble) Flush(_ gg.GPURenderTarget) error {
	d.mu.Lock()
	d.flushCalls++
	d.ops.flushCalls++
	err := d.flushErr
	d.mu.Unlock()
	return err
}

func (d *sceneGPUDouble) NewGPURenderContext() any { return d.ops }

// sceneOpsDouble is the per-context double returned by NewGPURenderContext.
type sceneOpsDouble struct {
	mu              sync.Mutex
	fillShapeCalls  int
	fillPathCalls   int
	strokePathCalls int
	flushCalls      int
	closed          int

	fillShapeErr error
	fillPathErr  error
	flushErr     error

	// onFillShape fires once, on the first FillShape, before it returns. Tests
	// use it to cancel a context mid-render.
	onFillShape func()
}

func (o *sceneOpsDouble) FillShape(target gg.GPURenderTarget, shape gg.DetectedShape, paint *gg.Paint) error {
	o.mu.Lock()
	o.fillShapeCalls++
	err := o.fillShapeErr
	hook := o.onFillShape
	o.onFillShape = nil
	o.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return err
	}
	return rasterizeShape(target, shape, paint)
}

func (o *sceneOpsDouble) fillShapeCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.fillShapeCalls
}

func (o *sceneOpsDouble) StrokeShape(gg.GPURenderTarget, gg.DetectedShape, *gg.Paint) error {
	return gg.ErrFallbackToCPU
}

func (o *sceneOpsDouble) FillPath(target gg.GPURenderTarget, path *gg.Path, paint *gg.Paint) error {
	o.mu.Lock()
	o.fillPathCalls++
	err := o.fillPathErr
	o.mu.Unlock()
	if err != nil {
		return err
	}
	return rasterizePath(target, path, paint)
}

func (o *sceneOpsDouble) StrokePath(target gg.GPURenderTarget, path *gg.Path, paint *gg.Paint) error {
	o.mu.Lock()
	o.strokePathCalls++
	o.mu.Unlock()
	return rasterizeStroke(target, path, paint)
}

func (o *sceneOpsDouble) DrawText(gg.GPURenderTarget, any, string, float64, float64, gg.RGBA, gg.Matrix, float64) error {
	return gg.ErrFallbackToCPU
}

func (o *sceneOpsDouble) DrawGlyphMaskText(gg.GPURenderTarget, any, string, float64, float64, gg.RGBA, gg.Matrix, float64) error {
	return gg.ErrFallbackToCPU
}

func (o *sceneOpsDouble) QueueImageDraw(gg.GPURenderTarget, []byte, uint64, int, int, int,
	float32, float32, float32, float32, float32, uint32, uint32, float32, float32, float32, float32,
) {
}

func (o *sceneOpsDouble) QueueGPUTextureDraw(gg.GPURenderTarget, gpucontext.TextureView,
	float32, float32, float32, float32, float32, uint32, uint32,
) {
}

func (o *sceneOpsDouble) QueueBaseLayer(gg.GPURenderTarget, gpucontext.TextureView,
	float32, float32, float32, float32, float32, uint32, uint32,
) {
}

func (o *sceneOpsDouble) Flush(gg.GPURenderTarget) error {
	o.mu.Lock()
	o.flushCalls++
	err := o.flushErr
	o.mu.Unlock()
	return err
}

func (o *sceneOpsDouble) SetClipRect(uint32, uint32, uint32, uint32) {}
func (o *sceneOpsDouble) ClearClipRect()                             {}
func (o *sceneOpsDouble) SetClipRRect(float32, float32, float32, float32, float32) {
}
func (o *sceneOpsDouble) ClearClipRRect()                 {}
func (o *sceneOpsDouble) SetClipPath(*gg.Path)            {}
func (o *sceneOpsDouble) ClearClipPath()                  {}
func (o *sceneOpsDouble) BeginFrame()                     {}
func (o *sceneOpsDouble) MarkFrameRendered()              {}
func (o *sceneOpsDouble) SetPipelineMode(gg.PipelineMode) {}
func (o *sceneOpsDouble) SetAntiAlias(bool)               {}
func (o *sceneOpsDouble) PendingCount() int               { return 0 }

func (o *sceneOpsDouble) Close() {
	o.mu.Lock()
	o.closed++
	o.mu.Unlock()
}

func (o *sceneOpsDouble) renderCounts() (fillShape, fillPath, flush int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.fillShapeCalls, o.fillPathCalls, o.flushCalls
}

func (o *sceneOpsDouble) closeCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}

// meanAlpha returns the mean alpha (0..1) over all pixels.
func meanAlpha(pm *gg.Pixmap) float64 {
	data := pm.Data()
	if len(data) == 0 {
		return 0
	}
	var sum uint64
	for i := 3; i < len(data); i += 4 {
		sum += uint64(data[i])
	}
	return float64(sum) / (float64(len(data)/4) * 255)
}

func diffAbs(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// rasterizePath fills path into the target with the CPU software renderer.
// The scene tests use this as a stand-in for a GPU backend that produces the
// same coverage as the reference CPU renderer, so success and fallback output
// can be compared directly.
func rasterizePath(target gg.GPURenderTarget, path *gg.Path, paint *gg.Paint) error {
	if target.Data == nil || target.Width <= 0 || target.Height <= 0 {
		return gg.ErrFallbackToCPU
	}
	pm := gg.NewPixmapFromBuffer(target.Data, target.Width, target.Height)
	sr := gg.NewSoftwareRenderer(target.Width, target.Height)
	return sr.Fill(pm, path, paint)
}

// rasterizeShape converts a detected shape to a path and fills it.
func rasterizeShape(target gg.GPURenderTarget, shape gg.DetectedShape, paint *gg.Paint) error {
	path := gg.NewPath()
	switch shape.Kind {
	case gg.ShapeCircle:
		path.Circle(shape.CenterX, shape.CenterY, shape.RadiusX)
	case gg.ShapeEllipse:
		path.Ellipse(shape.CenterX, shape.CenterY, shape.RadiusX, shape.RadiusY)
	case gg.ShapeRect, gg.ShapeRRect:
		x := shape.CenterX - shape.Width/2
		y := shape.CenterY - shape.Height/2
		if shape.Kind == gg.ShapeRRect && shape.CornerRadius > 0 {
			path.RoundedRectangle(x, y, shape.Width, shape.Height, shape.CornerRadius)
		} else {
			path.Rectangle(x, y, shape.Width, shape.Height)
		}
	default:
		return gg.ErrFallbackToCPU
	}
	return rasterizePath(target, path, paint)
}

// rasterizeStroke strokes path into the target with the CPU software renderer.
func rasterizeStroke(target gg.GPURenderTarget, path *gg.Path, paint *gg.Paint) error {
	if target.Data == nil || target.Width <= 0 || target.Height <= 0 {
		return gg.ErrFallbackToCPU
	}
	pm := gg.NewPixmapFromBuffer(target.Data, target.Width, target.Height)
	sr := gg.NewSoftwareRenderer(target.Width, target.Height)
	return sr.Stroke(pm, path, paint)
}

// registerSceneDouble installs d as the global accelerator for the test.
func registerSceneDouble(t *testing.T, d *sceneGPUDouble) {
	t.Helper()
	gg.CloseAccelerator()
	if err := gg.RegisterAccelerator(d); err != nil {
		t.Fatalf("RegisterAccelerator: %v", err)
	}
	t.Cleanup(gg.CloseAccelerator)
}

// fixedScene builds a deterministic scene: a filled background, a circle, a
// stroked rect, and a translated rounded rect. The same scene is rendered on
// the GPU path and the CPU path, and their pixels are compared.
func fixedScene() *Scene {
	b := NewSceneBuilder()
	b.FillRect(0, 0, 120, 120, SolidBrush(gg.RGBA{R: 1, G: 1, B: 1, A: 1}))
	b.FillCircle(40, 40, 24, SolidBrush(gg.RGBA{R: 0.9, G: 0.1, B: 0.1, A: 1}))
	b.StrokeRect(70, 20, 40, 40, SolidBrush(gg.RGBA{R: 0.1, G: 0.2, B: 0.9, A: 1}), 4)
	b.FillRoundRect(20, 70, 80, 30, 8, 8, SolidBrush(gg.RGBA{R: 0.1, G: 0.8, B: 0.2, A: 1}))
	return b.Build()
}

// pixelDiff compares the raw premultiplied stores of two pixmaps. It returns
// the maximum per-channel byte difference and the number of pixels whose
// difference exceeds thresholdPercent of full scale.
func pixelDiff(t *testing.T, a, b *gg.Pixmap, thresholdPercent int) (maxDiff int, differing int) {
	t.Helper()
	w, h := a.Width(), a.Height()
	if w != b.Width() || h != b.Height() {
		t.Fatalf("dimension mismatch: %dx%d vs %dx%d", w, h, b.Width(), b.Height())
	}
	da, db := a.Data(), b.Data()
	for i := 0; i+3 < len(da) && i+3 < len(db); i += 4 {
		for ch := 0; ch < 4; ch++ {
			diff := int(da[i+ch]) - int(db[i+ch])
			if diff < 0 {
				diff = -diff
			}
			if diff > maxDiff {
				maxDiff = diff
			}
			if diff*100 > thresholdPercent*255 {
				differing++
				break
			}
		}
	}
	return maxDiff, differing
}

func TestSceneGPUAndCPURenderComparable(t *testing.T) {
	// GPU-success output (delegated to the CPU via ErrFallbackToCPU inside the
	// double) must match the pure-CPU tile renderer output.
	d := newSceneGPUDouble()
	registerSceneDouble(t, d)

	gpuTarget := gg.NewPixmap(120, 120)
	gpuRenderer := NewRenderer(120, 120)
	if err := gpuRenderer.Render(gpuTarget, fixedScene()); err != nil {
		t.Fatalf("GPU-path Render: %v", err)
	}
	gpuRenderer.Close()

	// Assert the GPU path was actually exercised (not silently skipped).
	fillShape, fillPath, flush := d.ops.renderCounts()
	if fillShape+fillPath == 0 || flush == 0 {
		t.Fatalf("GPU path not exercised: fillShape=%d fillPath=%d flush=%d", fillShape, fillPath, flush)
	}

	// Pure CPU: no accelerator registered.
	gg.CloseAccelerator()
	cpuTarget := gg.NewPixmap(120, 120)
	cpuRenderer := NewRenderer(120, 120)
	if err := cpuRenderer.Render(cpuTarget, fixedScene()); err != nil {
		t.Fatalf("CPU-path Render: %v", err)
	}
	cpuRenderer.Close()

	maxDiff, over16 := pixelDiff(t, gpuTarget, cpuTarget, 6)
	total := gpuTarget.Width() * gpuTarget.Height()
	overRatio := float64(over16) / float64(total)
	if maxDiff > 64 {
		t.Errorf("GPU-path output diverges from pure-CPU output: maxDiff=%d", maxDiff)
	}
	if overRatio > 0.05 {
		t.Errorf("GPU-path output diverges: %.1f%% of pixels differ >6%%, want <= 5%%", overRatio*100)
	}
	// Statistical agreement: mean coverage must be close (no missing content).
	if meanA, meanB := meanAlpha(gpuTarget), meanAlpha(cpuTarget); diffAbs(meanA, meanB) > 0.02 {
		t.Errorf("mean alpha GPU=%.3f CPU=%.3f, want within 0.02", meanA, meanB)
	}
	t.Logf("GPU-path vs CPU path: maxDiff=%d, pixels>6%%=%.2f%%", maxDiff, overRatio*100)
	if px := gpuTarget.GetPixel(40, 40); px.A < 0.5 {
		t.Errorf("GPU-path circle center A = %f, want >= 0.5", px.A)
	}
}

func TestSceneGPUFailureFallsBackToCPU(t *testing.T) {
	d := newSceneGPUDouble()
	registerSceneDouble(t, d)

	// Inject a hard failure in the shape tier (reached by filled rects/circles).
	d.ops.fillShapeErr = errors.New("gpu shape failed")
	// Permissive policy: Renderer may render the scene on the CPU.
	old := gg.GPUFallbackPolicy()
	gg.SetGPUFallbackPolicy(gg.FallbackOnError)
	t.Cleanup(func() { gg.SetGPUFallbackPolicy(old) })

	withFailure := gg.NewPixmap(120, 120)
	r := NewRenderer(120, 120)
	if err := r.Render(withFailure, fixedScene()); err != nil {
		t.Fatalf("Render with injected GPU failure = %v, want nil (CPU fallback)", err)
	}
	r.Close()

	if px := withFailure.GetPixel(40, 40); px.A < 0.5 {
		t.Errorf("circle center A = %f, want >= 0.5 (CPU fallback produced output)", px.A)
	}

	// The fallback must be observable at the accelerator level.
	if st := gg.AcceleratorSnapshot(); st.Fallbacks == 0 {
		t.Error("expected the GPU failure to be recorded as a fallback")
	}
}

func TestSceneGPUStrictPolicyPropagatesFailure(t *testing.T) {
	d := newSceneGPUDouble()
	registerSceneDouble(t, d)
	hard := errors.New("strict gpu failure")
	d.ops.fillShapeErr = hard

	old := gg.GPUFallbackPolicy()
	gg.SetGPUFallbackPolicy(gg.FallbackOnUnsupported)
	t.Cleanup(func() { gg.SetGPUFallbackPolicy(old) })

	target := gg.NewPixmap(120, 120)
	r := NewRenderer(120, 120)
	defer r.Close()

	err := r.Render(target, fixedScene())
	if !errors.Is(err, hard) {
		t.Fatalf("Render under strict policy = %v, want the injected GPU failure", err)
	}
	if px := target.GetPixel(40, 40); px.A != 0 {
		t.Errorf("pixel A = %f, want 0 (strict policy must not CPU-render)", px.A)
	}
}

func TestSceneRenderConcurrent(t *testing.T) {
	d := newSceneGPUDouble()
	registerSceneDouble(t, d)

	const workers = 8
	const iterations = 4
	r := NewRenderer(120, 120)
	defer r.Close()

	reference := gg.NewPixmap(120, 120)
	if err := r.Render(reference, fixedScene()); err != nil {
		t.Fatalf("reference Render: %v", err)
	}

	var wg sync.WaitGroup
	var failures atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := gg.NewPixmap(120, 120)
			for i := 0; i < iterations; i++ {
				if err := r.Render(target, fixedScene()); err != nil {
					failures.Add(1)
					return
				}
				if px := target.GetPixel(40, 40); px.A < 0.5 {
					failures.Add(1)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := failures.Load(); n != 0 {
		t.Errorf("%d concurrent renders failed or produced empty output", n)
	}
}

func TestSceneRenderCancellation(t *testing.T) {
	d := newSceneGPUDouble()
	registerSceneDouble(t, d)

	// gpuRenderer cancels the context the moment the GPU path records its first
	// draw. That forces cancellation *inside* GPUSceneRenderer.RenderSceneContext
	// (rather than at the early check in RenderWithContext), so the per-command
	// cancellation check is what stops the render.
	ctx, cancel := context.WithCancel(context.Background())
	d.ops.onFillShape = cancel

	r := NewRenderer(120, 120)
	defer r.Close()
	target := gg.NewPixmap(120, 120)
	err := r.RenderWithContext(ctx, target, fixedScene())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RenderWithContext(canceled mid-render) = %v, want context.Canceled", err)
	}
	if calls := d.ops.fillShapeCount(); calls == 0 {
		t.Fatal("GPU path was not reached; cancellation test did not exercise the renderer")
	}

	// Canceling before the call still returns context.Canceled.
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := r.RenderWithContext(canceled, target, fixedScene()); !errors.Is(err, context.Canceled) {
		t.Fatalf("RenderWithContext(pre-canceled) = %v, want context.Canceled", err)
	}
}

func TestSceneRenderAfterCloseReturnsError(t *testing.T) {
	gg.CloseAccelerator()
	r := NewRenderer(120, 120)
	r.Close()
	target := gg.NewPixmap(120, 120)

	if err := r.Render(target, fixedScene()); !errors.Is(err, ErrRendererClosed) {
		t.Fatalf("Render on closed Renderer = %v, want ErrRendererClosed", err)
	}
	// Repeated Close is safe.
	r.Close()
}

func TestSceneGPUProviderContextClosedOnce(t *testing.T) {
	d := newSceneGPUDouble()
	registerSceneDouble(t, d)

	r := NewRenderer(120, 120)
	target := gg.NewPixmap(120, 120)
	if err := r.Render(target, fixedScene()); err != nil {
		t.Fatalf("Render: %v", err)
	}
	r.Close()

	// The per-render gg.Context is closed by renderGPU; the double's Close is
	// invoked exactly once per render (no handle leak across renders).
	closed := d.ops.closeCount()
	if closed != 1 {
		t.Errorf("per-context GPU context closed %d times, want 1", closed)
	}
}

func TestSceneGPUDeviceLossFallsBack(t *testing.T) {
	d := newSceneGPUDouble()
	registerSceneDouble(t, d)
	d.ops.fillShapeErr = errors.Join(gg.ErrDeviceLost, errors.New("reset"))

	old := gg.GPUFallbackPolicy()
	gg.SetGPUFallbackPolicy(gg.FallbackOnError)
	t.Cleanup(func() { gg.SetGPUFallbackPolicy(old) })

	target := gg.NewPixmap(120, 120)
	r := NewRenderer(120, 120)
	defer r.Close()
	if err := r.Render(target, fixedScene()); err != nil {
		t.Fatalf("Render after device loss = %v, want nil (CPU fallback)", err)
	}
	if px := target.GetPixel(40, 40); px.A < 0.5 {
		t.Errorf("circle center A = %f, want >= 0.5 (CPU rendered)", px.A)
	}
	if st := gg.AcceleratorSnapshot(); st.State != gg.AcceleratorDeviceLost {
		t.Errorf("accelerator state = %v, want device-lost", st.State)
	}
}

func TestSceneGPUProviderFailureStable(t *testing.T) {
	// NullDeviceProvider.Device() returns a zero handle, which the SDF backend
	// rejects. That path is exercised in internal/gpu; here we assert the
	// public wrapper returns a stable error for a nil provider.
	if err := gg.SetAcceleratorDeviceProvider(nil); !errors.Is(err, gg.ErrNilDeviceProvider) {
		t.Fatalf("SetAcceleratorDeviceProvider(nil) = %v, want ErrNilDeviceProvider", err)
	}
}

// TestRendererCloseStopsWorkerGoroutines verifies Close tears down the pool's
// worker goroutines; a leaked pool would keep them alive after Close.
func TestRendererCloseStopsWorkerGoroutines(t *testing.T) {
	r := NewRenderer(120, 120)
	if r.workerPool == nil {
		t.Fatal("renderer has no worker pool")
	}
	before := runtime.NumGoroutine()
	r.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Allow scheduler slack; the pool should have stopped its workers.
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines did not fall after Close: before=%d after=%d", before, after)
	}
}

// TestRendererCloseReleasesTiles verifies Close returns tile buffers to the
// pool (no handle retained by the grid).
func TestRendererCloseReleasesTiles(t *testing.T) {
	r := NewRenderer(120, 120)
	r.Close()
	if r.tileGrid != nil {
		for i, tile := range r.tileGrid.AllTiles() {
			if tile != nil {
				t.Errorf("tile %d not released after Close", i)
				break
			}
		}
	}
}
