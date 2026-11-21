package gg

import (
	"testing"
)

// layerGPUAccelerator is a test accelerator whose FillPath rasterizes into the
// target pixmap through the software renderer, emulating a backend that
// resolves its output to the CPU buffer at Flush time. It lets the tests check
// that layer compositing sees GPU-produced pixels and that the CPU compositor
// applies the same transform on both paths.
type layerGPUAccelerator struct {
	mockAccelerator
	fillCalls  int
	flushCalls int
	flushErr   error // if non-nil, Flush reports it (fallback simulation)
}

func (a *layerGPUAccelerator) CanAccelerate(op AcceleratedOp) bool {
	return op&AccelFill != 0
}

func (a *layerGPUAccelerator) FillPath(target GPURenderTarget, path *Path, paint *Paint) error {
	a.fillCalls++
	pm := NewPixmapFromBuffer(target.Data, target.Width, target.Height)
	sr := NewSoftwareRenderer(target.Width, target.Height)
	return sr.Fill(pm, path, paint)
}

func (a *layerGPUAccelerator) Flush(GPURenderTarget) error {
	a.flushCalls++
	return a.flushErr
}

// layerGPUContextProvider also exposes a per-context render context so the
// gg.Context uses the modern per-context route (with PendingCount) rather than
// the global accelerator route.
type layerGPUContextProvider struct {
	*layerGPUAccelerator
	context *layerGPUContext
}

func (p *layerGPUContextProvider) NewGPURenderContext() any { return p.context }

// layerGPUContext implements gpuContextOps and counts pending draws the way the
// real GPURenderContext does.
type layerGPUContext struct {
	legacyGPUContext
	pending     int
	fillCalls   int
	flushCalls  int
	flushTarget GPURenderTarget
}

// FillPath records a pending draw and rasterizes it into the target pixmap,
// matching the real queue-then-resolve contract closely enough to read pixels.
func (c *layerGPUContext) FillPath(target GPURenderTarget, path *Path, paint *Paint) error {
	c.fillCalls++
	c.pending++
	pm := NewPixmapFromBuffer(target.Data, target.Width, target.Height)
	sr := NewSoftwareRenderer(target.Width, target.Height)
	return sr.Fill(pm, path, paint)
}

func (c *layerGPUContext) Flush(target GPURenderTarget) error {
	c.flushCalls++
	c.pending = 0
	c.flushTarget = target
	return nil
}

func (c *layerGPUContext) PendingCount() int { return c.pending }

// register installs the provider/context pair and returns the per-context ops.
func (p *layerGPUContextProvider) register(t *testing.T) *layerGPUContext {
	t.Helper()
	resetAccelerator()
	ctx := &layerGPUContext{}
	acc := &layerGPUAccelerator{
		mockAccelerator: mockAccelerator{name: "layer-test-gpu"},
	}
	provider := &layerGPUContextProvider{layerGPUAccelerator: acc, context: ctx}
	if err := RegisterAccelerator(provider); err != nil {
		t.Fatalf("RegisterAccelerator: %v", err)
	}
	t.Cleanup(func() {
		resetAccelerator()
	})
	_ = p
	return ctx
}

// TestLayerColorMatrix_GPUPathMatchesSoftware renders the same layer with the
// per-context GPU route and with pure software, and requires identical pixels.
func TestLayerColorMatrix_GPUPathMatchesSoftware(t *testing.T) {
	// Software reference.
	software := NewContext(16, 16)
	defer func() { _ = software.Close() }()
	drawTintedLayer(software, 0.5)
	softwarePixel := premulPixel(software.pixmap, 8, 8)

	// GPU route.
	new(layerGPUContextProvider).register(t)
	gpu := NewContext(16, 16)
	defer func() { _ = gpu.Close() }()
	drawTintedLayer(gpu, 0.5)
	gpuPixel := premulPixel(gpu.pixmap, 8, 8)

	assertPremul(t, "GPU vs software", gpuPixel, softwarePixel, 1)
}

// TestLayerColorMatrix_GPUDrawsFlushedBeforeCompositing checks that queued GPU
// work is flushed while the layer is still the active target.
func TestLayerColorMatrix_GPUDrawsFlushedBeforeCompositing(t *testing.T) {
	ctx := new(layerGPUContextProvider).register(t)

	dc := NewContext(8, 8)
	defer func() { _ = dc.Close() }()

	dc.PushLayerWithColorMatrix(BlendNormal, 1.0, GrayscaleColorMatrix())
	dc.SetRGBA(1, 0, 0, 1)
	dc.DrawRectangle(0, 0, 8, 8)
	if err := dc.Fill(); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if ctx.pending == 0 {
		t.Fatal("expected the GPU context to have queued work")
	}
	dc.PopLayer()

	if ctx.flushCalls == 0 {
		t.Fatal("PopLayer did not flush pending GPU work")
	}
	if ctx.pending != 0 {
		t.Errorf("pending GPU work after PopLayer = %d, want 0", ctx.pending)
	}
	// Grayscale red = 0.2126 -> 54.
	assertPremul(t, "GPU + grayscale", premulPixel(dc.pixmap, 4, 4), [4]uint8{54, 54, 54, 255}, 2)
}

// drawTintedLayer draws a semi-transparent red/green pair inside a 50% opacity
// layer carrying a saturation matrix, so the test exercises alpha, opacity and
// the matrix together.
func drawTintedLayer(dc *Context, opacity float64) {
	dc.PushLayerWithColorMatrix(BlendNormal, opacity, SaturationColorMatrix(0.5))
	dc.SetRGBA(1, 0, 0, 0.6)
	dc.DrawRectangle(0, 0, 16, 16)
	_ = dc.Fill()
	dc.PopLayer()
}

// TestLayerColorMatrix_FlushTargetsMatchLayerStack verifies that each push and
// pop flushes GPU work against the pixmap that was actually the active target,
// so work queued for an ancestor can never be resolved into a child layer (or
// left stranded when the target switches).
func TestLayerColorMatrix_FlushTargetsMatchLayerStack(t *testing.T) {
	ctx := new(layerGPUContextProvider).register(t)

	dc := NewContext(8, 8)
	defer func() { _ = dc.Close() }()

	base := dc.pixmap
	dc.PushLayerWithColorMatrix(BlendNormal, 1.0, GrayscaleColorMatrix())
	outer := dc.pixmap
	dc.SetRGBA(0, 0, 1, 1)
	dc.DrawRectangle(0, 0, 8, 8)
	_ = dc.Fill() // queued for outer

	dc.PushLayer(BlendNormal, 1.0) // must flush outer before switching
	inner := dc.pixmap
	if ctx.pending != 0 {
		t.Fatalf("pending GPU work = %d after push, want 0 (outer should be flushed)", ctx.pending)
	}

	dc.SetRGBA(0, 1, 0, 1)
	dc.DrawRectangle(0, 0, 8, 8)
	_ = dc.Fill() // queued for inner
	dc.PopLayer() // must flush inner

	dc.PopLayer() // must not find stale work; composites outer

	if len(dc.layerStack.layers) != 0 {
		t.Fatalf("layer depth = %d, want 0", len(dc.layerStack.layers))
	}
	if dc.pixmap != base {
		t.Error("PopLayer did not restore the base pixmap")
	}
	if outer == inner || inner == base || outer == base {
		t.Fatal("test setup broken: layer pixmaps must be distinct")
	}
	// Blue outer layer drawn first, green inner on top, then grayscale over
	// the composite: both blue and green become luminance ~0.0722/0.7152, so
	// the result is a gray whose value reflects the green (topmost) content.
	got := premulPixel(dc.pixmap, 4, 4)
	if got[0] != got[1] || got[1] != got[2] {
		t.Errorf("nested grayscale result should be gray, got %v", got)
	}
}
