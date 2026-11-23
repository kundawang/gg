package gg

import (
	"errors"
	"sync"
	"testing"

	"github.com/gogpu/gpucontext"
	"github.com/gogpu/gputypes"
)

// lifecycleRecorder is an injectable accelerator double. It records every
// method invocation and lifecycle transition so tests can assert on the exact
// sequence of state changes and resource releases.
//
// It never renders anything: Fill/Stroke return ErrFallbackToCPU so the CPU
// path produces verifiable output unless a test explicitly changes the script.
type lifecycleRecorder struct {
	name string

	initErr  error
	flushErr error
	shapeErr error
	pathErr  error
	canAccel AcceleratedOp

	mu        sync.Mutex
	events    []string // ordered record of method calls
	initCalls int
	closes    int
	starts    int

	// provider wiring
	provider        gpucontext.DeviceProvider
	providerErr     error
	providerCalls   int
	providerApplied int
}

func newLifecycleRecorder(name string, ops AcceleratedOp) *lifecycleRecorder {
	return &lifecycleRecorder{name: name, canAccel: ops}
}

func (r *lifecycleRecorder) record(ev string) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *lifecycleRecorder) eventCount(ev string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e == ev {
			n++
		}
	}
	return n
}

func (r *lifecycleRecorder) Name() string {
	r.record("Name")
	return r.name
}

func (r *lifecycleRecorder) Init() error {
	r.mu.Lock()
	r.initCalls++
	r.mu.Unlock()
	r.record("Init")
	return r.initErr
}

func (r *lifecycleRecorder) Close() {
	r.mu.Lock()
	r.closes++
	r.mu.Unlock()
	r.record("Close")
}

func (r *lifecycleRecorder) CanAccelerate(op AcceleratedOp) bool {
	r.record("CanAccelerate")
	return op&r.canAccel != 0
}

func (r *lifecycleRecorder) FillPath(_ GPURenderTarget, _ *Path, _ *Paint) error {
	r.record("FillPath")
	return r.pathErrOrFallback()
}

func (r *lifecycleRecorder) StrokePath(_ GPURenderTarget, _ *Path, _ *Paint) error {
	r.record("StrokePath")
	return r.pathErrOrFallback()
}

func (r *lifecycleRecorder) FillShape(_ GPURenderTarget, _ DetectedShape, _ *Paint) error {
	r.record("FillShape")
	if r.shapeErr != nil {
		return r.shapeErr
	}
	return ErrFallbackToCPU
}

func (r *lifecycleRecorder) StrokeShape(_ GPURenderTarget, _ DetectedShape, _ *Paint) error {
	r.record("StrokeShape")
	if r.shapeErr != nil {
		return r.shapeErr
	}
	return ErrFallbackToCPU
}

func (r *lifecycleRecorder) Flush(_ GPURenderTarget) error {
	r.record("Flush")
	return r.flushErr
}

func (r *lifecycleRecorder) pathErrOrFallback() error {
	if r.pathErr != nil {
		return r.pathErr
	}
	return ErrFallbackToCPU
}

func (r *lifecycleRecorder) BeginFrame() {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	r.record("BeginFrame")
}

func (r *lifecycleRecorder) SetDeviceProvider(p gpucontext.DeviceProvider) error {
	r.mu.Lock()
	r.providerCalls++
	if r.providerErr == nil {
		r.provider = p
		r.providerApplied++
	}
	r.mu.Unlock()
	r.record("SetDeviceProvider")
	return r.providerErr
}

func (r *lifecycleRecorder) getProvider() gpucontext.DeviceProvider {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.provider
}

// nilDeviceProvider is a typed nil pointer stored in an interface. Its methods
// would dereference nil, so gg must reject it before calling into it.
type nilDeviceProvider struct{ mockProviderBase }

// mockProviderBase fills the remaining DeviceProvider methods with zero values.
type mockProviderBase struct{}

func (mockProviderBase) Device() gpucontext.Device { return gpucontext.Device{} }
func (mockProviderBase) Queue() gpucontext.Queue   { return gpucontext.Queue{} }
func (mockProviderBase) SurfaceFormat() gputypes.TextureFormat {
	return gputypes.TextureFormatUndefined
}
func (mockProviderBase) Adapter() gpucontext.Adapter         { return gpucontext.Adapter{} }
func (mockProviderBase) AdapterInfo() gpucontext.AdapterInfo { return gpucontext.AdapterInfo{} }
func (mockProviderBase) Features() gputypes.Features         { return 0 }
func (mockProviderBase) DownlevelCapabilities() gputypes.DownlevelCapabilities {
	return gputypes.DownlevelCapabilities{}
}

func TestAcceleratorRegisterRecordsLifecycle(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	rec := newLifecycleRecorder("rec", AccelFill)
	if err := RegisterAccelerator(rec); err != nil {
		t.Fatalf("RegisterAccelerator: %v", err)
	}
	if got := Accelerator(); got != rec {
		t.Fatalf("Accelerator() = %v, want recorder", got)
	}

	st := AcceleratorSnapshot()
	if st.State != AcceleratorReady {
		t.Errorf("state = %v, want ready", st.State)
	}
	if st.Name != "rec" {
		t.Errorf("name = %q, want rec", st.Name)
	}
	if st.Registrations != 1 {
		t.Errorf("registrations = %d, want 1", st.Registrations)
	}
	if st.Transitions == 0 {
		t.Error("expected at least one transition")
	}
	if rec.eventCount("Init") != 1 {
		t.Errorf("Init calls = %d, want 1", rec.eventCount("Init"))
	}
}

func TestAcceleratorInitFailureKeepsPrevious(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	good := newLifecycleRecorder("good", AccelFill)
	if err := RegisterAccelerator(good); err != nil {
		t.Fatalf("register good: %v", err)
	}
	bad := newLifecycleRecorder("bad", AccelFill)
	bad.initErr = errors.New("boom")

	if err := RegisterAccelerator(bad); !errors.Is(err, bad.initErr) {
		t.Fatalf("RegisterAccelerator(bad) = %v, want init error", err)
	}
	if got := Accelerator(); got != good {
		t.Fatal("failed registration must keep the previous accelerator usable")
	}
	if good.eventCount("Close") != 0 {
		t.Error("previous accelerator must not be closed when the replacement fails")
	}
	st := AcceleratorSnapshot()
	if st.InitFailures != 1 {
		t.Errorf("InitFailures = %d, want 1", st.InitFailures)
	}
	if !errors.Is(st.LastError, bad.initErr) {
		t.Errorf("LastError = %v, want init error", st.LastError)
	}
}

func TestAcceleratorReplaceClosesPreviousExactlyOnce(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	first := newLifecycleRecorder("first", AccelFill)
	second := newLifecycleRecorder("second", AccelFill)
	third := newLifecycleRecorder("third", AccelFill)

	for _, a := range []*lifecycleRecorder{first, second} {
		if err := RegisterAccelerator(a); err != nil {
			t.Fatalf("register %s: %v", a.name, err)
		}
	}
	if first.eventCount("Close") != 1 {
		t.Errorf("first Close calls = %d, want 1", first.eventCount("Close"))
	}
	if err := RegisterAccelerator(third); err != nil {
		t.Fatalf("register third: %v", err)
	}
	if second.eventCount("Close") != 1 {
		t.Errorf("second Close calls = %d, want 1", second.eventCount("Close"))
	}
	if third.eventCount("Close") != 0 {
		t.Error("current accelerator must stay open")
	}
}

func TestAcceleratorCloseIsIdempotentAndReleasesOnce(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	rec := newLifecycleRecorder("once", AccelFill)
	if err := RegisterAccelerator(rec); err != nil {
		t.Fatalf("register: %v", err)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			CloseAccelerator()
		}()
	}
	wg.Wait()

	if rec.eventCount("Close") != 1 {
		t.Fatalf("concurrent CloseAccelerator invoked Close %d times, want 1", rec.eventCount("Close"))
	}
	if Accelerator() != nil {
		t.Error("Accelerator() must be nil after close")
	}
	if st := AcceleratorSnapshot(); st.State != AcceleratorClosed {
		t.Errorf("state = %v, want closed", st.State)
	}
	// A second, sequential call is also a no-op.
	CloseAccelerator()
	if rec.eventCount("Close") != 1 {
		t.Errorf("Close called again after close; count = %d", rec.eventCount("Close"))
	}
}

func TestAcceleratorDeviceLossExcludesDispatch(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	rec := newLifecycleRecorder("lossy", AccelFill|AccelCircleSDF)
	if err := RegisterAccelerator(rec); err != nil {
		t.Fatalf("register: %v", err)
	}

	ReportAcceleratorDeviceLost(nil)
	if Accelerator() != nil {
		t.Fatal("Accelerator() must be nil after device loss")
	}
	st := AcceleratorSnapshot()
	if st.State != AcceleratorDeviceLost {
		t.Errorf("state = %v, want device-lost", st.State)
	}
	if st.DeviceLosses != 1 {
		t.Errorf("DeviceLosses = %d, want 1", st.DeviceLosses)
	}
	if !errors.Is(st.FirstError, ErrDeviceLost) {
		t.Errorf("FirstError = %v, want ErrDeviceLost", st.FirstError)
	}

	// Repeated reports are counted but do not reopen or double-transition state.
	before := AcceleratorSnapshot()
	ReportAcceleratorDeviceLost(errors.New("again"))
	after := AcceleratorSnapshot()
	if after.DeviceLosses != before.DeviceLosses+1 {
		t.Errorf("DeviceLosses = %d, want %d", after.DeviceLosses, before.DeviceLosses+1)
	}
	if after.Transitions != before.Transitions {
		t.Error("already-lost accelerator must not transition again")
	}
	if Accelerator() != nil {
		t.Error("accelerator must stay excluded after repeated reports")
	}

	// Rendering with no usable accelerator produces verifiable CPU output.
	dc := NewContext(64, 64)
	dc.SetRGB(0, 1, 0)
	dc.DrawRectangle(8, 8, 48, 48)
	if err := dc.Fill(); err != nil {
		t.Fatalf("Fill() after loss = %v, want nil (CPU fallback)", err)
	}
	if px := dc.pixmap.GetPixel(32, 32); px.G < 0.5 {
		t.Errorf("pixel G = %f, want >= 0.5 (software output)", px.G)
	}
	_ = dc.Close()
}

func TestReinitAcceleratorRecoversAfterLoss(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	rec := newLifecycleRecorder("phoenix", AccelFill)
	if err := RegisterAccelerator(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	ReportAcceleratorDeviceLost(nil)

	if err := ReinitAccelerator(); err != nil {
		t.Fatalf("ReinitAccelerator: %v", err)
	}
	if got := Accelerator(); got != rec {
		t.Fatal("accelerator must be usable again after reinit")
	}
	if rec.eventCount("Init") != 2 {
		t.Errorf("Init calls = %d, want 2 (register + reinit)", rec.eventCount("Init"))
	}
	if st := AcceleratorSnapshot(); st.State != AcceleratorReady {
		t.Errorf("state = %v, want ready", st.State)
	}
}

func TestReinitAcceleratorFailureKeepsLost(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	rec := newLifecycleRecorder("flaky", AccelFill)
	if err := RegisterAccelerator(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	ReportAcceleratorDeviceLost(nil)

	rec.initErr = errors.New("still gone")
	if err := ReinitAccelerator(); err == nil {
		t.Fatal("ReinitAccelerator must return the init failure")
	}
	if Accelerator() != nil {
		t.Fatal("failed reinit must leave the accelerator excluded")
	}
	if st := AcceleratorSnapshot(); !errors.Is(st.LastError, rec.initErr) {
		t.Errorf("LastError = %v, want init failure", st.LastError)
	}
}

func TestReinitAcceleratorNoRegistration(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	if err := ReinitAccelerator(); !errors.Is(err, ErrNoAccelerator) {
		t.Fatalf("ReinitAccelerator() = %v, want ErrNoAccelerator", err)
	}
}

func TestAcceleratorProviderNilAndReapply(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	rec := newLifecycleRecorder("shared", AccelFill)
	if err := RegisterAccelerator(rec); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := SetAcceleratorDeviceProvider(nil); !errors.Is(err, ErrNilDeviceProvider) {
		t.Errorf("nil provider = %v, want ErrNilDeviceProvider", err)
	}
	var typedNil *nilDeviceProvider
	if err := SetAcceleratorDeviceProvider(typedNil); !errors.Is(err, ErrNilDeviceProvider) {
		t.Errorf("typed-nil provider = %v, want ErrNilDeviceProvider", err)
	}
	if rec.eventCount("SetDeviceProvider") != 0 {
		t.Error("nil provider must not reach the accelerator")
	}

	good := &nilDeviceProviderConcrete{}
	if err := SetAcceleratorDeviceProvider(good); err != nil {
		t.Fatalf("valid provider: %v", err)
	}
	if rec.getProvider() != good {
		t.Error("provider not recorded by accelerator")
	}
	if st := AcceleratorSnapshot(); st.ProviderSets != 1 {
		t.Errorf("ProviderSets = %d, want 1", st.ProviderSets)
	}

	// A provider failure is recorded and returned, and is not marked as set.
	rec.providerErr = errors.New("bad device")
	if err := SetAcceleratorDeviceProvider(&nilDeviceProviderConcrete{}); err == nil {
		t.Fatal("provider failure must be returned")
	}
	if st := AcceleratorSnapshot(); st.ProviderSets != 1 {
		t.Errorf("failed provider set must not increment ProviderSets, got %d", st.ProviderSets)
	}
}

// nilDeviceProviderConcrete is a concrete (non-nil) provider for tests.
type nilDeviceProviderConcrete struct{ mockProviderBase }

func TestAcceleratorProviderReappliedOnReinit(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	rec := newLifecycleRecorder("shared", AccelFill)
	if err := RegisterAccelerator(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	provider := &nilDeviceProviderConcrete{}
	if err := SetAcceleratorDeviceProvider(provider); err != nil {
		t.Fatalf("set provider: %v", err)
	}

	ReportAcceleratorDeviceLost(nil)
	if err := ReinitAccelerator(); err != nil {
		t.Fatalf("reinit: %v", err)
	}
	if rec.providerApplied != 2 {
		t.Errorf("provider applied %d times, want 2 (initial + reinit)", rec.providerApplied)
	}
	if rec.getProvider() != provider {
		t.Error("reinit must re-apply the remembered provider")
	}
}

func TestAcceleratorBeginFrameCountsOnlyWhenUsable(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	rec := newLifecycleRecorder("frames", AccelFill)
	if err := RegisterAccelerator(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	BeginAcceleratorFrame()
	if rec.starts != 1 {
		t.Fatalf("BeginFrame calls = %d, want 1", rec.starts)
	}
	before := AcceleratorSnapshot().Frames

	ReportAcceleratorDeviceLost(nil)
	BeginAcceleratorFrame()
	if rec.starts != 1 {
		t.Error("BeginAcceleratorFrame must not touch a lost accelerator")
	}
	if got := AcceleratorSnapshot().Frames; got != before {
		t.Errorf("Frames incremented for lost accelerator: %d -> %d", before, got)
	}
}

func TestFallbackPolicy(t *testing.T) {
	old := GPUFallbackPolicy()
	t.Cleanup(func() { SetGPUFallbackPolicy(old) })

	hard := errors.New("gpu exploded")
	unsupported := ErrFallbackToCPU

	SetGPUFallbackPolicy(FallbackOnUnsupported)
	if AllowCPUFallback(nil) {
		t.Error("nil error must never permit a fallback")
	}
	if !AllowCPUFallback(unsupported) {
		t.Error("strict policy must still permit fallback for ErrFallbackToCPU")
	}
	if AllowCPUFallback(hard) {
		t.Error("strict policy must not permit fallback for a hard GPU error")
	}

	SetGPUFallbackPolicy(FallbackOnError)
	if !AllowCPUFallback(hard) {
		t.Error("permissive policy must permit fallback for any GPU error")
	}
	if !AllowCPUFallback(unsupported) {
		t.Error("permissive policy must permit fallback for ErrFallbackToCPU")
	}
	if AllowCPUFallback(nil) {
		t.Error("nil error must never permit a fallback")
	}
}

func TestClassifyAcceleratorError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want AcceleratorState
	}{
		{"nil", nil, AcceleratorReady},
		{"closed", ErrAcceleratorClosed, AcceleratorClosed},
		{"wrapped closed", wrap(ErrAcceleratorClosed), AcceleratorClosed},
		{"lost", ErrDeviceLost, AcceleratorDeviceLost},
		{"wrapped lost", wrap(ErrDeviceLost), AcceleratorDeviceLost},
		{"generic", errors.New("other"), AcceleratorUnregistered},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyAcceleratorError(tt.err); got != tt.want {
				t.Errorf("ClassifyAcceleratorError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func wrap(err error) error { return errors.Join(errors.New("outer"), err) }

func TestAcceleratorConcurrentRegisterAndClose(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := newLifecycleRecorder("racer", AccelFill)
			_ = RegisterAccelerator(rec)
			if i%4 == 0 {
				CloseAccelerator()
			}
			_ = Accelerator()
			_ = AcceleratorSnapshot()
		}(i)
	}
	wg.Wait()
}

func TestDeviceLossAfterCloseDoesNotRewriteState(t *testing.T) {
	resetAccelerator()
	t.Cleanup(resetAccelerator)

	rec := newLifecycleRecorder("closed-first", AccelFill)
	if err := RegisterAccelerator(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	CloseAccelerator()

	ReportAcceleratorDeviceLost(nil)
	if st := AcceleratorSnapshot(); st.State != AcceleratorClosed {
		t.Errorf("state = %v, want closed (loss after close must not rewrite)", st.State)
	}
	if Accelerator() != nil {
		t.Error("accelerator must remain unusable")
	}
}
