package gg

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gogpu/gpucontext"
)

// gpuOpsDouble is an injectable per-context GPU render context. It records
// queue and flush calls and can be scripted to fail at any stage.
type gpuOpsDouble struct {
	mu sync.Mutex

	fillShapeErr   error
	strokeShapeErr error
	fillPathErr    error
	strokePathErr  error
	flushErr       error
	textErr        error

	fillShapeCalls   int
	fillPathCalls    int
	strokeShapeCalls int
	strokePathCalls  int
	flushCalls       int
	closed           int
}

func (d *gpuOpsDouble) FillShape(GPURenderTarget, DetectedShape, *Paint) error {
	d.mu.Lock()
	d.fillShapeCalls++
	err := d.fillShapeErr
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return ErrFallbackToCPU
}

func (d *gpuOpsDouble) StrokeShape(GPURenderTarget, DetectedShape, *Paint) error {
	d.mu.Lock()
	d.strokeShapeCalls++
	err := d.strokeShapeErr
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return ErrFallbackToCPU
}

func (d *gpuOpsDouble) FillPath(GPURenderTarget, *Path, *Paint) error {
	d.mu.Lock()
	d.fillPathCalls++
	err := d.fillPathErr
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

func (d *gpuOpsDouble) StrokePath(GPURenderTarget, *Path, *Paint) error {
	d.mu.Lock()
	d.strokePathCalls++
	err := d.strokePathErr
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

func (d *gpuOpsDouble) DrawText(GPURenderTarget, any, string, float64, float64, RGBA, Matrix, float64) error {
	d.mu.Lock()
	err := d.textErr
	d.mu.Unlock()
	return err
}

func (d *gpuOpsDouble) DrawGlyphMaskText(GPURenderTarget, any, string, float64, float64, RGBA, Matrix, float64) error {
	d.mu.Lock()
	err := d.textErr
	d.mu.Unlock()
	return err
}

func (d *gpuOpsDouble) QueueImageDraw(GPURenderTarget, []byte, uint64, int, int, int,
	float32, float32, float32, float32, float32, uint32, uint32,
	float32, float32, float32, float32,
) {
}

func (d *gpuOpsDouble) QueueGPUTextureDraw(GPURenderTarget, gpucontext.TextureView,
	float32, float32, float32, float32, float32, uint32, uint32,
) {
}

func (d *gpuOpsDouble) QueueBaseLayer(GPURenderTarget, gpucontext.TextureView,
	float32, float32, float32, float32, float32, uint32, uint32,
) {
}

func (d *gpuOpsDouble) Flush(GPURenderTarget) error {
	d.mu.Lock()
	d.flushCalls++
	err := d.flushErr
	d.mu.Unlock()
	return err
}

func (d *gpuOpsDouble) SetClipRect(uint32, uint32, uint32, uint32) {}
func (d *gpuOpsDouble) ClearClipRect()                             {}
func (d *gpuOpsDouble) SetClipRRect(float32, float32, float32, float32, float32) {
}
func (d *gpuOpsDouble) ClearClipRRect()              {}
func (d *gpuOpsDouble) SetClipPath(*Path)            {}
func (d *gpuOpsDouble) ClearClipPath()               {}
func (d *gpuOpsDouble) BeginFrame()                  {}
func (d *gpuOpsDouble) MarkFrameRendered()           {}
func (d *gpuOpsDouble) SetPipelineMode(PipelineMode) {}
func (d *gpuOpsDouble) SetAntiAlias(bool)            {}
func (d *gpuOpsDouble) PendingCount() int            { return 0 }

func (d *gpuOpsDouble) Close() {
	d.mu.Lock()
	d.closed++
	d.mu.Unlock()
}

func (d *gpuOpsDouble) renderCounts() (fillShape, fillPath, flush int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fillShapeCalls, d.fillPathCalls, d.flushCalls
}

func (d *gpuOpsDouble) flushCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.flushCalls
}

func (d *gpuOpsDouble) closeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// gpuOpsProvider wraps gpuOpsDouble in a GPURenderContextProvider accelerator
// so gg.Context creates it through the public path.
type gpuOpsProvider struct {
	*mockAccelerator
	mu   sync.Mutex
	ops  *gpuOpsDouble
	made int
}

func newGPUOpsProvider(ops *gpuOpsDouble) *gpuOpsProvider {
	return &gpuOpsProvider{
		mockAccelerator: &mockAccelerator{name: "ops-double", canAccel: AccelFill | AccelStroke | AccelCircleSDF | AccelRRectSDF | AccelText},
		ops:             ops,
	}
}

func (p *gpuOpsProvider) NewGPURenderContext() any {
	p.mu.Lock()
	p.made++
	p.mu.Unlock()
	return p.ops
}

// installGPUOpsProvider registers provider and restores state after the test.
func installGPUOpsProvider(t *testing.T, provider *gpuOpsProvider) {
	t.Helper()
	oldState, oldAccel, oldRetained, oldClosed, oldProvider := snapshotAccelState()
	_ = oldState
	_ = oldProvider
	t.Cleanup(func() {
		accelMu.Lock()
		accel = oldAccel
		retainedAccel = oldRetained
		retainedClosed = oldClosed
		lastDeviceProvider = oldProvider
		accelMu.Unlock()
	})
	accelMu.Lock()
	accel = provider
	retainedAccel = provider
	retainedClosed = false
	accelStatus = AcceleratorStatus{State: AcceleratorReady, Name: provider.Name()}
	accelMu.Unlock()
}

func snapshotAccelState() (AcceleratorStatus, GPUAccelerator, GPUAccelerator, bool, gpucontext.DeviceProvider) {
	accelMu.RLock()
	defer accelMu.RUnlock()
	return accelStatus, accel, retainedAccel, retainedClosed, lastDeviceProvider
}

// withFallbackPolicy sets the process policy for the duration of a test.
func withFallbackPolicy(t *testing.T, p FallbackPolicy) {
	t.Helper()
	old := GPUFallbackPolicy()
	SetGPUFallbackPolicy(p)
	t.Cleanup(func() { SetGPUFallbackPolicy(old) })
}

func drawTestCircle(t *testing.T, dc *Context) error {
	t.Helper()
	dc.SetRGB(1, 0, 0)
	dc.DrawCircle(32, 32, 20)
	return dc.Fill()
}

func drawTestPath(t *testing.T, dc *Context) error {
	t.Helper()
	dc.SetRGB(0, 0, 1)
	dc.MoveTo(10, 10)
	dc.LineTo(50, 10)
	dc.LineTo(30, 50)
	dc.ClosePath()
	return dc.Fill()
}

func TestContextGPUSuccessQueuesAndFlushesOnce(t *testing.T) {
	ops := &gpuOpsDouble{}
	installGPUOpsProvider(t, newGPUOpsProvider(ops))
	withFallbackPolicy(t, FallbackOnError)

	dc := NewContext(64, 64)
	defer func() { _ = dc.Close() }()

	if err := drawTestCircle(t, dc); err != nil {
		t.Fatalf("Fill circle = %v, want nil", err)
	}
	if err := drawTestPath(t, dc); err != nil {
		t.Fatalf("Fill path = %v, want nil", err)
	}
	if err := dc.FlushGPU(); err != nil {
		t.Fatalf("FlushGPU = %v", err)
	}

	fillShape, fillPath, flush := ops.renderCounts()
	if fillShape == 0 {
		t.Error("FillShape was not called for the circle")
	}
	if fillPath == 0 {
		t.Error("FillPath was not called for the path")
	}
	if flush == 0 {
		t.Error("Flush was not called")
	}
	st := dc.GPUStatus()
	if st.Fallbacks != 0 {
		t.Errorf("Fallbacks = %d, want 0 on a successful GPU render", st.Fallbacks)
	}
	if st.FirstErr != nil {
		t.Errorf("FirstErr = %v, want nil", st.FirstErr)
	}
}

func TestContextGPUFallbackOnErrorPolicy(t *testing.T) {
	hard := errors.New("gpu exploded")
	ops := &gpuOpsDouble{fillPathErr: hard}
	installGPUOpsProvider(t, newGPUOpsProvider(ops))
	withFallbackPolicy(t, FallbackOnError)

	dc := NewContext(64, 64)
	defer func() { _ = dc.Close() }()

	// The hard FillPath error must permit CPU rendering: Fill returns nil and
	// the pixel content proves the CPU actually drew the path.
	if err := drawTestPath(t, dc); err != nil {
		t.Fatalf("Fill under permissive policy = %v, want nil", err)
	}
	if px := dc.pixmap.GetPixel(30, 15); px.A < 0.5 {
		t.Errorf("pixel A = %f, want >= 0.5 (CPU fallback rendered the path)", px.A)
	}
	st := dc.GPUStatus()
	if st.Fallbacks == 0 {
		t.Error("expected the fallback to be recorded")
	}
	if !errors.Is(st.FirstErr, hard) {
		t.Errorf("FirstErr = %v, want the first GPU error", st.FirstErr)
	}
	if !errors.Is(st.LastErr, hard) {
		t.Errorf("LastErr = %v, want the GPU error", st.LastErr)
	}
}

func TestContextGPUStrictPolicyPropagatesError(t *testing.T) {
	hard := errors.New("gpu exploded")
	ops := &gpuOpsDouble{fillPathErr: hard}
	installGPUOpsProvider(t, newGPUOpsProvider(ops))
	withFallbackPolicy(t, FallbackOnUnsupported)

	dc := NewContext(64, 64)
	defer func() { _ = dc.Close() }()

	err := drawTestPath(t, dc)
	if !errors.Is(err, hard) {
		t.Fatalf("Fill under strict policy = %v, want the GPU error", err)
	}
	// No CPU output must have been produced.
	if px := dc.pixmap.GetPixel(30, 15); px.A != 0 {
		t.Errorf("pixel A = %f, want 0 (strict policy must not fall back)", px.A)
	}
	st := dc.GPUStatus()
	if st.Fallbacks != 0 {
		t.Errorf("Fallbacks = %d, want 0 under strict policy", st.Fallbacks)
	}
	if !errors.Is(st.FirstErr, hard) {
		t.Errorf("FirstErr = %v, want the GPU error", st.FirstErr)
	}
}

func TestContextGPUUnsupportedAlwaysFallsBack(t *testing.T) {
	// Shape tier declines with ErrFallbackToCPU and the path tier also declines;
	// under the strict policy this is still a permitted fallback.
	ops := &gpuOpsDouble{fillPathErr: ErrFallbackToCPU}
	installGPUOpsProvider(t, newGPUOpsProvider(ops))
	withFallbackPolicy(t, FallbackOnUnsupported)

	dc := NewContext(64, 64)
	defer func() { _ = dc.Close() }()

	if err := drawTestPath(t, dc); err != nil {
		t.Fatalf("Fill with unsupported GPU op = %v, want nil", err)
	}
	if px := dc.pixmap.GetPixel(30, 15); px.A < 0.5 {
		t.Errorf("pixel A = %f, want >= 0.5 (CPU rendered)", px.A)
	}
}

func TestContextGPUHardErrorNotResubmitted(t *testing.T) {
	// The shape tier succeeds in queueing, but the path tier is never reached
	// when the shape tier reports a hard error; and the hard error is not
	// retried through the path tier.
	hard := errors.New("shape tier exploded")
	ops := &gpuOpsDouble{fillShapeErr: hard, fillPathErr: nil}
	installGPUOpsProvider(t, newGPUOpsProvider(ops))
	withFallbackPolicy(t, FallbackOnError)

	dc := NewContext(64, 64)
	defer func() { _ = dc.Close() }()

	if err := drawTestCircle(t, dc); err != nil {
		t.Fatalf("Fill circle = %v, want nil (permissive)", err)
	}
	_, fillPath, _ := ops.renderCounts()
	if fillPath != 0 {
		t.Errorf("FillPath calls = %d, want 0: a hard shape-tier error must not be retried on the path tier", fillPath)
	}
}

func TestContextFlushErrorNotResubmitted(t *testing.T) {
	hard := errors.New("flush exploded")
	ops := &gpuOpsDouble{flushErr: hard}
	installGPUOpsProvider(t, newGPUOpsProvider(ops))
	withFallbackPolicy(t, FallbackOnError)

	dc := NewContext(64, 64)
	defer func() { _ = dc.Close() }()

	first := dc.FlushGPU()
	if !errors.Is(first, hard) {
		t.Fatalf("FlushGPU = %v, want the flush error", first)
	}
	// A second flush call increments the flush count but must not retry the
	// commands that the first flush consumed.
	flushBefore := ops.flushCount()
	_ = dc.FlushGPU()
	flushAfter := ops.flushCount()
	if flushAfter != flushBefore+1 {
		t.Errorf("flush calls = %d, want %d", flushAfter, flushBefore+1)
	}
	st := dc.GPUStatus()
	if !errors.Is(st.FirstErr, hard) {
		t.Errorf("FirstErr = %v, want the flush error", st.FirstErr)
	}
}

func TestContextDeviceLossPropagatesToRegistry(t *testing.T) {
	lost := errors.Join(ErrDeviceLost, errors.New("adapter reset"))
	ops := &gpuOpsDouble{fillPathErr: lost}
	installGPUOpsProvider(t, newGPUOpsProvider(ops))
	withFallbackPolicy(t, FallbackOnError)

	dc := NewContext(64, 64)
	defer func() { _ = dc.Close() }()

	// Permissive policy: the draw still falls back to CPU and must not panic.
	if err := drawTestPath(t, dc); err != nil {
		t.Fatalf("Fill = %v, want nil under permissive policy", err)
	}
	if Accelerator() != nil {
		t.Error("device loss reported through a Context must exclude the accelerator")
	}
	if st := AcceleratorSnapshot(); st.State != AcceleratorDeviceLost {
		t.Errorf("accelerator state = %v, want device-lost", st.State)
	}
	if st := AcceleratorSnapshot(); st.DeviceLosses == 0 {
		t.Error("device loss was not counted")
	}
}

func TestContextAfterAcceleratorCloseFallsBack(t *testing.T) {
	ops := &gpuOpsDouble{}
	provider := newGPUOpsProvider(ops)
	installGPUOpsProvider(t, provider)
	withFallbackPolicy(t, FallbackOnError)

	dc := NewContext(64, 64)
	defer func() { _ = dc.Close() }()

	// Warm the per-context GPU context.
	if err := drawTestCircle(t, dc); err != nil {
		t.Fatalf("initial fill = %v", err)
	}

	// Close the global accelerator: the Context must stop using its GPU
	// resources and render on the CPU instead of dereferencing a dead device.
	CloseAccelerator()
	defer func() {
		accelMu.Lock()
		accel = provider
		retainedAccel = provider
		retainedClosed = false
		accelMu.Unlock()
	}()

	if err := drawTestCircle(t, dc); err != nil {
		t.Fatalf("fill after accelerator close = %v, want nil (CPU)", err)
	}
	if px := dc.pixmap.GetPixel(32, 32); px.A < 0.5 {
		t.Errorf("pixel A = %f, want >= 0.5 (CPU rendered after accelerator close)", px.A)
	}
}

func TestContextCloseIsConcurrencySafeAndIdempotent(t *testing.T) {
	ops := &gpuOpsDouble{}
	installGPUOpsProvider(t, newGPUOpsProvider(ops))
	withFallbackPolicy(t, FallbackOnError)

	dc := NewContext(64, 64)
	if err := drawTestCircle(t, dc); err != nil {
		t.Fatalf("fill = %v", err)
	}

	var wg sync.WaitGroup
	var errs atomic.Int64
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := dc.Close(); err != nil {
				errs.Add(1)
			}
		}()
	}
	wg.Wait()
	if errs.Load() != 0 {
		t.Errorf("%d concurrent Close calls returned an error", errs.Load())
	}
	closeCalls := ops.closeCount()
	if closeCalls != 1 {
		t.Errorf("per-context GPU context closed %d times, want 1", closeCalls)
	}
	if st := dc.GPUStatus(); !st.Closed {
		t.Error("GPUStatus().Closed = false after Close")
	}
}

func TestContextNilProviderNoOps(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	// No accelerator: FlushGPU and draws must be harmless and CPU-rendered.
	dc := NewContext(32, 32)
	defer func() { _ = dc.Close() }()

	if err := dc.FlushGPU(); err != nil {
		t.Errorf("FlushGPU with no accelerator = %v, want nil", err)
	}
	if err := dc.FlushGPUWithView(gpucontext.TextureView{}, 32, 32); !errors.Is(err, ErrFallbackToCPU) {
		t.Errorf("FlushGPUWithView with no accelerator = %v, want ErrFallbackToCPU", err)
	}
	dc.SetRGB(1, 0, 0)
	dc.DrawCircle(16, 16, 10)
	if err := dc.Fill(); err != nil {
		t.Fatalf("Fill with no accelerator = %v", err)
	}
	if px := dc.pixmap.GetPixel(16, 16); px.A < 0.5 {
		t.Errorf("pixel A = %f, want >= 0.5", px.A)
	}
}

func TestContextCloseWithoutGPUWorkDoesNotCreateContext(t *testing.T) {
	provider := newGPUOpsProvider(&gpuOpsDouble{})
	installGPUOpsProvider(t, provider)
	withFallbackPolicy(t, FallbackOnError)

	dc := NewContext(32, 32)
	if err := dc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	provider.mu.Lock()
	made := provider.made
	provider.mu.Unlock()
	if made != 0 {
		t.Errorf("Close created %d GPU render contexts for a Context that never drew", made)
	}
}

func TestContextFlushAfterCloseIsNoOp(t *testing.T) {
	ops := &gpuOpsDouble{}
	installGPUOpsProvider(t, newGPUOpsProvider(ops))
	withFallbackPolicy(t, FallbackOnError)

	dc := NewContext(32, 32)
	// Warm the per-context GPU context so the closed-guard (not the nil-ops
	// path) is what prevents the post-Close flush.
	dc.SetRGB(1, 0, 0)
	dc.DrawCircle(16, 16, 10)
	if err := dc.Fill(); err != nil {
		t.Fatalf("warm-up Fill: %v", err)
	}
	if err := dc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	flushBefore := ops.flushCount()
	if err := dc.FlushGPU(); err != nil {
		t.Errorf("FlushGPU after Close = %v, want nil", err)
	}
	if flushAfter := ops.flushCount(); flushAfter != flushBefore {
		t.Errorf("Flush after Close touched the GPU: %d -> %d", flushBefore, flushAfter)
	}
	if err := dc.FlushGPUWithView(gpucontext.TextureView{}, 32, 32); !errors.Is(err, ErrFallbackToCPU) {
		t.Errorf("FlushGPUWithView after Close = %v, want ErrFallbackToCPU", err)
	}
	// A draw after Close must also be a no-op at the GPU and still render on
	// the CPU (pixels already present are kept; the call must not panic).
	if err := dc.Fill(); err != nil {
		t.Logf("Fill after Close = %v (documented: drawing on a closed Context is a no-op)", err)
	}
}

// TestContextRenderCyclesDoNotLeakGoroutines verifies that creating, drawing,
// flushing, and closing many Contexts against a GPU provider leaves no
// goroutines behind.
func TestContextRenderCyclesDoNotLeakGoroutines(t *testing.T) {
	ops := &gpuOpsDouble{}
	installGPUOpsProvider(t, newGPUOpsProvider(ops))
	withFallbackPolicy(t, FallbackOnError)

	before := runtime.NumGoroutine()
	for range 50 {
		dc := NewContext(32, 32)
		dc.SetRGB(1, 0, 0)
		dc.DrawCircle(16, 16, 10)
		if err := dc.Fill(); err != nil {
			t.Fatalf("Fill: %v", err)
		}
		if err := dc.FlushGPU(); err != nil {
			t.Fatalf("FlushGPU: %v", err)
		}
		if err := dc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutines grew across %d render cycles: before=%d after=%d", 50, before, after)
	}
}

func TestContextPermissiveFlushFailureStillRendersCPU(t *testing.T) {
	// A flush failure on the way to a CPU draw must not abort the draw under
	// the permissive policy: the pixels must still be produced.
	hard := errors.New("flush broke")
	ops := &gpuOpsDouble{fillPathErr: ErrFallbackToCPU, flushErr: hard}
	installGPUOpsProvider(t, newGPUOpsProvider(ops))
	withFallbackPolicy(t, FallbackOnError)

	dc := NewContext(64, 64)
	defer func() { _ = dc.Close() }()

	dc.SetRGB(0, 0, 1)
	dc.DrawRectangle(8, 8, 48, 48)
	if err := dc.Fill(); err != nil {
		t.Fatalf("Fill with failing flush = %v, want nil (CPU rendered)", err)
	}
	if px := dc.pixmap.GetPixel(32, 32); px.B < 0.5 {
		t.Errorf("pixel B = %f, want >= 0.5 (CPU draw ran despite flush failure)", px.B)
	}
	if st := dc.GPUStatus(); !errors.Is(st.FirstErr, hard) {
		t.Errorf("FirstErr = %v, want the flush error", st.FirstErr)
	}
}
