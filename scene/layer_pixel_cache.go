package scene

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gogpu/gg"
)

// LayerKey is the full signature of a cacheable layer rasterization.
//
// Every field participates in equality: two layers share a cached pixmap
// only when their content hash, device scale, filter descriptor, and
// layer compositing parameters all match. In particular, different device
// scales are different keys, so a pixmap rasterized at scale 1.0 can never
// be reused for a 2.0 display.
type LayerKey struct {
	// ContentHash is the FNV-1a hash of the layer's sub-encoding, mixed
	// with the resource versions of every referenced image.
	ContentHash uint64

	// DeviceScale is the target device pixel ratio, quantized to avoid
	// floating-point noise (see QuantizeScale). A layer rasterized at one
	// scale is never served at another.
	DeviceScale float32

	// FilterHash identifies the filter chain applied after rasterization
	// (zero for unfiltered layers). Any change to a filter parameter
	// changes this hash and therefore forces a rebuild.
	FilterHash uint64

	// Blend and Alpha are the layer compositing parameters. They do not
	// affect the rasterized pixels of an isolated layer buffer, but are
	// part of the signature so callers that key layers only through the
	// cache never conflate two differently-composited layers.
	Blend BlendMode
	Alpha float32

	// LayerID identifies the static layer slot within a scene (0 for the
	// first pushed layer, 1 for the next, ...). It makes keys unique
	// across sibling layers that happen to hash identically.
	LayerID uint32
}

// QuantizeScale rounds a device scale to 4 decimal places so that
// mathematically equal scales expressed with float noise (e.g. 1.0000001)
// map to the same cache key while genuinely different scales stay apart.
func QuantizeScale(scale float32) float32 {
	if scale <= 0 {
		return 1
	}
	const q = 10000
	return float32(int64(scale*q+0.5)) / q
}

// LayerSnapshot is an immutable view of a cached layer rasterization.
//
// The underlying pixmap is owned by the cache and must never be modified.
// Snapshot deliberately does not expose the *gg.Pixmap; callers read
// pixels through PixelAt/Pixels/Blit, all of which either copy or blend
// read-only. A cached entry can therefore be shared across goroutines
// without defensive copies on every hit.
type LayerSnapshot struct {
	pixmap  *gg.Pixmap
	originX int
	originY int
	bounds  Rect
	key     LayerKey
}

// Width returns the pixmap width in device pixels.
func (s *LayerSnapshot) Width() int { return s.pixmap.Width() }

// Height returns the pixmap height in device pixels.
func (s *LayerSnapshot) Height() int { return s.pixmap.Height() }

// OriginX returns the canvas-space X origin of the pixmap's top-left pixel.
func (s *LayerSnapshot) OriginX() int { return s.originX }

// OriginY returns the canvas-space Y origin of the pixmap's top-left pixel.
func (s *LayerSnapshot) OriginY() int { return s.originY }

// Bounds returns the canvas-space bounds covered by the pixmap.
// The returned value is a copy; mutating it does not affect the cache.
func (s *LayerSnapshot) Bounds() Rect { return s.bounds }

// Key returns the signature the snapshot was stored under.
func (s *LayerSnapshot) Key() LayerKey { return s.key }

// PixelAt returns the premultiplied-RGBA pixel at pixmap-local coordinates.
// Returns a fully transparent pixel outside the pixmap.
func (s *LayerSnapshot) PixelAt(x, y int) gg.RGBA {
	if x < 0 || y < 0 || x >= s.pixmap.Width() || y >= s.pixmap.Height() {
		return gg.Transparent
	}
	return s.pixmap.GetPixel(x, y)
}

// Pixels returns a copy of the pixmap's premultiplied-RGBA bytes.
// Callers own the returned slice and may modify it freely.
func (s *LayerSnapshot) Pixels() []byte {
	src := s.pixmap.Data()
	out := make([]byte, len(src))
	copy(out, src)
	return out
}

// Blit composites the cached pixmap onto dst using premultiplied
// source-over blending, at the snapshot's canvas origin. dst is
// interpreted as a canvas-sized pixmap of the given width.
func (s *LayerSnapshot) Blit(dst *gg.Pixmap) {
	if dst == nil {
		return
	}
	blitSnapshot(dst, s.pixmap, s.originX, s.originY)
}

// LayerBuildFunc rasterizes a layer on demand. The returned pixmap must be
// freshly allocated (or otherwise unshared); the cache takes ownership and
// never mutates it. Bounds are the canvas-space rect the pixmap covers,
// origin is its top-left canvas coordinate. Returning a nil pixmap makes
// the build a no-op (nothing is cached, a nil snapshot is returned).
type LayerBuildFunc func() (pixmap *gg.Pixmap, originX, originY int, bounds Rect)

// PixelCacheStats are observable counters for a LayerPixelCache.
type PixelCacheStats struct {
	// Size is the current resident memory in bytes.
	Size int64
	// MaxSize is the configured byte budget.
	MaxSize int64
	// PeakSize is the high-water mark of resident bytes.
	PeakSize int64
	// Entries is the current number of resident entries.
	Entries int
	// Hits counts successful Lookup calls.
	Hits uint64
	// Misses counts Lookup calls that found nothing.
	Misses uint64
	// Rebuilds counts entries produced by Store/Warm (actual rasterizations
	// whose result entered the cache).
	Rebuilds uint64
	// Evictions counts entries removed to satisfy the byte budget or
	// explicit invalidation.
	Evictions uint64
	// Oversized counts rasterizations rejected because they alone exceed
	// the budget. Their pixmaps are never stored.
	Oversized uint64
	// InFlight is the number of warmup builds currently running.
	InFlight int
	// Canceled counts warmup calls aborted by context cancellation.
	Canceled uint64
	// Disabled is true when the budget is zero: the cache stores nothing
	// and every lookup misses (deterministic full-render fallback).
	Disabled bool
}

// layerEntry is one immutable cache record.
type layerEntry struct {
	pixmap  *gg.Pixmap
	originX int
	originY int
	bounds  Rect
	key     LayerKey
	size    int64
	stored  time.Time
	element *listElement
}

// LayerPixelCache caches rasterized layer pixmaps keyed by a full
// (layer, scale, filter) signature.
//
// Guarantees:
//   - Entries are immutable after insertion; lookups return snapshots that
//     never expose the internal pixmap for mutation.
//   - Eviction is LRU and bounded by an explicit byte budget, observable
//     through Stats.
//   - A zero budget disables caching deterministically (every lookup is a
//     miss; builds still run and return one-off snapshots).
//   - Warmup is single-flight per key and context-aware: concurrent callers
//     for the same key share one build, and a canceled caller gets
//     ctx.Err() without corrupting the cache.
//   - All methods are safe for concurrent use.
type LayerPixelCache struct {
	mu      sync.Mutex
	entries map[LayerKey]*layerEntry
	lru     lruList
	size    int64
	maxSize int64
	peak    int64

	// inFlight tracks running warm builds for single-flight.
	inFlight map[LayerKey]*inflightBuild

	hits      atomic.Uint64
	misses    atomic.Uint64
	rebuilds  atomic.Uint64
	evictions atomic.Uint64
	oversized atomic.Uint64
	Canceled  atomic.Uint64
}

// inflightBuild is a shared warmup attempt for one key.
type inflightBuild struct {
	done chan struct{}
	snap *LayerSnapshot
	err  error
}

// NewLayerPixelCache creates a cache with the given byte budget.
// A budget of zero creates a disabled cache that never stores anything but
// remains fully callable (all operations fall back to direct rendering).
func NewLayerPixelCache(maxBytes int64) *LayerPixelCache {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return &LayerPixelCache{
		entries:  make(map[LayerKey]*layerEntry),
		maxSize:  maxBytes,
		inFlight: make(map[LayerKey]*inflightBuild),
	}
}

// Enabled reports whether the cache has a non-zero budget.
func (c *LayerPixelCache) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxSize > 0
}

// SetBudget replaces the byte budget. A zero budget disables the cache and
// clears all entries; a smaller budget evicts LRU entries until the new
// limit is satisfied.
func (c *LayerPixelCache) SetBudget(maxBytes int64) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	c.mu.Lock()
	c.maxSize = maxBytes
	if maxBytes == 0 {
		c.clearLocked()
	} else {
		c.evictUntilLocked(maxBytes)
	}
	c.mu.Unlock()
}

// Budget returns the current byte budget.
func (c *LayerPixelCache) Budget() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxSize
}

// Lookup returns an immutable snapshot for key, or nil on miss.
// Hits and misses are counted for Stats.
func (c *LayerPixelCache) Lookup(key LayerKey) *LayerSnapshot {
	c.mu.Lock()
	if c.maxSize == 0 {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil
	}
	entry, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil
	}
	c.lru.MoveToFront(entry.element)
	snap := &LayerSnapshot{
		pixmap:  entry.pixmap,
		originX: entry.originX,
		originY: entry.originY,
		bounds:  entry.bounds,
		key:     entry.key,
	}
	c.mu.Unlock()
	c.hits.Add(1)
	return snap
}

// Store inserts a freshly rasterized pixmap under key. The cache takes
// ownership of pixmap; callers must not mutate it afterwards.
//
// Deterministic fallbacks:
//   - disabled cache (zero budget): nothing is stored; the caller keeps
//     using the pixmap directly for the current frame.
//   - pixmap larger than the whole budget: nothing is stored and the
//     oversized counter is incremented.
//   - budget pressure: LRU entries are evicted first. If even an empty
//     cache cannot fit it, the pixmap is rejected (never partially cached).
func (c *LayerPixelCache) Store(key LayerKey, pixmap *gg.Pixmap, originX, originY int, bounds Rect) *LayerSnapshot {
	if pixmap == nil {
		return nil
	}
	entrySize := pixmapSize(pixmap)
	if entrySize <= 0 {
		return nil
	}

	c.mu.Lock()
	if c.maxSize == 0 {
		c.mu.Unlock()
		// Disabled: hand back a one-off snapshot without storing.
		return &LayerSnapshot{pixmap: pixmap, originX: originX, originY: originY, bounds: bounds, key: key}
	}
	if entrySize > c.maxSize {
		c.mu.Unlock()
		c.oversized.Add(1)
		return &LayerSnapshot{pixmap: pixmap, originX: originX, originY: originY, bounds: bounds, key: key}
	}

	// Replace any existing entry for the same key.
	if old, ok := c.entries[key]; ok {
		c.removeEntryLocked(old)
	}
	c.evictUntilLocked(c.maxSize - entrySize)

	entry := &layerEntry{
		pixmap:  pixmap,
		originX: originX,
		originY: originY,
		bounds:  bounds,
		key:     key,
		size:    entrySize,
		stored:  time.Now(),
	}
	entry.element = c.lru.PushFront(entry)
	c.entries[key] = entry
	c.size += entrySize
	if c.size > c.peak {
		c.peak = c.size
	}
	snap := &LayerSnapshot{
		pixmap:  entry.pixmap,
		originX: entry.originX,
		originY: entry.originY,
		bounds:  entry.bounds,
		key:     entry.key,
	}
	c.mu.Unlock()

	c.rebuilds.Add(1)
	return snap
}

// ErrNoBuildFunc is returned by Warm when its build function is nil.
var ErrNoBuildFunc = errors.New("scene: layer cache warmup requires a non-nil build function")

// Warm rasterizes key via build and stores the result, with single-flight
// semantics: concurrent Warm calls for the same key share one build and
// return the same snapshot.
//
// Cancellation: if ctx is canceled while waiting for an in-flight build,
// this call returns ctx.Err() and counts a cancellation. The shared build
// itself is not interrupted (another caller may be waiting for it); its
// result is still cached if it succeeds.
//
// When the cache is disabled the build still runs and its snapshot is
// returned without being stored, so warmup-based code paths keep working.
func (c *LayerPixelCache) Warm(ctx context.Context, key LayerKey, build LayerBuildFunc) (*LayerSnapshot, error) {
	if build == nil {
		return nil, ErrNoBuildFunc
	}
	// An already-canceled context fails deterministically without building.
	if err := ctx.Err(); err != nil {
		c.Canceled.Add(1)
		return nil, err
	}

	c.mu.Lock()
	// Fast path: already resident.
	if c.maxSize > 0 {
		if entry, ok := c.entries[key]; ok {
			c.lru.MoveToFront(entry.element)
			snap := snapshotOf(entry)
			c.mu.Unlock()
			c.hits.Add(1)
			return snap, nil
		}
	}
	// Single-flight: join an existing build.
	if ib, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		select {
		case <-ib.done:
			return ib.snap, ib.err
		case <-ctx.Done():
			c.Canceled.Add(1)
			return nil, ctx.Err()
		}
	}
	ib := &inflightBuild{done: make(chan struct{})}
	c.inFlight[key] = ib
	c.mu.Unlock()

	// Run the build without holding any lock.
	pixmap, ox, oy, b := build()
	snap := c.Store(key, pixmap, ox, oy, b)

	c.mu.Lock()
	delete(c.inFlight, key)
	ib.snap = snap
	ib.err = nil
	close(ib.done)
	c.mu.Unlock()

	return snap, nil
}

// InFlight returns the number of currently running warmup builds.
func (c *LayerPixelCache) InFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inFlight)
}

// Invalidate removes a single signature. It is a precise invalidation
// primitive used when one layer is known to have changed.
func (c *LayerPixelCache) Invalidate(key LayerKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[key]; ok {
		c.removeEntryLocked(entry)
		c.evictions.Add(1)
	}
}

// InvalidateLayer removes every cached signature for a layer slot, across
// all scales and filters. Used when the layer's geometry or clip changes.
func (c *LayerPixelCache) InvalidateLayer(layerID uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var removed int
	for key, entry := range c.entries {
		if key.LayerID == layerID {
			c.removeEntryLocked(entry)
			removed++
		}
	}
	if removed > 0 {
		c.evictions.Add(uint64(removed))
	}
}

// InvalidateAll clears every entry (explicit clear). Budget is retained.
func (c *LayerPixelCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
}

// Stats returns a point-in-time copy of the cache counters.
func (c *LayerPixelCache) Stats() PixelCacheStats {
	c.mu.Lock()
	s := PixelCacheStats{
		Size:     c.size,
		MaxSize:  c.maxSize,
		PeakSize: c.peak,
		Entries:  len(c.entries),
		InFlight: len(c.inFlight),
		Disabled: c.maxSize == 0,
	}
	c.mu.Unlock()
	s.Hits = c.hits.Load()
	s.Misses = c.misses.Load()
	s.Rebuilds = c.rebuilds.Load()
	s.Evictions = c.evictions.Load()
	s.Oversized = c.oversized.Load()
	s.Canceled = c.Canceled.Load()
	return s
}

// ResetStats zeroes the monotonic counters. Resident entries and the peak
// high-water mark are reset together so a fresh monitoring window starts
// from the current resident size.
func (c *LayerPixelCache) ResetStats() {
	c.hits.Store(0)
	c.misses.Store(0)
	c.rebuilds.Store(0)
	c.evictions.Store(0)
	c.oversized.Store(0)
	c.Canceled.Store(0)
	c.mu.Lock()
	c.peak = c.size
	c.mu.Unlock()
}

// Size returns current resident bytes.
func (c *LayerPixelCache) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

// EntryCount returns the number of resident entries.
func (c *LayerPixelCache) EntryCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// --- internal helpers (call with mu held) ---

func (c *LayerPixelCache) removeEntryLocked(entry *layerEntry) {
	c.lru.Remove(entry.element)
	delete(c.entries, entry.key)
	c.size -= entry.size
}

func (c *LayerPixelCache) clearLocked() {
	removed := len(c.entries)
	c.entries = make(map[LayerKey]*layerEntry)
	c.lru.Init()
	c.size = 0
	if removed > 0 {
		c.evictions.Add(uint64(removed))
	}
}

// evictUntilLocked evicts least-recently-used entries until size <= target.
func (c *LayerPixelCache) evictUntilLocked(target int64) {
	for c.size > target {
		elem := c.lru.Back()
		if elem == nil {
			return
		}
		entry := elem.Value
		c.removeEntryLocked(entry)
		c.evictions.Add(1)
	}
}

func snapshotOf(entry *layerEntry) *LayerSnapshot {
	return &LayerSnapshot{
		pixmap:  entry.pixmap,
		originX: entry.originX,
		originY: entry.originY,
		bounds:  entry.bounds,
		key:     entry.key,
	}
}

// blitSnapshot blends src onto dst at canvas offset (ox, oy) using
// premultiplied source-over.
//
//nolint:gosec // G115: index conversions bounded by width/height checks
func blitSnapshot(dst, src *gg.Pixmap, ox, oy int) {
	dstW, dstH := dst.Width(), dst.Height()
	srcW, srcH := src.Width(), src.Height()
	dstData := dst.Data()
	srcData := src.Data()
	dstStride := dstW * 4
	srcStride := srcW * 4

	startY := max(0, -oy)
	startX := max(0, -ox)
	endY := min(srcH, dstH-oy)
	endX := min(srcW, dstW-ox)

	for sy := startY; sy < endY; sy++ {
		dy := sy + oy
		srcRow := sy * srcStride
		dstRow := dy * dstStride
		for sx := startX; sx < endX; sx++ {
			dx := sx + ox
			srcOff := srcRow + sx*4
			dstOff := dstRow + dx*4
			if dstOff+3 >= len(dstData) || srcOff+3 >= len(srcData) {
				continue
			}
			sa := srcData[srcOff+3]
			if sa == 0 {
				continue
			}
			if sa == 255 {
				dstData[dstOff] = srcData[srcOff]
				dstData[dstOff+1] = srcData[srcOff+1]
				dstData[dstOff+2] = srcData[srcOff+2]
				dstData[dstOff+3] = 255
				continue
			}
			invAlpha := 255 - uint32(sa)
			dstData[dstOff] = srcData[srcOff] + uint8((uint32(dstData[dstOff])*invAlpha+127)/255)
			dstData[dstOff+1] = srcData[srcOff+1] + uint8((uint32(dstData[dstOff+1])*invAlpha+127)/255)
			dstData[dstOff+2] = srcData[srcOff+2] + uint8((uint32(dstData[dstOff+2])*invAlpha+127)/255)
			dstData[dstOff+3] = sa + uint8((uint32(dstData[dstOff+3])*invAlpha+127)/255)
		}
	}
}
