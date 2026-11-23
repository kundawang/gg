//go:build !nogpu

package gpu

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/gogpu/gg"
	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
	"github.com/gogpu/wgpu/core"
)

// Texture-related errors.
var (
	// ErrTextureReleased is returned when operating on a released texture.
	ErrTextureReleased = errors.New("wgpu: texture has been released")

	// ErrTextureSizeMismatch is returned when pixmap size doesn't match texture.
	ErrTextureSizeMismatch = errors.New("wgpu: pixmap size does not match texture")

	// ErrNilPixmap is returned when pixmap is nil.
	ErrNilPixmap = errors.New("wgpu: pixmap is nil")

	// ErrTextureReadbackNotSupported is returned when readback is not available:
	// the texture has no CPU mirror (nothing was ever uploaded) and no GPU
	// readback path is attached.
	ErrTextureReadbackNotSupported = errors.New("wgpu: texture readback not supported (no CPU mirror, no GPU readback path)")
)

// TextureFormat represents the pixel format of a GPU texture.
type TextureFormat uint8

const (
	// TextureFormatRGBA8 is the standard RGBA format with 8 bits per channel.
	TextureFormatRGBA8 TextureFormat = iota

	// TextureFormatBGRA8 is BGRA format, often used for surface presentation.
	TextureFormatBGRA8

	// TextureFormatR8 is single-channel 8-bit format, used for masks.
	TextureFormatR8
)

// String returns a human-readable name for the format.
func (f TextureFormat) String() string {
	switch f {
	case TextureFormatRGBA8:
		return "RGBA8"
	case TextureFormatBGRA8:
		return "BGRA8"
	case TextureFormatR8:
		return "R8"
	default:
		return fmt.Sprintf("Unknown(%d)", f)
	}
}

// BytesPerPixel returns the number of bytes per pixel for the format.
func (f TextureFormat) BytesPerPixel() int {
	switch f {
	case TextureFormatRGBA8, TextureFormatBGRA8:
		return 4
	case TextureFormatR8:
		return 1
	default:
		return 4
	}
}

// ToWGPUFormat converts to wgpu gputypes.TextureFormat.
func (f TextureFormat) ToWGPUFormat() gputypes.TextureFormat {
	switch f {
	case TextureFormatRGBA8:
		return gputypes.TextureFormatRGBA8Unorm
	case TextureFormatBGRA8:
		return gputypes.TextureFormatBGRA8Unorm
	case TextureFormatR8:
		return gputypes.TextureFormatR8Unorm
	default:
		return gputypes.TextureFormatRGBA8Unorm
	}
}

// TextureIOMetrics records byte and operation counters for a texture.
type TextureIOMetrics struct {
	// UploadCount is the number of successful upload operations.
	UploadCount uint64

	// UploadBytes is the total source bytes consumed by uploads.
	UploadBytes uint64

	// DownloadCount is the number of successful readback operations.
	DownloadCount uint64

	// DownloadBytes is the total bytes produced by readbacks.
	DownloadBytes uint64
}

// GPUTexture represents a GPU texture resource.
// It wraps the underlying wgpu texture and provides a high-level interface
// for texture operations including upload and download.
//
// Data model:
//   - Every successful upload updates a CPU-side mirror in the texture's
//     native format (tightly packed). The mirror is the safe fallback for
//     readback when no GPU readback path is available.
//   - When a real GPU device is attached via AttachGPU, uploads are staged
//     through a StagingPool (bucketed, reused buffers with 256-byte row
//     alignment) and submitted with queue.WriteTexture, honoring non-zero
//     origins for region updates.
//   - Pixel data is premultiplied alpha end-to-end (gg.Pixmap convention),
//     so upload → download round-trips losslessly.
//
// Lifecycle and concurrency:
//   - GPUTexture is safe for concurrent use; uploads and downloads are
//     serialized internally.
//   - Async users must Retain before enqueueing work and Release when done.
//     Close marks the texture released; the CPU mirror and GPU handles are
//     freed only when the last reference drops, so in-flight async work
//     never touches freed memory.
type GPUTexture struct {
	mu sync.RWMutex

	// GPU resource IDs (stub - will be real wgpu handles when available)
	textureID core.TextureID
	viewID    core.TextureViewID

	// Real GPU handles (optional, set via AttachGPU).
	device  *wgpu.Device
	queue   *wgpu.Queue
	gpuTex  *wgpu.Texture
	gpuView *wgpu.TextureView

	// Texture properties
	width  int
	height int
	format TextureFormat

	// CPU mirror of the texture contents in native format, tightly packed
	// (stride == width * BytesPerPixel). Nil until the first upload.
	data []byte

	// Staging pool for GPU uploads (bucketed reuse, no per-upload alloc).
	pool    *StagingPool
	ownPool bool // true if pool was created by AttachGPU

	// Memory tracking
	sizeBytes uint64
	manager   *MemoryManager // optional, for memory tracking

	// IO metrics
	uploadCount   atomic.Uint64
	uploadBytes   atomic.Uint64
	downloadCount atomic.Uint64
	downloadBytes atomic.Uint64

	// State
	released atomic.Bool
	refs     atomic.Int32 // in-flight async references
	label    string
}

// TextureConfig holds configuration for creating a new texture.
type TextureConfig struct {
	// Width is the texture width in pixels.
	Width int

	// Height is the texture height in pixels.
	Height int

	// Format is the pixel format.
	Format TextureFormat

	// Label is an optional debug label.
	Label string

	// Usage flags (default: CopySrc | CopyDst | TextureBinding)
	Usage gputypes.TextureUsage
}

// DefaultTextureUsage is the default usage for textures created without specific flags.
const DefaultTextureUsage = gputypes.TextureUsageCopySrc | gputypes.TextureUsageCopyDst | gputypes.TextureUsageTextureBinding

// CreateTexture creates a new GPU texture with the given configuration.
// The texture is uninitialized and should be filled with UploadPixmap.
//
// Dimensions are validated against MaxTextureDimension and MaxTextureBytes
// to catch overflow and runaway allocations early.
func CreateTexture(backend *Backend, config TextureConfig) (*GPUTexture, error) {
	sizeBytes, err := checkTextureSize(config.Width, config.Height, config.Format)
	if err != nil {
		return nil, err
	}

	// Allow nil backend for stub/testing mode
	// When backend is nil, we create a logical texture without GPU resources
	if backend != nil && !backend.IsInitialized() {
		return nil, ErrNotInitialized
	}

	// Set default usage if not specified
	// Note: usage will be used when actual GPU texture creation is implemented
	_ = config.Usage // Acknowledge usage for future GPU texture creation

	tex := &GPUTexture{
		width:     config.Width,
		height:    config.Height,
		format:    config.Format,
		sizeBytes: uint64(sizeBytes), //nolint:gosec // bounded by MaxTextureBytes check
		label:     config.Label,
		// textureID and viewID are zero (stub)
	}

	return tex, nil
}

// CreateTextureFromPixmap creates a GPU texture from a pixmap, uploading
// the pixel data immediately.
func CreateTextureFromPixmap(backend *Backend, pixmap *gg.Pixmap, label string) (*GPUTexture, error) {
	if pixmap == nil {
		return nil, ErrNilPixmap
	}

	tex, err := CreateTexture(backend, TextureConfig{
		Width:  pixmap.Width(),
		Height: pixmap.Height(),
		Format: TextureFormatRGBA8,
		Label:  label,
	})
	if err != nil {
		return nil, err
	}

	if err := tex.UploadPixmap(pixmap); err != nil {
		tex.Close()
		return nil, err
	}

	return tex, nil
}

// AttachGPU connects real GPU handles to this texture. After attaching,
// uploads are staged through a StagingPool and submitted with
// queue.WriteTexture (with non-zero origin for region uploads).
// If pool is nil, a private pool is created and owned by the texture.
// The texture does not take ownership of device, queue, gpuTex, or gpuView.
func (t *GPUTexture) AttachGPU(device *wgpu.Device, queue *wgpu.Queue, gpuTex *wgpu.Texture, gpuView *wgpu.TextureView, pool *StagingPool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.device = device
	t.queue = queue
	t.gpuTex = gpuTex
	t.gpuView = gpuView
	if pool != nil {
		t.pool = pool
		t.ownPool = false
	} else {
		t.pool = NewStagingPool()
		t.ownPool = true
	}
}

// StagingPool returns the texture's staging pool, or nil if no GPU is attached.
func (t *GPUTexture) StagingPool() *StagingPool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.pool
}

// Width returns the texture width in pixels.
func (t *GPUTexture) Width() int {
	return t.width
}

// Height returns the texture height in pixels.
func (t *GPUTexture) Height() int {
	return t.height
}

// Format returns the texture format.
func (t *GPUTexture) Format() TextureFormat {
	return t.format
}

// SizeBytes returns the texture size in bytes.
func (t *GPUTexture) SizeBytes() uint64 {
	return t.sizeBytes
}

// Label returns the debug label.
func (t *GPUTexture) Label() string {
	return t.label
}

// IsReleased returns true if the texture has been released.
func (t *GPUTexture) IsReleased() bool {
	return t.released.Load()
}

// Metrics returns a snapshot of the texture's IO counters.
func (t *GPUTexture) Metrics() TextureIOMetrics {
	return TextureIOMetrics{
		UploadCount:   t.uploadCount.Load(),
		UploadBytes:   t.uploadBytes.Load(),
		DownloadCount: t.downloadCount.Load(),
		DownloadBytes: t.downloadBytes.Load(),
	}
}

// TextureID returns the underlying wgpu texture ID.
// Returns a zero ID for stub textures.
func (t *GPUTexture) TextureID() core.TextureID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.textureID
}

// ViewID returns the texture view ID.
// Returns a zero ID for stub textures.
func (t *GPUTexture) ViewID() core.TextureViewID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.viewID
}

// Retain increments the async reference count. Async users (upload queues,
// readback jobs) must call Retain before enqueueing and Release when done.
// Retain on a released texture is still safe; resources are freed only when
// the count reaches zero after Close.
func (t *GPUTexture) Retain() {
	t.refs.Add(1)
}

// Release decrements the async reference count, freeing resources if the
// texture was closed and this was the last reference.
func (t *GPUTexture) Release() {
	if t.refs.Add(-1) == 0 && t.released.Load() {
		t.freeResources()
	}
}

// UploadPixmap uploads pixel data from a Pixmap to the GPU texture.
// The pixmap dimensions must match the texture dimensions.
//
// The pixmap's premultiplied RGBA data is converted to the texture format
// (RGBA8 copy, BGRA8 swizzle, R8 alpha extraction), stored in the CPU
// mirror, and — when a GPU is attached — staged through the pool and
// written to the GPU texture.
func (t *GPUTexture) UploadPixmap(pixmap *gg.Pixmap) error {
	if t.released.Load() {
		return ErrTextureReleased
	}

	if pixmap == nil {
		return ErrNilPixmap
	}

	if pixmap.Width() != t.width || pixmap.Height() != t.height {
		return fmt.Errorf("%w: expected %dx%d, got %dx%d",
			ErrTextureSizeMismatch, t.width, t.height, pixmap.Width(), pixmap.Height())
	}

	return t.uploadRegion(0, 0, pixmap)
}

// UploadRegion uploads pixel data to a region of the texture at a non-zero
// origin. This is useful for texture atlas updates. The pixmap provides
// exactly region.Width x region.Height pixels.
func (t *GPUTexture) UploadRegion(x, y int, pixmap *gg.Pixmap) error {
	if t.released.Load() {
		return ErrTextureReleased
	}

	if pixmap == nil {
		return ErrNilPixmap
	}

	if err := t.checkRegion(x, y, pixmap.Width(), pixmap.Height()); err != nil {
		return err
	}

	return t.uploadRegion(x, y, pixmap)
}

// checkRegion validates a region against texture bounds without integer
// overflow (x and y are range-checked before any addition).
func (t *GPUTexture) checkRegion(x, y, w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("%w: empty region %dx%d", ErrEmptyPixmap, w, h)
	}
	if x < 0 || y < 0 || x > t.width || y > t.height ||
		w > t.width-x || h > t.height-y {
		return fmt.Errorf("%w: region (%d,%d)+(%dx%d) exceeds texture bounds (%dx%d)",
			ErrInvalidDimensions, x, y, w, h, t.width, t.height)
	}
	return nil
}

// uploadRegion performs the actual upload of a validated pixmap region.
func (t *GPUTexture) uploadRegion(x, y int, pixmap *gg.Pixmap) error {
	w, h := pixmap.Width(), pixmap.Height()
	if w <= 0 || h <= 0 {
		return fmt.Errorf("%w: %dx%d", ErrEmptyPixmap, w, h)
	}
	src := pixmap.Data()
	srcStride := w * 4 // gg.Pixmap is always tightly packed RGBA

	t.mu.Lock()
	defer t.mu.Unlock()

	// Allocate the CPU mirror lazily on first upload.
	if t.data == nil {
		t.data = make([]byte, t.sizeBytes)
	}

	// Update the CPU mirror (safe fallback for readback).
	convertToTextureFormat(t.data, t.width*t.format.BytesPerPixel(),
		src, srcStride, x, y, w, h, t.format)

	// GPU upload path: stage through the pool with aligned rows.
	if t.queue != nil && t.gpuTex != nil && t.pool != nil {
		if err := t.stageAndWriteLocked(src, srcStride, x, y, w, h); err != nil {
			return err
		}
	}

	t.uploadCount.Add(1)
	t.uploadBytes.Add(uint64(w * h * 4)) //nolint:gosec // dims bounded by MaxTextureDimension
	return nil
}

// stageAndWriteLocked packs source pixels into a pooled staging buffer with
// GPU-aligned rows and submits queue.WriteTexture. Caller must hold mu.
func (t *GPUTexture) stageAndWriteLocked(src []byte, srcStride, x, y, w, h int) error {
	buf, err := t.pool.Acquire(t.format, w, h, srcStride)
	if err != nil {
		return fmt.Errorf("staging acquire for %dx%d %s upload: %w", w, h, t.format, err)
	}
	defer buf.Release()

	// Pack rows into the aligned staging layout, converting format.
	convertToTextureFormat(buf.Bytes(), buf.BytesPerRow(), src, srcStride, 0, 0, w, h, t.format)

	used := stagingBufferSize(buf.BytesPerRow(), buf.RowBytes(), h)
	if err := t.queue.WriteTexture(
		&wgpu.ImageCopyTexture{
			Texture:  t.gpuTex,
			MipLevel: 0,
			Origin:   wgpu.Origin3D{X: uint32(x), Y: uint32(y), Z: 0}, //nolint:gosec // bounds-checked in checkRegion
		},
		buf.Bytes()[:used],
		&wgpu.ImageDataLayout{
			Offset:       0,
			BytesPerRow:  uint32(buf.BytesPerRow()), //nolint:gosec // aligned row stride fits uint32
			RowsPerImage: uint32(h),                 //nolint:gosec // bounded by MaxTextureDimension
		},
		&wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1}, //nolint:gosec // bounded by MaxTextureDimension
	); err != nil {
		// Transfer failed: the staging buffer must not be reused.
		buf.Poison()
		return fmt.Errorf("write texture %s at (%d,%d): %w", t.label, x, y, err)
	}

	t.pool.Metrics.UploadBytes.Add(uint64(used)) //nolint:gosec // used is positive
	return nil
}

// UploadRawRegion uploads raw pixel data (already in the texture's format)
// to a region. srcStride of 0 means tightly packed. The data format must
// match the texture format exactly — use UploadRegion for automatic
// conversion from premultiplied RGBA pixmaps.
func (t *GPUTexture) UploadRawRegion(x, y, w, h int, data []byte, srcStride int, format TextureFormat) error {
	if t.released.Load() {
		return ErrTextureReleased
	}
	if format != t.format {
		return fmt.Errorf("%w: texture is %s, data is %s",
			ErrTextureFormatMismatch, t.format, format)
	}
	if err := t.checkRegion(x, y, w, h); err != nil {
		return err
	}

	bpp := t.format.BytesPerPixel()
	rowBytes := w * bpp
	if srcStride == 0 {
		srcStride = rowBytes
	}
	if srcStride < rowBytes {
		return fmt.Errorf("%w: source stride %d smaller than row bytes %d",
			ErrInvalidDimensions, srcStride, rowBytes)
	}
	need := srcStride*(h-1) + rowBytes
	if len(data) < need {
		return fmt.Errorf("%w: need %d bytes for %dx%d stride %d, have %d",
			ErrInvalidDimensions, need, w, h, srcStride, len(data))
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.data == nil {
		t.data = make([]byte, t.sizeBytes)
	}

	// Copy rows into the mirror (no format conversion needed).
	dstStride := t.width * bpp
	for row := 0; row < h; row++ {
		dstOff := (y+row)*dstStride + x*bpp
		srcOff := row * srcStride
		copy(t.data[dstOff:dstOff+rowBytes], data[srcOff:srcOff+rowBytes])
	}

	if t.queue != nil && t.gpuTex != nil && t.pool != nil {
		if err := t.stageAndWriteLocked(data, srcStride, x, y, w, h); err != nil {
			return err
		}
	}

	t.uploadCount.Add(1)
	t.uploadBytes.Add(uint64(need)) //nolint:gosec // need is positive
	return nil
}

// DownloadPixmap downloads the full texture contents into a new Pixmap.
//
// Readback strategy: the CPU mirror (maintained by every upload) is the
// safe fallback and is always used when present. Textures that were never
// uploaded and have no GPU readback path return ErrTextureReadbackNotSupported.
func (t *GPUTexture) DownloadPixmap() (*gg.Pixmap, error) {
	return t.DownloadRegion(0, 0, t.width, t.height)
}

// DownloadRegion downloads a rectangular region of the texture into a new
// Pixmap of size w x h. The region origin may be non-zero.
func (t *GPUTexture) DownloadRegion(x, y, w, h int) (*gg.Pixmap, error) {
	if t.released.Load() {
		return nil, ErrTextureReleased
	}
	if err := t.checkRegion(x, y, w, h); err != nil {
		return nil, err
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.data == nil {
		return nil, fmt.Errorf("%w: texture %q (%dx%d %s) has no uploaded data",
			ErrTextureReadbackNotSupported, t.label, t.width, t.height, t.format)
	}

	pm := gg.NewPixmap(w, h)
	dst := pm.Data()
	dstStride := w * 4
	srcStride := t.width * t.format.BytesPerPixel()
	convertFromTextureFormat(dst, dstStride, t.data, srcStride, x, y, w, h, t.format)

	t.downloadCount.Add(1)
	t.downloadBytes.Add(uint64(w * h * 4)) //nolint:gosec // dims bounded by MaxTextureDimension
	return pm, nil
}

// convertToTextureFormat converts a w x h block of premultiplied RGBA source
// pixels into the texture format, writing into dst at pixel origin (x, y).
// dstStride and srcStride are byte strides of the destination and source.
func convertToTextureFormat(dst []byte, dstStride int, src []byte, srcStride, x, y, w, h int, format TextureFormat) {
	switch format {
	case TextureFormatRGBA8:
		for row := 0; row < h; row++ {
			d := (y+row)*dstStride + x*4
			s := row * srcStride
			copy(dst[d:d+w*4], src[s:s+w*4])
		}
	case TextureFormatBGRA8:
		for row := 0; row < h; row++ {
			d := (y+row)*dstStride + x*4
			s := row * srcStride
			for i := 0; i < w*4; i += 4 {
				dst[d+i+0] = src[s+i+2]
				dst[d+i+1] = src[s+i+1]
				dst[d+i+2] = src[s+i+0]
				dst[d+i+3] = src[s+i+3]
			}
		}
	case TextureFormatR8:
		// Coverage masks: store the alpha channel.
		for row := 0; row < h; row++ {
			d := (y+row)*dstStride + x
			s := row * srcStride
			for i := 0; i < w; i++ {
				dst[d+i] = src[s+i*4+3]
			}
		}
	}
}

// convertFromTextureFormat converts a w x h block of texture-format pixels
// at origin (x, y) back into premultiplied RGBA for a Pixmap.
func convertFromTextureFormat(dst []byte, dstStride int, src []byte, srcStride, x, y, w, h int, format TextureFormat) {
	switch format {
	case TextureFormatRGBA8:
		for row := 0; row < h; row++ {
			d := row * dstStride
			s := (y+row)*srcStride + x*4
			copy(dst[d:d+w*4], src[s:s+w*4])
		}
	case TextureFormatBGRA8:
		for row := 0; row < h; row++ {
			d := row * dstStride
			s := (y+row)*srcStride + x*4
			for i := 0; i < w*4; i += 4 {
				dst[d+i+0] = src[s+i+2]
				dst[d+i+1] = src[s+i+1]
				dst[d+i+2] = src[s+i+0]
				dst[d+i+3] = src[s+i+3]
			}
		}
	case TextureFormatR8:
		// Expand coverage to a white mask with alpha = coverage.
		for row := 0; row < h; row++ {
			d := row * dstStride
			s := (y+row)*srcStride + x
			for i := 0; i < w; i++ {
				v := src[s+i]
				dst[d+i*4+0] = 255
				dst[d+i*4+1] = 255
				dst[d+i*4+2] = 255
				dst[d+i*4+3] = v
			}
		}
	}
}

// SetMemoryManager sets the memory manager for tracking.
// This is called internally when allocating through MemoryManager.
func (t *GPUTexture) SetMemoryManager(m *MemoryManager) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.manager = m
}

// Close releases the GPU texture resources.
// Uploads and downloads after Close return ErrTextureReleased.
// If async references are still held, the CPU mirror and GPU handles are
// freed when the last reference is released — in-flight work never sees
// freed memory. Close is idempotent.
func (t *GPUTexture) Close() {
	if t.released.Swap(true) {
		return // Already released
	}

	if t.refs.Load() == 0 {
		t.freeResources()
	}
}

// freeResources releases the mirror, GPU handles, and pool. Called at most
// once: either directly by Close, or by the last Release after Close.
func (t *GPUTexture) freeResources() {
	t.mu.Lock()
	manager := t.manager
	t.manager = nil
	t.data = nil
	t.textureID = core.TextureID{}
	t.viewID = core.TextureViewID{}
	t.device = nil
	t.queue = nil
	t.gpuTex = nil
	t.gpuView = nil
	pool := t.pool
	ownPool := t.ownPool
	t.pool = nil
	t.mu.Unlock()

	// Notify memory manager if present (outside the lock to avoid deadlock).
	if manager != nil {
		manager.unregisterTexture(t)
	}
	if ownPool && pool != nil {
		pool.Close()
	}
}

// String returns a string representation of the texture.
func (t *GPUTexture) String() string {
	status := "active"
	if t.released.Load() {
		status = "released"
	}
	return fmt.Sprintf("GPUTexture[%s %dx%d %s %d bytes %s]",
		t.label, t.width, t.height, t.format, t.sizeBytes, status)
}
