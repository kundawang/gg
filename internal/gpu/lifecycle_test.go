//go:build !nogpu

package gpu

import (
	"errors"
	"sync"
	"testing"

	"github.com/gogpu/gg"
)

// TestGPURenderContext_RejectsCommandsAfterClose verifies that every command
// entry point fails with gg.ErrAcceleratorClosed once Close has run, and never
// queues into a context whose resources were released.
func TestGPURenderContext_RejectsCommandsAfterClose(t *testing.T) {
	s := NewGPUShared()
	rc := s.NewRenderContext()

	target := makeTestTarget(64, 64)
	path := gg.NewPath()
	path.Rectangle(8, 8, 48, 48)
	paint := gg.NewPaint()
	paint.SetBrush(gg.Solid(gg.Red))
	shape := gg.DetectedShape{Kind: gg.ShapeCircle, CenterX: 32, CenterY: 32, RadiusX: 20, RadiusY: 20}

	rc.Close()

	if err := rc.FillPath(target, path, paint); !errors.Is(err, gg.ErrAcceleratorClosed) {
		t.Errorf("FillPath after Close = %v, want ErrAcceleratorClosed", err)
	}
	if err := rc.StrokePath(target, path, paint); !errors.Is(err, gg.ErrAcceleratorClosed) {
		t.Errorf("StrokePath after Close = %v, want ErrAcceleratorClosed", err)
	}
	if err := rc.FillShape(target, shape, paint); !errors.Is(err, gg.ErrAcceleratorClosed) {
		t.Errorf("FillShape after Close = %v, want ErrAcceleratorClosed", err)
	}
	if err := rc.StrokeShape(target, shape, paint); !errors.Is(err, gg.ErrAcceleratorClosed) {
		t.Errorf("StrokeShape after Close = %v, want ErrAcceleratorClosed", err)
	}
	if err := rc.Flush(target); !errors.Is(err, gg.ErrAcceleratorClosed) {
		t.Errorf("Flush after Close = %v, want ErrAcceleratorClosed", err)
	}

	if got := rc.PendingCount(); got != 0 {
		t.Errorf("PendingCount = %d after Close, want 0", got)
	}
}

// TestGPURenderContext_CloseIdempotent verifies Close can be called repeatedly
// and does not release resources twice.
func TestGPURenderContext_CloseIdempotent(t *testing.T) {
	s := NewGPUShared()
	rc := s.NewRenderContext()
	rc.Close()
	rc.Close()
	rc.Close()
	if !rc.closed.Load() {
		t.Error("context should be marked closed")
	}
}

// TestGPUShared_EnsureGPUAfterCloseFails verifies lazy GPU init cannot happen
// on a closed shared holder; it must yield ErrAcceleratorClosed instead of
// resurrecting a device.
func TestGPUShared_EnsureGPUAfterCloseFails(t *testing.T) {
	s := NewGPUShared()
	s.Close()

	s.mu.Lock()
	err := s.ensureGPU()
	s.mu.Unlock()
	if !errors.Is(err, gg.ErrAcceleratorClosed) {
		t.Fatalf("ensureGPU after Close = %v, want ErrAcceleratorClosed", err)
	}
}

// TestGPUShared_CloseIdempotent verifies repeated Close calls do not touch
// already-released state and keep the holder closed.
func TestGPUShared_CloseIdempotent(t *testing.T) {
	s := NewGPUShared()
	s.Close()
	s.Close()
	if !s.IsClosed() {
		t.Error("shared holder should stay closed")
	}
}

// TestSDFAccelerator_CloseAndReinit verifies the SDF accelerator can be closed
// and re-initialized (the recovery path used by ReinitAccelerator), and that a
// second Close is a no-op.
func TestSDFAccelerator_CloseAndReinit(t *testing.T) {
	a := &SDFAccelerator{}
	if err := a.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	a.Close()
	a.Close() // idempotent

	sharedAfterClose := a.Shared()
	if sharedAfterClose != nil && !sharedAfterClose.IsClosed() {
		t.Error("shared holder should be closed after accelerator Close")
	}

	if err := a.Init(); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	if a.closed {
		t.Error("accelerator should not be marked closed after re-Init")
	}
	if shared := a.Shared(); shared == nil || shared.IsClosed() {
		t.Error("re-Init should leave a usable shared holder")
	}
}

// TestSDFAccelerator_DefaultCtxRecreatedAfterReinit verifies the default
// render context is usable again after a close/re-init cycle.
func TestSDFAccelerator_DefaultCtxRecreatedAfterReinit(t *testing.T) {
	a := &SDFAccelerator{}
	if err := a.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	target := makeTestTarget(64, 64)
	path := gg.NewPath()
	path.Rectangle(8, 8, 48, 48)
	paint := gg.NewPaint()
	paint.SetBrush(gg.Solid(gg.Red))
	if err := a.FillPath(target, path, paint); err != nil {
		t.Fatalf("FillPath: %v", err)
	}
	a.Close()

	if err := a.Init(); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	if err := a.FillPath(target, path, paint); err != nil {
		t.Fatalf("FillPath after re-Init: %v", err)
	}
	a.Close()
}

// TestClassifyGPUError verifies backend lifecycle errors are mapped onto gg's
// sentinels so callers can detect device loss / closed accelerator with
// errors.Is, including through wrapping.
func TestClassifyGPUError(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want error // sentinel expected via errors.Is; nil means unchanged
	}{
		{"nil stays nil", nil, nil},
		{"not initialized is closed", ErrNotInitialized, gg.ErrAcceleratorClosed},
		{"plain error unchanged", errors.New("plain"), nil},
		{"already classified lost", gg.ErrDeviceLost, gg.ErrDeviceLost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyGPUError(tt.in)
			if tt.in == nil {
				if got != nil {
					t.Fatalf("ClassifyGPUError(nil) = %v, want nil", got)
				}
				return
			}
			if tt.want != nil && !errors.Is(got, tt.want) {
				t.Errorf("ClassifyGPUError(%v) = %v, want errors.Is(_, %v)", tt.in, got, tt.want)
			}
			// The original error must remain reachable.
			if !errors.Is(got, tt.in) {
				t.Errorf("ClassifyGPUError(%v) dropped the original error", tt.in)
			}
		})
	}
}

// TestSDFAccelerator_SetDeviceProviderNil verifies a nil provider is rejected
// before dereferencing.
func TestSDFAccelerator_SetDeviceProviderNil(t *testing.T) {
	a := &SDFAccelerator{}
	if err := a.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer a.Close()
	if err := a.SetDeviceProvider(nil); !errors.Is(err, gg.ErrNilDeviceProvider) {
		t.Fatalf("SetDeviceProvider(nil) = %v, want ErrNilDeviceProvider", err)
	}
}

// TestGPURenderContext_CloseConcurrent verifies Close is safe under concurrent
// calls (no double release of cached resources).
func TestGPURenderContext_CloseConcurrent(t *testing.T) {
	s := NewGPUShared()
	rc := s.NewRenderContext()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc.Close()
		}()
	}
	wg.Wait()
	if !rc.closed.Load() {
		t.Error("context should be closed")
	}
}
