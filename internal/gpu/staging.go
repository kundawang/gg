//go:build !nogpu

package gpu

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Staging buffer errors.
var (
	// ErrStagingPoolClosed is returned when acquiring from a closed pool.
	ErrStagingPoolClosed = errors.New("wgpu: staging pool is closed")

	// ErrStagingDeviceLost is returned when the pool's device was lost and
	// no new staging buffers can be acquired until Reset is called.
	ErrStagingDeviceLost = errors.New("wgpu: staging pool device lost")

	// ErrTextureTooLarge is returned when texture dimensions or byte size
	// exceed implementation limits (or would overflow integer arithmetic).
	ErrTextureTooLarge = errors.New("wgpu: texture size exceeds limits")

	// ErrTextureFormatMismatch is returned when raw upload data format
	// does not match the texture format.
	ErrTextureFormatMismatch = errors.New("wgpu: pixel format does not match texture format")

	// ErrEmptyPixmap is returned when uploading a pixmap with zero area.
	ErrEmptyPixmap = errors.New("wgpu: pixmap is empty")
)

// Texture size limits. These mirror common WebGPU limits and exist to catch
// integer overflow and runaway allocations before they happen.
const (
	// MaxTextureDimension is the maximum width or height of a texture.
	MaxTextureDimension = 16384

	// MaxTextureBytes is the maximum byte size of a single texture's
	// CPU mirror / staging allocation (1 GiB).
	MaxTextureBytes = 1 << 30

	// copyBytesPerRowAlignment is the required bytes-per-row alignment
	// for GPU texture copies (WebGPU COPY_BYTES_PER_ROW_ALIGNMENT).
	copyBytesPerRowAlignment = 256

	// minStagingSizeClass is the smallest staging buffer bucket (1 KiB).
	minStagingSizeClass = 1024

	// maxStagingPerBucket caps how many idle buffers one bucket retains.
	maxStagingPerBucket = 4
)

// checkTextureSize validates dimensions and returns the tightly packed byte
// size for the given format. It guards against zero/negative dimensions,
// dimensions beyond MaxTextureDimension, and byte-size overflow.
func checkTextureSize(width, height int, format TextureFormat) (int, error) {
	if width <= 0 || height <= 0 {
		return 0, fmt.Errorf("%w: %dx%d", ErrInvalidDimensions, width, height)
	}
	if width > MaxTextureDimension || height > MaxTextureDimension {
		return 0, fmt.Errorf("%w: %dx%d exceeds max dimension %d",
			ErrTextureTooLarge, width, height, MaxTextureDimension)
	}
	bpp := format.BytesPerPixel()
	bytes := uint64(width) * uint64(height) * uint64(bpp) //nolint:gosec // dims validated positive above
	if bytes > MaxTextureBytes {
		return 0, fmt.Errorf("%w: %dx%d %s needs %d bytes, limit is %d",
			ErrTextureTooLarge, width, height, format, bytes, uint64(MaxTextureBytes))
	}
	return int(bytes), nil
}

// alignBytesPerRow rounds a row byte count up to the GPU copy alignment.
func alignBytesPerRow(rowBytes int) int {
	return (rowBytes + copyBytesPerRowAlignment - 1) &^ (copyBytesPerRowAlignment - 1)
}

// stagingBufferSize returns the buffer size needed for an aligned copy of
// height rows: all rows except the last are padded to bytesPerRow.
func stagingBufferSize(bytesPerRow, rowBytes, rows int) int {
	if rows <= 0 {
		return 0
	}
	return bytesPerRow*(rows-1) + rowBytes
}

// stagingSizeClass rounds a byte count up to the next power-of-two bucket,
// with a floor of minStagingSizeClass. Bucketing by size class lets buffers
// serve requests of similar (not just identical) size.
func stagingSizeClass(bytes int) int {
	class := minStagingSizeClass
	for class < bytes {
		class <<= 1
	}
	return class
}

// stagingKey identifies a bucket of interchangeable staging buffers.
// Buffers are bucketed by pixel format, GPU-aligned row stride, and
// power-of-two size class.
type stagingKey struct {
	format      TextureFormat
	sizeClass   int
	bytesPerRow int
}

// StagingMetrics holds atomic counters describing pool activity.
// All fields are safe to read via Snapshot without holding locks.
type StagingMetrics struct {
	// Allocations is the number of freshly allocated buffers.
	Allocations atomic.Uint64

	// Reuses is the number of times an existing pooled buffer was reused.
	Reuses atomic.Uint64

	// Poisoned is the number of buffers discarded due to device loss or
	// transfer failure instead of being returned to the pool.
	Poisoned atomic.Uint64

	// LiveBytes is the current total bytes held by the pool (idle + in use).
	LiveBytes atomic.Int64

	// PeakLiveBytes is the high-water mark of LiveBytes.
	PeakLiveBytes atomic.Int64

	// UploadBytes is the total bytes staged for upload through this pool.
	UploadBytes atomic.Uint64

	// DownloadBytes is the total bytes staged for readback through this pool.
	DownloadBytes atomic.Uint64
}

// StagingMetricsSnapshot is a point-in-time copy of StagingMetrics.
type StagingMetricsSnapshot struct {
	Allocations   uint64
	Reuses        uint64
	Poisoned      uint64
	LiveBytes     int64
	PeakLiveBytes int64
	UploadBytes   uint64
	DownloadBytes uint64
}

// Snapshot returns a consistent-enough copy of the metrics for reporting.
func (m *StagingMetrics) Snapshot() StagingMetricsSnapshot {
	return StagingMetricsSnapshot{
		Allocations:   m.Allocations.Load(),
		Reuses:        m.Reuses.Load(),
		Poisoned:      m.Poisoned.Load(),
		LiveBytes:     m.LiveBytes.Load(),
		PeakLiveBytes: m.PeakLiveBytes.Load(),
		UploadBytes:   m.UploadBytes.Load(),
		DownloadBytes: m.DownloadBytes.Load(),
	}
}

// recordLiveBytes updates the live and peak byte counters by delta.
func (m *StagingMetrics) recordLiveBytes(delta int64) {
	live := m.LiveBytes.Add(delta)
	for {
		peak := m.PeakLiveBytes.Load()
		if live <= peak || m.PeakLiveBytes.CompareAndSwap(peak, live) {
			return
		}
	}
}

// StagingBuffer is a reusable CPU-side staging buffer for texture uploads
// and readbacks. Buffers are bucketed by format, aligned row stride, and
// size class, and are reference counted: a buffer is only returned to the
// pool (and thus reused) when its last reference is released AND it has not
// been poisoned by a transfer failure or device loss.
//
// Concurrency: Retain/Release/Poison are safe for concurrent use. Bytes()
// must only be accessed by a goroutine holding a reference.
type StagingBuffer struct {
	pool *StagingPool
	key  stagingKey

	data        []byte // len == key.sizeClass
	width       int    // logical pixel width of the staged content
	height      int    // logical pixel height of the staged content
	srcStride   int    // stride of the source/destination pixel data
	rowBytes    int    // unpadded bytes per row (width * bpp)
	bytesPerRow int    // GPU-aligned bytes per row

	refs     atomic.Int32
	poisoned atomic.Bool
}

// Bytes returns the usable staging area (size-class length; only the first
// stagingBufferSize(bytesPerRow, rowBytes, height) bytes are meaningful).
func (b *StagingBuffer) Bytes() []byte { return b.data }

// BytesPerRow returns the GPU-aligned row stride of the staged layout.
func (b *StagingBuffer) BytesPerRow() int { return b.bytesPerRow }

// RowBytes returns the unpadded bytes per row of the staged content.
func (b *StagingBuffer) RowBytes() int { return b.rowBytes }

// Width returns the logical pixel width of the staged content.
func (b *StagingBuffer) Width() int { return b.width }

// Height returns the logical pixel height of the staged content.
func (b *StagingBuffer) Height() int { return b.height }

// Format returns the pixel format of the staged content.
func (b *StagingBuffer) Format() TextureFormat { return b.key.format }

// Retain increments the reference count. The caller must hold at least one
// reference already.
func (b *StagingBuffer) Retain() {
	b.refs.Add(1)
}

// Release decrements the reference count. When it reaches zero the buffer is
// returned to its pool for reuse — unless it was poisoned or the pool is
// closed/device-lost, in which case it is discarded. A buffer in flight on
// an async queue (refs > 0) is never reused.
func (b *StagingBuffer) Release() {
	if b.refs.Add(-1) != 0 {
		return
	}
	if b.poisoned.Load() {
		b.pool.discard(b, true)
		return
	}
	b.pool.recycle(b)
}

// Poison marks the buffer as unusable for reuse (device lost, transfer
// failure). The buffer is discarded when its last reference is released.
// Poisoning a buffer that is still referenced is safe: it will not be
// recycled while in use, and will be dropped rather than reused afterward.
func (b *StagingBuffer) Poison() {
	b.poisoned.Store(true)
}

// StagingPool manages reusable staging buffers bucketed by format, aligned
// stride, and size class. It eliminates per-upload temporary allocations on
// the texture path (the "GPU preview re-uploads the same atlas every frame"
// hot path).
//
// Lifecycle semantics:
//   - Acquire hands out a buffer with refcount 1.
//   - Async users Retain before enqueue and Release after completion.
//   - A buffer is reused only after its refcount reaches zero unpoisoned.
//   - NotifyDeviceLost poisons the pool: subsequent Acquires fail and
//     released buffers are discarded instead of recycled, until Reset.
//   - Close discards all idle buffers; in-use buffers are discarded on
//     release.
//
// StagingPool is safe for concurrent use.
type StagingPool struct {
	mu      sync.Mutex
	free    map[stagingKey][]*StagingBuffer
	closed  bool
	lost    bool
	Metrics StagingMetrics
}

// NewStagingPool creates an empty staging buffer pool.
func NewStagingPool() *StagingPool {
	return &StagingPool{
		free: make(map[stagingKey][]*StagingBuffer),
	}
}

// Acquire returns a staging buffer able to hold width x height pixels of the
// given format with the given source stride, laid out with GPU-aligned rows.
// srcStride of 0 means tightly packed (width * BytesPerPixel).
//
// Returns ErrStagingPoolClosed if the pool is closed, ErrStagingDeviceLost
// if the device was lost and not yet reset, and ErrTextureTooLarge /
// ErrInvalidDimensions for invalid or overflowing sizes.
func (p *StagingPool) Acquire(format TextureFormat, width, height, srcStride int) (*StagingBuffer, error) {
	rowBytes, err := checkTextureSize(width, 1, format)
	if err != nil {
		return nil, err
	}
	if _, err := checkTextureSize(width, height, format); err != nil {
		return nil, err
	}
	if srcStride == 0 {
		srcStride = rowBytes
	}
	if srcStride < rowBytes {
		return nil, fmt.Errorf("%w: source stride %d smaller than row bytes %d",
			ErrInvalidDimensions, srcStride, rowBytes)
	}

	bytesPerRow := alignBytesPerRow(rowBytes)
	needed := stagingBufferSize(bytesPerRow, rowBytes, height)
	key := stagingKey{
		format:      format,
		sizeClass:   stagingSizeClass(needed),
		bytesPerRow: bytesPerRow,
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrStagingPoolClosed
	}
	if p.lost {
		p.mu.Unlock()
		return nil, ErrStagingDeviceLost
	}
	if bucket := p.free[key]; len(bucket) > 0 {
		buf := bucket[len(bucket)-1]
		p.free[key] = bucket[:len(bucket)-1]
		p.mu.Unlock()
		buf.width, buf.height, buf.srcStride, buf.rowBytes = width, height, srcStride, rowBytes
		buf.refs.Store(1)
		p.Metrics.Reuses.Add(1)
		return buf, nil
	}
	p.mu.Unlock()

	buf := &StagingBuffer{
		pool:        p,
		key:         key,
		data:        make([]byte, key.sizeClass),
		width:       width,
		height:      height,
		srcStride:   srcStride,
		rowBytes:    rowBytes,
		bytesPerRow: bytesPerRow,
	}
	buf.refs.Store(1)
	p.Metrics.Allocations.Add(1)
	p.Metrics.recordLiveBytes(int64(key.sizeClass))
	return buf, nil
}

// recycle returns an unpoisoned buffer to its bucket. Called by Release.
func (p *StagingPool) recycle(b *StagingBuffer) {
	p.mu.Lock()
	if p.closed || p.lost {
		p.mu.Unlock()
		p.discard(b, false)
		return
	}
	bucket := p.free[b.key]
	if len(bucket) >= maxStagingPerBucket {
		p.mu.Unlock()
		p.discard(b, false)
		return
	}
	p.free[b.key] = append(bucket, b)
	p.mu.Unlock()
}

// discard drops a buffer from the pool accounting (it becomes garbage).
// poisoned should be true only when the buffer was explicitly poisoned by
// a transfer failure or device loss.
func (p *StagingPool) discard(b *StagingBuffer, poisoned bool) {
	if poisoned {
		p.Metrics.Poisoned.Add(1)
	}
	p.Metrics.recordLiveBytes(-int64(b.key.sizeClass))
}

// NotifyDeviceLost marks the pool's device as lost. All idle buffers are
// discarded, subsequent Acquire calls fail with ErrStagingDeviceLost, and
// in-flight buffers are discarded (never reused) as they are released.
// Call Reset after the device has been recreated.
func (p *StagingPool) NotifyDeviceLost() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lost = true
	p.drainLocked()
}

// Reset clears the device-lost state, allowing new acquisitions.
func (p *StagingPool) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lost = false
}

// Close discards all idle buffers and prevents further acquisitions.
// In-use buffers are discarded when released. Close is idempotent.
func (p *StagingPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.drainLocked()
}

// drainLocked discards all idle buffers. Caller must hold mu.
func (p *StagingPool) drainLocked() {
	for key, bucket := range p.free {
		for _, b := range bucket {
			p.Metrics.recordLiveBytes(-int64(b.key.sizeClass))
		}
		delete(p.free, key)
	}
}

// IdleCount returns the number of buffers currently idle in the pool.
func (p *StagingPool) IdleCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, bucket := range p.free {
		n += len(bucket)
	}
	return n
}
