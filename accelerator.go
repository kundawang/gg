package gg

import (
	"errors"
	"image"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gogpu/gg/text"
	"github.com/gogpu/gpucontext"
)

// ErrFallbackToCPU indicates the GPU accelerator cannot handle this operation.
// The caller should transparently fall back to CPU rendering.
var ErrFallbackToCPU = errors.New("gg: falling back to CPU rendering")

// Sentinels describing accelerator lifecycle failures. All of them are
// comparable with errors.Is, including through wrapping done by backends.
var (
	// ErrAcceleratorClosed is returned when an operation is attempted on an
	// accelerator after Close. No GPU resource may be used after close.
	ErrAcceleratorClosed = errors.New("gg: accelerator is closed")

	// ErrDeviceLost is returned when the GPU device disappeared (driver reset,
	// external provider shutdown, surface teardown). The accelerator is put in
	// the device-lost state and is not used again until a successful
	// [ReinitAccelerator] or [RegisterAccelerator].
	ErrDeviceLost = errors.New("gg: GPU device lost")

	// ErrNilDeviceProvider is returned when a nil DeviceProvider is passed to
	// [SetAcceleratorDeviceProvider].
	ErrNilDeviceProvider = errors.New("gg: device provider must not be nil")

	// ErrNoAccelerator is returned by lifecycle operations that require a
	// registered accelerator.
	ErrNoAccelerator = errors.New("gg: no GPU accelerator registered")
)

// FallbackPolicy controls when a GPU error may be resolved by rendering the
// operation on the CPU instead. Falling back is always an explicit policy
// decision, never an implicit side effect.
type FallbackPolicy uint32

const (
	// FallbackOnUnsupported only falls back when the accelerator reports
	// [ErrFallbackToCPU] (the operation is not accelerated). Any other GPU
	// error is returned to the caller. This is the strict mode.
	FallbackOnUnsupported FallbackPolicy = iota

	// FallbackOnError falls back on every GPU error, including device loss and
	// backend failures. This is the default, and preserves the historical
	// behavior of gg: a broken GPU never prevents rendering. The first error is
	// still recorded in [AcceleratorStatus] and [GPUContextStatus]; it is not
	// swallowed.
	FallbackOnError
)

func (p FallbackPolicy) String() string {
	switch p {
	case FallbackOnUnsupported:
		return "on-unsupported"
	case FallbackOnError:
		return "on-error"
	default:
		return "unknown"
	}
}

// fallbackPolicyPtr holds the process-wide FallbackPolicy as an atomic uint32.
// Default is FallbackOnError (zero value is FallbackOnUnsupported, so it is
// initialized explicitly).
var fallbackPolicyPtr atomic.Uint32

func init() {
	fallbackPolicyPtr.Store(uint32(FallbackOnError))
}

// SetGPUFallbackPolicy sets the process-wide policy used to decide whether a
// failed GPU operation may be rendered on the CPU. It is safe for concurrent
// use.
func SetGPUFallbackPolicy(p FallbackPolicy) {
	fallbackPolicyPtr.Store(uint32(p))
}

// GPUFallbackPolicy returns the current process-wide fallback policy.
func GPUFallbackPolicy() FallbackPolicy {
	return FallbackPolicy(fallbackPolicyPtr.Load())
}

// AllowCPUFallback reports whether err permits falling back to CPU under the
// current [GPUFallbackPolicy]. A nil error never permits a fallback (the GPU
// operation succeeded).
func AllowCPUFallback(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrFallbackToCPU) {
		// Unsupported operations are the one case the strict policy allows.
		return true
	}
	return GPUFallbackPolicy() == FallbackOnError
}

// AcceleratorState describes where the registered accelerator is in its
// lifecycle. It is informational: [Accelerator] returns nil for both the
// device-lost and closed states.
type AcceleratorState uint32

const (
	// AcceleratorUnregistered means no accelerator has been registered yet.
	AcceleratorUnregistered AcceleratorState = iota

	// AcceleratorReady means the accelerator is initialized and usable.
	AcceleratorReady

	// AcceleratorDeviceLost means the GPU device was lost. The accelerator is
	// excluded from dispatch until a successful ReinitAccelerator or
	// RegisterAccelerator call.
	AcceleratorDeviceLost

	// AcceleratorClosed means CloseAccelerator released the accelerator.
	AcceleratorClosed
)

func (s AcceleratorState) String() string {
	switch s {
	case AcceleratorUnregistered:
		return "unregistered"
	case AcceleratorReady:
		return "ready"
	case AcceleratorDeviceLost:
		return "device-lost"
	case AcceleratorClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// AcceleratorStatus is a snapshot of the accelerator lifecycle. Errors are
// preserved so callers can detect a fallback without parsing log output.
type AcceleratorStatus struct {
	State         AcceleratorState
	Name          string
	Transitions   uint64 // lifecycle state changes observed
	Registrations uint64 // successful RegisterAccelerator calls
	InitFailures  uint64 // RegisterAccelerator calls whose Init failed
	Closes        uint64 // accelerator Close invocations (replace or explicit)
	DeviceLosses  uint64 // device-lost reports
	ProviderSets  uint64 // successful SetAcceleratorDeviceProvider calls
	Frames        uint64 // BeginAcceleratorFrame calls dispatched to an accelerator
	Fallbacks     uint64 // GPU operations resolved on the CPU
	FirstError    error  // first failure since the last successful registration
	LastError     error  // most recent failure
}

// AcceleratedOp describes operation types for GPU capability checking.
type AcceleratedOp uint32

const (
	// AccelFill represents path fill operations.
	AccelFill AcceleratedOp = 1 << iota

	// AccelStroke represents path stroke operations.
	AccelStroke

	// AccelScene represents full scene rendering.
	AccelScene

	// AccelText represents text rendering.
	AccelText

	// AccelImage represents image compositing.
	AccelImage

	// AccelGradient represents gradient rendering.
	AccelGradient

	// AccelCircleSDF represents SDF-based circle rendering.
	AccelCircleSDF

	// AccelRRectSDF represents SDF-based rounded rectangle rendering.
	AccelRRectSDF
)

// GPURenderTarget provides pixel buffer access for GPU output.
//
// Two mutually exclusive modes:
//
// GPU-direct mode: when View is non-nil, the render pass resolves directly
// to this texture view. Data/Stride are ignored and no CPU readback occurs.
// ViewWidth and ViewHeight specify the view dimensions in device pixels.
//
// CPU readback mode: when View is nil, rendering goes to an internal resolve
// texture and pixels are copied back into Data. Width, Height, and Stride
// describe the destination pixel buffer layout.
//
// The Data slice (when used) must be in premultiplied RGBA format, 4 bytes
// per pixel, laid out row by row with the given Stride.
type GPURenderTarget struct {
	// GPU-direct path: render to this view without CPU readback.
	// When non-nil, Data/Stride are ignored -- no CPU readback.
	// Type-assert to *wgpu.TextureView in internal/gpu consumers.
	View       gpucontext.TextureView
	ViewWidth  uint32
	ViewHeight uint32

	// PreserveContent signals that the surface view already has content from an
	// external renderer (e.g., g3d). When true, that content becomes the
	// compositor base and a queued DrawGPUTextureBase layer is ignored. A
	// single-sample pass loads the surface directly; an MSAA pass renders a
	// transparent overlay and alpha-composites its resolve onto the surface.
	// Set by gogpu after MarkExternalContent(). ADR-059.
	PreserveContent bool

	// Compositor handles surface-level decisions (LoadOp, damage scissoring,
	// MSAA overlay compositing). Set per-frame by the host framework (gogpu)
	// through ggcanvas. When nil, gg uses legacy internal paths. ADR-067.
	Compositor gpucontext.SurfaceCompositor

	// Damage-aware compositing (ADR-026/028): when non-empty, compositor
	// uses LoadOpLoad (preserve previous frame) and per-rect scissor.
	// Single rect: one scissor for entire pass.
	// Multiple rects: per-draw dynamic scissor (ADR-028).
	DamageRects []image.Rectangle

	// CPU readback path.
	Data          []uint8
	Width, Height int
	Stride        int // bytes per row
}

// GPUAccelerator is an optional GPU acceleration provider.
//
// When registered via RegisterAccelerator, the Context tries GPU acceleration
// first for supported operations. If the accelerator returns ErrFallbackToCPU
// or any error, rendering transparently falls back to CPU.
//
// Implementations should be provided by GPU backend packages (e.g., gg/gpu/).
// Users opt in to GPU acceleration via blank import:
//
//	import _ "github.com/gogpu/gg/gpu" // enables GPU acceleration
type GPUAccelerator interface {
	// Name returns the accelerator name (e.g., "wgpu", "vulkan").
	Name() string

	// Init initializes GPU resources. Called once during registration.
	Init() error

	// Close releases GPU resources.
	Close()

	// CanAccelerate reports whether the accelerator supports the given operation.
	// This is a fast check used to skip GPU entirely for unsupported operations.
	CanAccelerate(op AcceleratedOp) bool

	// FillPath renders a filled path to the target.
	// Returns ErrFallbackToCPU if the path cannot be GPU-accelerated.
	FillPath(target GPURenderTarget, path *Path, paint *Paint) error

	// StrokePath renders a stroked path to the target.
	// Returns ErrFallbackToCPU if the path cannot be GPU-accelerated.
	StrokePath(target GPURenderTarget, path *Path, paint *Paint) error

	// FillShape renders a detected shape using SDF.
	// This is the fast path for circles and rounded rectangles.
	// Returns ErrFallbackToCPU if the shape is not supported.
	FillShape(target GPURenderTarget, shape DetectedShape, paint *Paint) error

	// StrokeShape renders a detected shape outline using SDF.
	// Returns ErrFallbackToCPU if the shape is not supported.
	StrokeShape(target GPURenderTarget, shape DetectedShape, paint *Paint) error

	// Flush dispatches any pending GPU operations to the target pixel buffer.
	// Batch-capable accelerators accumulate shapes during FillShape/StrokeShape
	// and dispatch them all in a single GPU pass on Flush.
	// Immediate-mode accelerators (e.g., CPU SDF) return nil.
	Flush(target GPURenderTarget) error
}

// DeviceProviderAware is an optional interface for accelerators that can share
// GPU resources with an external provider (e.g., gogpu window).
// When SetDeviceProvider is called, the accelerator reuses the provided GPU
// device instead of creating its own.
//
// The provider's Device() returns gpucontext.Device (opaque handle).
// Use wgpu.DeviceFromHandle() to extract the concrete *wgpu.Device:
//
//	wgpuDev := wgpu.DeviceFromHandle(provider.Device())
type DeviceProviderAware interface {
	SetDeviceProvider(provider gpucontext.DeviceProvider) error
}

// GPURenderContextProvider is an optional interface for accelerators that
// support per-context GPU rendering. When the accelerator implements this
// interface, each gg.Context lazily creates its own GPURenderContext for
// isolated pending command queues, clip state, and frame tracking.
//
// This follows the Skia GrContext pattern: shared device/pipelines/atlas,
// per-context pending ops. Without this, all contexts share one default
// render context (backward compatible, single-context behavior).
type GPURenderContextProvider interface {
	// NewGPURenderContext creates a new per-context GPU render context.
	// The returned value should be stored on the gg.Context and closed
	// when the Context is closed.
	NewGPURenderContext() any
}

// FrameAware is an optional interface for accelerators that need per-frame
// lifecycle management. BeginFrame resets per-frame state so that the first
// render pass of each frame replaces the surface, while subsequent mid-frame
// flushes preserve its content.
//
// Without calling BeginFrame, the surface is only cleared on the very first
// frame and all subsequent frames composite on top of previous content,
// causing progressive accumulation artifacts.
type FrameAware interface {
	BeginFrame()
}

// GPUTextAccelerator is an optional interface for accelerators that support
// GPU-accelerated text rendering via MSDF (Multi-channel Signed Distance
// Field). When the registered accelerator implements this interface,
// Context.DrawString will use the GPU pipeline instead of CPU freetype.
//
// Text is rendered as Tier 4 in the unified render pass, after SDF shapes,
// convex polygons, and stencil-then-cover paths. The MSDF approach provides
// resolution-independent, crisp text at any scale.
type GPUTextAccelerator interface {
	// DrawText renders text at position (x, y) in user space where y is the
	// baseline. The face provides font metrics and glyph iteration. Color is
	// the text color in RGBA. The matrix parameter is the context's current
	// transformation matrix (CTM), which the GPU pipeline composes into the
	// vertex shader uniform so that Scale, Rotate, and Skew transforms
	// affect text rendering, not just position.
	//
	// The deviceScale parameter is the ratio of physical pixels to logical
	// pixels (e.g., 2.0 on a Retina display). The MSDF pipeline uses this
	// to compute an effective font size (logical size * deviceScale), which
	// produces a higher screenPxRange and crisper text on HiDPI displays.
	// Pass 1.0 for standard (non-HiDPI) rendering.
	//
	// Returns ErrFallbackToCPU if GPU text rendering is not available.
	DrawText(target GPURenderTarget, face any, text string, x, y float64, color RGBA, matrix Matrix, deviceScale float64) error
}

// GPUGlyphMaskAccelerator is an optional interface for accelerators that
// support Tier 6 glyph mask text rendering. When the registered accelerator
// implements this interface, Context.DrawString routes small horizontal text
// through the glyph mask pipeline instead of MSDF.
//
// Glyph mask rendering CPU-rasterizes glyphs at the exact device pixel size
// into an R8 alpha atlas, then draws them as textured quads on the GPU. This
// produces pixel-perfect hinted text for horizontal layouts at small sizes
// (typically <48px), matching the quality of native platform text renderers.
//
// The MSDF pipeline (GPUTextAccelerator) remains the preferred path for
// rotated, scaled, or large text where resolution-independence matters.
type GPUGlyphMaskAccelerator interface {
	// DrawGlyphMaskText renders text at position (x, y) in user space where
	// y is the baseline. The face provides font metrics and glyph iteration.
	//
	// The paint parameter provides the text color. The matrix parameter is
	// the context's current transformation matrix (CTM). The deviceScale
	// parameter is the ratio of physical to logical pixels.
	//
	// viewportW and viewportH are the viewport dimensions for building the
	// ortho projection in the vertex shader.
	//
	// Returns ErrFallbackToCPU if glyph mask rendering is not available.
	DrawGlyphMaskText(target GPURenderTarget, face any, s string, x, y float64, color RGBA, matrix Matrix, deviceScale float64) error
}

// GPUAliasedTextAccelerator is an optional interface for accelerators that
// support aliased (non-anti-aliased) text rendering through the glyph mask
// pipeline. When TextModeAliased is set, Context.DrawString routes text
// through this interface instead of GPUGlyphMaskAccelerator.
//
// The glyph masks are rasterized with binary coverage (0 or 255 only) using
// the NoAAFiller, matching Skia's SkFont::Edging::kAlias behavior.
type GPUAliasedTextAccelerator interface {
	DrawGlyphMaskTextAliased(target GPURenderTarget, face any, s string, x, y float64, color RGBA, matrix Matrix, deviceScale float64) error
}

// GPUShapedTextAccelerator extends GPUGlyphMaskAccelerator with support for
// pre-shaped glyph rendering. This eliminates re-shaping at render time —
// the scene's stored glyph IDs and positions are used directly.
//
// textMode selects regular or binary coverage without requiring a second
// public accelerator interface.
//
// Implements the ADR-022 "shape once, render anywhere" guarantee.
// Enterprise pattern: Skia drawTextBlob, Vello draw_glyphs, Flutter drawParagraph.
type GPUShapedTextAccelerator interface {
	DrawShapedGlyphMaskText(target GPURenderTarget, face any, glyphs []text.ShapedGlyph, x, y float64, color RGBA, matrix Matrix, deviceScale float64, textMode TextMode) error
}

var (
	accelMu sync.RWMutex

	// accel is the usable accelerator. It is the only handle used for
	// dispatch: nil means "GPU unavailable, render on CPU". A lost or closed
	// accelerator is retained in retainedAccel so status and re-initialization
	// remain possible without exposing dead resources to dispatch.
	accel GPUAccelerator

	// retainedAccel is the most recently registered accelerator, usable or not.
	retainedAccel GPUAccelerator

	// retainedClosed is true once Close has been invoked on retainedAccel.
	retainedClosed bool

	// accelStatus is the lifecycle counters snapshot. Guarded by accelMu.
	accelStatus = AcceleratorStatus{State: AcceleratorUnregistered}
)

// RegisterAccelerator registers a GPU accelerator for optional GPU rendering.
//
// Only one accelerator can be registered. Subsequent calls replace the previous one.
// The accelerator's Init() method is called during registration.
// If Init() fails, the accelerator is not registered and the error is returned;
// the previous accelerator (if any) stays registered and usable.
//
// A successful registration clears the device-lost state recorded by
// [ReportAcceleratorDeviceLost], so recovery is: register the same or a new
// accelerator.
//
// Typical usage via blank import in GPU backend packages:
//
//	func init() {
//	    gg.RegisterAccelerator(NewWGPUAccelerator())
//	}
func RegisterAccelerator(a GPUAccelerator) error {
	if a == nil {
		return errors.New("gg: accelerator must not be nil")
	}
	if err := a.Init(); err != nil {
		accelMu.Lock()
		accelStatus.InitFailures++
		accelStatus.LastError = err
		if accelStatus.FirstError == nil {
			accelStatus.FirstError = err
		}
		accelMu.Unlock()
		return err
	}
	accelMu.Lock()
	oldRetained := retainedAccel
	oldClosed := retainedClosed
	accel = a
	retainedAccel = a
	retainedClosed = false
	accelStatus.State = AcceleratorReady
	accelStatus.Name = a.Name()
	accelStatus.Registrations++
	accelStatus.Transitions++
	accelStatus.FirstError = nil
	accelStatus.LastError = nil
	accelMu.Unlock()

	// Propagate current logger to the new accelerator so it inherits
	// any SetLogger configuration that was applied before registration.
	propagateLogger(a, Logger())

	// Close the previous accelerator exactly once. A previous accelerator that
	// had already lost its device is still retained, so it must be released
	// here too; Close is never called twice on the same object.
	if oldRetained != nil && oldRetained != a && !oldClosed {
		recordAcceleratorClose()
		oldRetained.Close()
	}
	return nil
}

// Accelerator returns the currently registered GPU accelerator, or nil if none
// is usable. A registered accelerator that reported device loss or has been
// closed returns nil: callers fall back to CPU rather than touching dead
// GPU resources. Use [AcceleratorStatus] to distinguish the states.
func Accelerator() GPUAccelerator {
	accelMu.RLock()
	a := accel
	accelMu.RUnlock()
	return a
}

// CloseAccelerator shuts down the global GPU accelerator, releasing all GPU
// resources (textures, pipelines, device, instance). After this call,
// [Accelerator] returns nil and rendering falls back to CPU.
//
// Call this at application shutdown to prevent VkImage and other GPU memory
// leaks. It is safe to call when no accelerator is registered (no-op).
// CloseAccelerator is idempotent: the underlying Close is invoked at most once
// per registration, even under concurrent calls.
//
// Example:
//
//	defer gg.CloseAccelerator()
func CloseAccelerator() {
	accelMu.Lock()
	a := accel
	accel = nil
	if a == nil {
		// No usable accelerator: close the retained one if that has not
		// happened yet (e.g. it was lost before this call).
		a = retainedAccel
	}
	if a == nil || retainedClosed {
		accelMu.Unlock()
		return
	}
	retainedClosed = true
	accelStatus.State = AcceleratorClosed
	accelStatus.Transitions++
	accelMu.Unlock()

	recordAcceleratorClose()
	a.Close()
}

// ReportAcceleratorDeviceLost marks the registered accelerator as unusable
// after the GPU device was lost (driver reset, provider shutdown, surface
// teardown). Subsequent [Accelerator] calls return nil, so rendering falls
// back to the CPU, and no further GPU resource is used.
//
// The first report is recorded; repeated reports are counted but do not
// change the state. Recovery requires a successful [RegisterAccelerator] or
// [ReinitAccelerator].
//
// Backends call this themselves when they observe a lost device; it is also
// exposed so hosts that own the device (gogpu) can notify gg.
func ReportAcceleratorDeviceLost(err error) {
	if err == nil {
		err = ErrDeviceLost
	}
	accelMu.Lock()
	defer accelMu.Unlock()
	accelStatus.DeviceLosses++
	recordAccelErrorLocked(err)
	// Only a currently-usable accelerator can become lost. Reporting loss for
	// an already-closed or already-lost accelerator is counted but must not
	// rewrite the state (close is terminal until a new registration/reinit).
	if accel == nil {
		return
	}
	accel = nil
	accelStatus.State = AcceleratorDeviceLost
	accelStatus.Transitions++
}

// ReinitAccelerator re-initializes the registered accelerator after it lost
// its device or was closed, returning it to the ready state on success.
//
// It calls Init() on the accelerator again (backends must re-acquire whatever
// resources they released) and, when the accelerator implements
// [DeviceProviderAware] and a provider was set previously, re-applies that
// provider so shared-device setups recover without the host repeating the
// wiring.
//
// It returns [ErrNoAccelerator] when nothing was ever registered. A previously
// closed accelerator may be re-initialized: re-initialization is the
// documented recovery path from [CloseAccelerator] as well as from device
// loss, so both succeed when the backend can acquire its resources again.
func ReinitAccelerator() error {
	accelMu.Lock()
	a := retainedAccel
	provider := lastDeviceProvider
	accelMu.Unlock()
	if a == nil {
		return ErrNoAccelerator
	}
	if err := a.Init(); err != nil {
		accelMu.Lock()
		recordAccelErrorLocked(err)
		accelMu.Unlock()
		return err
	}
	if provider != nil {
		if dpa, ok := a.(DeviceProviderAware); ok {
			if err := dpa.SetDeviceProvider(provider); err != nil {
				accelMu.Lock()
				recordAccelErrorLocked(err)
				accelMu.Unlock()
				return err
			}
		}
	}
	accelMu.Lock()
	accel = a
	retainedClosed = false
	accelStatus.State = AcceleratorReady
	accelStatus.Name = a.Name()
	accelStatus.Transitions++
	accelMu.Unlock()
	return nil
}

// AcceleratorSnapshot returns a snapshot of the accelerator lifecycle. It is
// safe for concurrent use.
func AcceleratorSnapshot() AcceleratorStatus {
	accelMu.RLock()
	defer accelMu.RUnlock()
	return accelStatus
}

// RecordGPUFallback notes that a GPU operation was resolved on the CPU. ctx and
// op are informational; the error is folded into the lifecycle status so
// callers can inspect the first and most recent reasons a fallback happened.
func RecordGPUFallback(ctx, op string, err error) {
	if err == nil {
		err = ErrFallbackToCPU
	}
	accelMu.Lock()
	accelStatus.Fallbacks++
	recordAccelErrorLocked(err)
	accelMu.Unlock()
	if ctx != "" || op != "" {
		Logger().Debug("GPU fallback to CPU", "context", ctx, "op", op, "err", err)
	}
}

// recordAcceleratorClose bumps the close counter and transition count.
func recordAcceleratorClose() {
	accelMu.Lock()
	accelStatus.Closes++
	accelStatus.Transitions++
	accelMu.Unlock()
}

// recordAccelErrorLocked records err as the latest lifecycle error and keeps
// the first one. Callers must hold accelMu.
func recordAccelErrorLocked(err error) {
	if err == nil {
		return
	}
	accelStatus.LastError = err
	if accelStatus.FirstError == nil {
		accelStatus.FirstError = err
	}
}

// ClassifyAcceleratorError reports which lifecycle failure err represents, if
// any. Backends are expected to wrap their concrete error with the matching
// sentinel (errors.Is-compatible) so callers get a stable classification.
// A plain error is classified as a generic backend failure.
func ClassifyAcceleratorError(err error) AcceleratorState {
	switch {
	case err == nil:
		return AcceleratorReady
	case errors.Is(err, ErrAcceleratorClosed):
		return AcceleratorClosed
	case errors.Is(err, ErrDeviceLost):
		return AcceleratorDeviceLost
	default:
		return AcceleratorUnregistered
	}
}

// lastDeviceProvider remembers the most recently applied provider so
// ReinitAccelerator can restore shared-device wiring. Guarded by accelMu.
var lastDeviceProvider gpucontext.DeviceProvider

// SetAcceleratorDeviceProvider passes a device provider to the registered
// accelerator, enabling GPU device sharing. If no accelerator is registered
// or it doesn't support device sharing, this is a no-op.
//
// Passing a nil provider returns [ErrNilDeviceProvider] before touching the
// accelerator, so a misconfigured host cannot crash the backend.
//
// The provider's Device() returns gpucontext.Device; accelerators type-assert
// to *wgpu.Device to obtain HAL access for GPU operations.
func SetAcceleratorDeviceProvider(provider gpucontext.DeviceProvider) error {
	if isNilDeviceProvider(provider) {
		return ErrNilDeviceProvider
	}
	a := Accelerator()
	if a == nil {
		return nil
	}
	dpa, ok := a.(DeviceProviderAware)
	if !ok {
		return nil
	}
	if err := dpa.SetDeviceProvider(provider); err != nil {
		accelMu.Lock()
		recordAccelErrorLocked(err)
		accelMu.Unlock()
		return err
	}
	accelMu.Lock()
	lastDeviceProvider = provider
	accelStatus.ProviderSets++
	accelMu.Unlock()
	return nil
}

// isNilDeviceProvider reports whether provider is nil, including a typed nil
// pointer stored in the interface (whose methods would dereference nil).
func isNilDeviceProvider(provider gpucontext.DeviceProvider) bool {
	if provider == nil {
		return true
	}
	v := reflect.ValueOf(provider)
	//nolint:exhaustive // only nil-capable kinds can carry a typed-nil provider
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Func, reflect.Chan, reflect.Slice, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// AcceleratorCanRenderDirect returns true if the registered GPU accelerator
// is initialized and capable of rendering directly to a surface target.
// Returns false if no accelerator is registered, GPU init failed (e.g.,
// CPU-only adapter like llvmpipe), or the accelerator doesn't support
// direct surface rendering.
//
// Respects GOGPU_RENDER_MODE environment variable (ADR-020):
//   - auto (default): returns false for software adapters (CPU rasterizer is faster)
//   - cpu: always returns false (force CPU rasterizer)
//   - gpu: uses accelerator's own CanRenderDirect (force GPU path)
//
// Use this to decide whether to attempt RenderDirect or go straight to
// the universal CPU→texture→present path.
func AcceleratorCanRenderDirect() bool {
	switch renderMode() {
	case renderModeCPU:
		return false
	case renderModeGPU:
		a := Accelerator()
		if a == nil {
			return false
		}
		if drc, ok := a.(DirectRenderCapable); ok {
			return drc.CanRenderDirect()
		}
		return false
	}

	// Auto mode: check adapter type
	a := Accelerator()
	if a == nil {
		return false
	}
	if aa, ok := a.(AdapterAware); ok {
		if aa.IsSoftwareAdapter() {
			return false
		}
	}
	if drc, ok := a.(DirectRenderCapable); ok {
		return drc.CanRenderDirect()
	}
	return false
}

// DirectRenderCapable is an optional interface for accelerators that can
// report whether they are capable of rendering directly to a surface.
// CPU-only adapters (llvmpipe, SwiftShader) return false because SDF/MSDF
// shaders should not run on CPU — the accelerator stays uninitialized.
type DirectRenderCapable interface {
	CanRenderDirect() bool
}

// AdapterAware is an optional interface for accelerators that know
// whether they are running on a software (CPU) adapter.
// Used by AcceleratorCanRenderDirect in auto mode (ADR-020).
type AdapterAware interface {
	IsSoftwareAdapter() bool
}

// renderModeType represents the 2D rendering path preference.
type renderModeType int

const (
	renderModeAuto renderModeType = iota
	renderModeCPU
	renderModeGPU
)

// renderMode reads GOGPU_RENDER_MODE environment variable.
func renderMode() renderModeType {
	switch strings.ToLower(os.Getenv("GOGPU_RENDER_MODE")) {
	case "cpu":
		return renderModeCPU
	case "gpu":
		return renderModeGPU
	default:
		return renderModeAuto
	}
}

// BeginAcceleratorFrame signals the start of a new rendering frame.
// This resets per-frame state so that the first render pass clears the
// surface. Must be called once per frame before any drawing operations
// when using direct surface rendering (RenderDirect).
//
// If no accelerator is registered (or the registered one is not usable) or it
// doesn't implement FrameAware, this is a no-op.
func BeginAcceleratorFrame() {
	a := Accelerator()
	if a == nil {
		return
	}
	fa, ok := a.(FrameAware)
	if !ok {
		return
	}
	fa.BeginFrame()
	accelMu.Lock()
	accelStatus.Frames++
	accelMu.Unlock()
}

// ComputePipelineAware is an optional interface for accelerators that support
// the Vello-style compute pipeline. When the accelerator implements this interface
// and PipelineMode is Auto or Compute, the framework uses the compute pipeline
// for complex scenes instead of the render pass pipeline.
type ComputePipelineAware interface {
	// CanCompute reports whether the compute pipeline is available and ready.
	CanCompute() bool
}

// PipelineModeAware is an optional interface for accelerators that support
// pipeline mode selection (render pass vs compute). When the registered
// accelerator implements this interface, the Context propagates pipeline mode
// changes so the accelerator can route operations accordingly.
type PipelineModeAware interface {
	// SetPipelineMode sets the pipeline mode for subsequent operations.
	SetPipelineMode(mode PipelineMode)
}

// ForceSDFAware is an optional interface for GPU accelerators that support
// forced SDF rendering. When enabled, the accelerator bypasses the minimum
// size check for SDF shapes, allowing RasterizerSDF mode to force SDF
// rendering regardless of shape size.
type ForceSDFAware interface {
	SetForceSDF(force bool)
}

// ClipAware is an optional interface for accelerators that support
// hardware scissor rect clipping. When the Context has an active
// rectangular clip region (ClipRect), it passes the clip bounds to the
// accelerator as a scissor rect. The accelerator maps this to
// hal.RenderPassEncoder.SetScissorRect() for zero-cost GPU clipping.
//
// This covers ~95% of real-world UI clipping (scroll views, panels,
// list items). Path-based clips require GPU-CLIP-002 (stencil buffer).
type ClipAware interface {
	// SetClipRect sets the scissor rect for subsequent GPU draw commands.
	// Coordinates are in device pixels (uint32). The scissor rect clips
	// all rendering to the rectangle (x, y, w, h).
	SetClipRect(x, y, w, h uint32)

	// ClearClipRect removes the scissor rect, restoring full-framebuffer
	// rendering for subsequent draw commands.
	ClearClipRect()
}

// RRectClipAware is an optional interface for accelerators that support
// GPU-accelerated rounded rectangle clipping via analytic SDF in fragment
// shaders. When the Context has an active RRect clip region (ClipRoundRect),
// it passes the clip parameters to the accelerator. The fragment shader
// evaluates the RRect SDF per pixel and discards fragments outside the
// rounded rectangle.
//
// This is a two-level clip strategy: the scissor rect (via ClipAware)
// provides a free coarse clip to the bounding box, while the RRect SDF
// provides fine per-pixel clipping with anti-aliased rounded corners.
//
// This covers ~95% of non-rectangular UI clipping (card views, dialogs,
// avatars, pill buttons). Path-based clips require GPU-CLIP-003 (stencil).
type RRectClipAware interface {
	// SetClipRRect sets the rounded rectangle clip for subsequent GPU draw
	// commands. Coordinates are in device pixels (float32). The clip region
	// is defined by the rectangle (x, y, w, h) with uniform corner radius.
	SetClipRRect(x, y, w, h, radius float32)

	// ClearClipRRect removes the rounded rectangle clip, restoring full
	// rendering for subsequent draw commands.
	ClearClipRRect()
}

// PathClipAware is an optional interface for accelerators that support
// GPU-accelerated arbitrary path clipping via depth buffer. When the Context
// has an active path-based clip region (dc.Clip() with non-rect/non-rrect
// path), it passes the device-space clip path to the accelerator.
//
// The GPU renders the clip path to the depth buffer using fan tessellation
// (DepthClipPipeline), then content shaders test against the clip depth.
// This is GPU-CLIP-003a — the third level of clip support after scissor rect
// (ClipAware) and SDF rrect (RRectClipAware).
//
// Follows the Flutter Impeller pattern: depth buffer for clip discrimination,
// stencil exclusively for path fill (Tier 2b).
type PathClipAware interface {
	// SetClipPath sets an arbitrary clip path for depth-based clipping.
	// The path is in device-space coordinates. Subsequent GPU draw commands
	// are clipped to the path region via the depth buffer.
	SetClipPath(path *Path)

	// ClearClipPath removes the arbitrary clip path, restoring full
	// rendering for subsequent draw commands.
	ClearClipPath()
}

// LCDLayoutAware is an optional interface for accelerators that accept a
// requested physical subpixel layout. Accepting the preference does not imply
// exact LCD output: a backend may conservatively render grayscale masks when
// it cannot composite per-channel coverage correctly.
//
// Use LCDLayoutRGB for most monitors, LCDLayoutBGR for rare BGR panels,
// or LCDLayoutNone to request grayscale (the default).
type LCDLayoutAware interface {
	// SetLCDLayout records the requested LCD subpixel layout.
	SetLCDLayout(layout LCDLayout)
}

// MaskAware is an optional interface for accelerators that support
// GPU-accelerated alpha masking. When the Context has an active mask,
// it uploads the mask data to the accelerator as a texture. The fragment
// shader multiplies output alpha by the mask texel, eliminating the need
// to fall back to CPU rendering for masked shapes.
//
// SetMaskTexture uploads the mask data (8-bit per pixel, row-major).
// Pass nil data to clear the mask texture.
// ClearMaskTexture removes the mask, restoring unmasked rendering.
type MaskAware interface {
	// SetMaskTexture uploads an alpha mask for GPU rendering.
	// data is an 8-bit grayscale buffer (width*height bytes).
	// The accelerator creates an R8Unorm texture and samples it in the
	// fragment shader to modulate output alpha.
	SetMaskTexture(data []byte, width, height int)

	// ClearMaskTexture removes the GPU mask texture.
	ClearMaskTexture()
}

// SceneStatsTracker is an optional interface for accelerators that track
// per-frame scene statistics for auto pipeline selection. The Context
// does not call these methods directly — the accelerator uses them internally.
type SceneStatsTracker interface {
	// SceneStats returns the accumulated scene statistics for the current frame.
	SceneStats() SceneStats
}
