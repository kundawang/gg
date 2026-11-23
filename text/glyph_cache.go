package text

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"sync"
	"sync/atomic"
)

// GlyphCacheObserver receives cache lifecycle events. It is intended for
// diagnostics and tests that need to prove a hot-reloaded font does not reuse
// outlines from the previous source generation.
//
// Observer callbacks are invoked synchronously, without any cache lock held,
// and may run concurrently from multiple goroutines. Implementations must be
// safe for concurrent use. A slow observer throttles cache access.
type GlyphCacheObserver interface {
	// OnOutlineAccess reports a lookup. hit is true when key resolved.
	OnOutlineAccess(key OutlineCacheKey, hit bool)

	// OnOutlineInsert reports that a new outline was stored under key.
	OnOutlineInsert(key OutlineCacheKey)
}

// GlyphCacheConfig holds configuration for GlyphCache.
type GlyphCacheConfig struct {
	// MaxEntries is the maximum number of cached glyph outlines.
	// Default: 4096
	MaxEntries int

	// FrameLifetime is the number of frames an entry can be unused
	// before being eligible for eviction during Maintain().
	// Default: 64
	FrameLifetime int

	// Observer, when non-nil, receives hit/miss and insertion events.
	// Default: nil
	Observer GlyphCacheObserver
}

// DefaultGlyphCacheConfig returns the default cache configuration.
func DefaultGlyphCacheConfig() GlyphCacheConfig {
	return GlyphCacheConfig{
		MaxEntries:    4096,
		FrameLifetime: 64,
	}
}

// OutlineCacheKey uniquely identifies a cached glyph outline.
type OutlineCacheKey struct {
	// FontID is a unique identifier for the font.
	//
	// FontID is derived from stable font metadata (full name and glyph count),
	// so two independently loaded versions of the same font file intentionally
	// share a FontID. It must therefore never be the only cache discriminator
	// for a font that can be hot-reloaded.
	FontID uint64

	// SourceGeneration identifies the FontSource instance that produced the
	// outline. Every FontSource created by NewFontSource receives a
	// process-unique generation, so reloading a font from the same path yields
	// a new generation even when FontID is unchanged. This prevents a fresh
	// source from observing outlines cached for a closed predecessor.
	//
	// Zero is reserved for keys that are not associated with a FontSource
	// (for example legacy callers that only have a ParsedFont).
	SourceGeneration uint64

	// GID is the glyph index within the font.
	GID GlyphID

	// Size is the font size in ppem (pixels per em).
	// We use int16 for efficiency; sizes above 32K are rare.
	Size int16

	// Hinting indicates the hinting mode used.
	Hinting Hinting

	// VariationHash distinguishes cache entries for different font variations.
	// Zero means no variations (static font or default instance).
	// Computed via [VariationHash].
	VariationHash uint64
}

// VariationHash computes an FNV-1a hash of font variation settings.
// Returns 0 for nil or empty variations (no extra cost for static fonts).
// Different variation coordinates produce different glyph outlines,
// so they must be cached separately.
func VariationHash(vars []FontVariation) uint64 {
	if len(vars) == 0 {
		return 0
	}
	h := fnv.New64a()
	for _, v := range vars {
		_, _ = h.Write(v.Tag[:])
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], math.Float32bits(v.Value))
		_, _ = h.Write(buf[:])
	}
	return h.Sum64()
}

// glyphEntry is an internal cache entry.
type glyphEntry struct {
	key     OutlineCacheKey
	outline *GlyphOutline

	// prev and next for LRU doubly-linked list
	prev *glyphEntry
	next *glyphEntry

	// lastAccessFrame is the frame number when this entry was last accessed.
	// Used for frame-based eviction during Maintain().
	lastAccessFrame uint64
}

// GlyphCache is a thread-safe LRU cache for glyph outlines.
// It provides fast lookups with automatic eviction of least recently used entries.
//
// The cache is sharded to reduce lock contention in concurrent access patterns.
// It supports both capacity-based eviction (when MaxEntries is reached) and
// frame-based eviction (during Maintain() calls).
//
// GlyphCache is safe for concurrent use.
type GlyphCache struct {
	// shards hold the actual cache entries
	shards [numShards]*glyphShard

	// config holds cache configuration
	config GlyphCacheConfig

	// observer receives lifecycle events (may be nil)
	observer GlyphCacheObserver

	// currentFrame is the current frame counter for frame-based eviction
	currentFrame atomic.Uint64

	// stats holds cache statistics
	stats GlyphCacheStats
}

// numShards is the number of cache shards for reduced lock contention.
const numShards = 16

// glyphShard is a single shard of the glyph cache.
type glyphShard struct {
	mu sync.RWMutex

	// entries maps OutlineCacheKey to cache entry
	entries map[OutlineCacheKey]*glyphEntry

	// head is the most recently used entry
	head *glyphEntry

	// tail is the least recently used entry
	tail *glyphEntry

	// maxEntries is the max entries for this shard
	maxEntries int

	// count is the current number of entries
	count int
}

// GlyphCacheStats holds cache statistics.
type GlyphCacheStats struct {
	Hits       atomic.Uint64
	Misses     atomic.Uint64
	Evictions  atomic.Uint64
	Insertions atomic.Uint64
}

// NewGlyphCache creates a new glyph cache with default configuration.
func NewGlyphCache() *GlyphCache {
	return NewGlyphCacheWithConfig(DefaultGlyphCacheConfig())
}

// NewGlyphCacheWithConfig creates a new glyph cache with the given configuration.
func NewGlyphCacheWithConfig(config GlyphCacheConfig) *GlyphCache {
	if config.MaxEntries <= 0 {
		config.MaxEntries = 4096
	}
	if config.FrameLifetime <= 0 {
		config.FrameLifetime = 64
	}

	c := &GlyphCache{
		config:   config,
		observer: config.Observer,
	}

	// Divide entries among shards
	entriesPerShard := (config.MaxEntries + numShards - 1) / numShards

	for i := 0; i < numShards; i++ {
		c.shards[i] = &glyphShard{
			entries:    make(map[OutlineCacheKey]*glyphEntry, entriesPerShard),
			maxEntries: entriesPerShard,
		}
	}

	return c
}

// Get retrieves a cached glyph outline.
// Returns nil if not found.
func (c *GlyphCache) Get(key OutlineCacheKey) *GlyphOutline {
	shard := c.getShard(key)
	frame := c.currentFrame.Load()

	shard.mu.Lock()
	entry, ok := shard.entries[key]
	if !ok {
		shard.mu.Unlock()
		c.stats.Misses.Add(1)
		if c.observer != nil {
			c.observer.OnOutlineAccess(key, false)
		}
		return nil
	}

	// Update access time and move to front
	entry.lastAccessFrame = frame
	shard.moveToFront(entry)
	outline := entry.outline
	shard.mu.Unlock()

	c.stats.Hits.Add(1)
	if c.observer != nil {
		c.observer.OnOutlineAccess(key, true)
	}
	return outline
}

// Set stores a glyph outline in the cache.
// If the cache is full, the least recently used entry is evicted.
func (c *GlyphCache) Set(key OutlineCacheKey, outline *GlyphOutline) {
	if outline == nil {
		return
	}
	// Keys that carry neither a font identity nor a generation would alias
	// every font; that is always a call-site bug, and silently accepting it is
	// exactly how hot-reload leaks happen. Reject it at the boundary.
	if ValidateKey(key) != nil {
		return
	}

	shard := c.getShard(key)
	frame := c.currentFrame.Load()

	shard.mu.Lock()
	// Check if already exists
	if existing, ok := shard.entries[key]; ok {
		existing.outline = outline
		existing.lastAccessFrame = frame
		shard.moveToFront(existing)
		shard.mu.Unlock()
		c.stats.Insertions.Add(1)
		if c.observer != nil {
			c.observer.OnOutlineInsert(key)
		}
		return
	}

	// Create new entry
	entry := &glyphEntry{
		key:             key,
		outline:         outline,
		lastAccessFrame: frame,
	}

	// Evict if necessary
	for shard.count >= shard.maxEntries && shard.tail != nil {
		shard.removeTail()
		c.stats.Evictions.Add(1)
	}

	// Add to cache
	shard.entries[key] = entry
	shard.addToFront(entry)
	shard.count++
	shard.mu.Unlock()

	c.stats.Insertions.Add(1)
	if c.observer != nil {
		c.observer.OnOutlineInsert(key)
	}
}

// GetOrCreate retrieves a cached outline or creates one using the provided function.
// This is an atomic operation that avoids redundant creation.
func (c *GlyphCache) GetOrCreate(key OutlineCacheKey, create func() *GlyphOutline) *GlyphOutline {
	// First try a fast read-only lookup
	shard := c.getShard(key)
	frame := c.currentFrame.Load()

	shard.mu.RLock()
	if _, ok := shard.entries[key]; ok {
		shard.mu.RUnlock()
		// Upgrade to write lock to update access time
		shard.mu.Lock()
		if entry, ok := shard.entries[key]; ok {
			entry.lastAccessFrame = frame
			shard.moveToFront(entry)
			outline := entry.outline
			shard.mu.Unlock()
			c.stats.Hits.Add(1)
			if c.observer != nil {
				c.observer.OnOutlineAccess(key, true)
			}
			return outline
		}
		shard.mu.Unlock()
	} else {
		shard.mu.RUnlock()
	}

	// Need to create
	if create == nil {
		c.stats.Misses.Add(1)
		if c.observer != nil {
			c.observer.OnOutlineAccess(key, false)
		}
		return nil
	}

	outline := create()
	if outline != nil {
		c.Set(key, outline)
	} else {
		c.stats.Misses.Add(1)
		if c.observer != nil {
			c.observer.OnOutlineAccess(key, false)
		}
	}
	return outline
}

// DeleteSourceGeneration removes every entry produced by the given
// FontSource generation. It returns the number of entries removed.
//
// Call this when a FontSource is closed (see FontSource.Close) so outlines
// built from released font data are not retained by the cache. It is safe to
// call concurrently with Get/Set/GetOrCreate/Clear; a concurrent insert of the
// same generation may survive (the caller races with a Close, which is
// inherently undefined), but no entry from another generation is touched.
func (c *GlyphCache) DeleteSourceGeneration(generation uint64) int {
	if generation == 0 {
		return 0
	}

	removed := 0
	for i := 0; i < numShards; i++ {
		shard := c.shards[i]
		shard.mu.Lock()
		for key, entry := range shard.entries {
			if key.SourceGeneration != generation {
				continue
			}
			shard.remove(entry)
			delete(shard.entries, key)
			shard.count--
			removed++
		}
		shard.mu.Unlock()
	}
	return removed
}

// Delete removes an entry from the cache.
func (c *GlyphCache) Delete(key OutlineCacheKey) {
	shard := c.getShard(key)

	shard.mu.Lock()
	defer shard.mu.Unlock()

	entry, ok := shard.entries[key]
	if !ok {
		return
	}

	shard.remove(entry)
	delete(shard.entries, key)
	shard.count--
}

// Clear removes all entries from the cache.
//
// Concurrency semantics: Clear takes each shard lock in turn and swaps the
// shard map. A Get/GetOrCreate that completes before the corresponding shard
// is cleared may still return an entry; a Set/GetOrCreate that completes after
// may insert an entry that survives. Outline values already returned to a
// caller remain valid — Clear only drops the cache's references. Callers that
// need a strict "no entry observed after this point" guarantee must
// synchronize with concurrent readers themselves.
func (c *GlyphCache) Clear() {
	for i := 0; i < numShards; i++ {
		shard := c.shards[i]
		shard.mu.Lock()
		shard.entries = make(map[OutlineCacheKey]*glyphEntry, shard.maxEntries)
		shard.head = nil
		shard.tail = nil
		shard.count = 0
		shard.mu.Unlock()
	}
}

// Maintain performs periodic maintenance on the cache.
// It evicts entries that haven't been accessed for FrameLifetime frames.
// Call this once per frame for frame-based eviction.
func (c *GlyphCache) Maintain() {
	frame := c.currentFrame.Add(1)
	// FrameLifetime is validated to be positive in NewGlyphCacheWithConfig
	frameLifetime := max(c.config.FrameLifetime, 1)

	// Avoid underflow when frame < FrameLifetime
	// frameLifetime is guaranteed >= 1 so conversion is safe
	frameLifetimeU64 := uint64(frameLifetime) //nolint:gosec // validated >= 1 above
	if frame < frameLifetimeU64 {
		return
	}
	threshold := frame - frameLifetimeU64

	for i := 0; i < numShards; i++ {
		shard := c.shards[i]
		shard.mu.Lock()

		// Walk from tail (oldest) and evict stale entries
		entry := shard.tail
		for entry != nil && entry.lastAccessFrame < threshold {
			prev := entry.prev
			delete(shard.entries, entry.key)
			shard.remove(entry)
			shard.count--
			c.stats.Evictions.Add(1)
			entry = prev
		}

		shard.mu.Unlock()
	}
}

// Len returns the total number of cached entries.
func (c *GlyphCache) Len() int {
	total := 0
	for i := 0; i < numShards; i++ {
		shard := c.shards[i]
		shard.mu.RLock()
		total += shard.count
		shard.mu.RUnlock()
	}
	return total
}

// ValidateKey rejects cache keys that omit the source generation for a font
// that can be hot-reloaded. It is a development/CI aid: checks that every key
// inserted during a font lifecycle run carries a generation (or is a
// documented bare-ParsedFont key with FontID != 0).
//
// The rule enforced here is: a key must not combine a zero generation with a
// zero FontID, because that is the shape produced by a buggy call site that
// forgot both discriminators and would alias every font.
func ValidateKey(key OutlineCacheKey) error {
	if key.FontID == 0 && key.SourceGeneration == 0 {
		return fmt.Errorf("text: outline cache key has neither FontID nor SourceGeneration (gid=%d size=%d)", key.GID, key.Size)
	}
	return nil
}

// Stats returns cache statistics.
func (c *GlyphCache) Stats() (hits, misses, evictions, insertions uint64) {
	return c.stats.Hits.Load(),
		c.stats.Misses.Load(),
		c.stats.Evictions.Load(),
		c.stats.Insertions.Load()
}

// HitRate returns the cache hit rate as a percentage.
// Returns 0 if there are no accesses.
func (c *GlyphCache) HitRate() float64 {
	hits := c.stats.Hits.Load()
	misses := c.stats.Misses.Load()
	total := hits + misses
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total) * 100
}

// ResetStats resets the cache statistics.
func (c *GlyphCache) ResetStats() {
	c.stats.Hits.Store(0)
	c.stats.Misses.Store(0)
	c.stats.Evictions.Store(0)
	c.stats.Insertions.Store(0)
}

// CurrentFrame returns the current frame number.
func (c *GlyphCache) CurrentFrame() uint64 {
	return c.currentFrame.Load()
}

// getShard returns the shard for the given key.
func (c *GlyphCache) getShard(key OutlineCacheKey) *glyphShard {
	// Hash the key to distribute across shards
	h := key.FontID
	h = h*31 + key.SourceGeneration
	h = h*31 + uint64(key.GID)
	// Size can be negative but we only use it for hashing, so cast is safe
	h = h*31 + uint64(int64(key.Size)) //#nosec G115 -- hash only, value can be negative
	h = h*31 + uint64(key.Hinting)     //#nosec G115 -- Hinting is a small enum value
	h = h*31 + key.VariationHash
	return c.shards[h%numShards]
}

// addToFront adds an entry to the front of the LRU list.
func (s *glyphShard) addToFront(entry *glyphEntry) {
	entry.prev = nil
	entry.next = s.head

	if s.head != nil {
		s.head.prev = entry
	}
	s.head = entry

	if s.tail == nil {
		s.tail = entry
	}
}

// moveToFront moves an entry to the front of the LRU list.
func (s *glyphShard) moveToFront(entry *glyphEntry) {
	if entry == s.head {
		return
	}

	s.remove(entry)
	s.addToFront(entry)
}

// remove removes an entry from the LRU list (does not delete from map).
func (s *glyphShard) remove(entry *glyphEntry) {
	if entry == nil {
		return
	}

	if entry.prev != nil {
		entry.prev.next = entry.next
	} else {
		s.head = entry.next
	}

	if entry.next != nil {
		entry.next.prev = entry.prev
	} else {
		s.tail = entry.prev
	}

	entry.prev = nil
	entry.next = nil
}

// removeTail removes and returns the tail entry.
func (s *glyphShard) removeTail() *glyphEntry {
	if s.tail == nil {
		return nil
	}

	entry := s.tail
	delete(s.entries, entry.key)
	s.remove(entry)
	s.count--
	return entry
}

// GlyphCachePool manages a pool of GlyphCaches for per-thread usage.
// This can further reduce contention in highly concurrent scenarios.
type GlyphCachePool struct {
	pool sync.Pool
}

// NewGlyphCachePool creates a new pool of glyph caches.
func NewGlyphCachePool() *GlyphCachePool {
	return &GlyphCachePool{
		pool: sync.Pool{
			New: func() any {
				return NewGlyphCache()
			},
		},
	}
}

// Get retrieves a cache from the pool.
func (p *GlyphCachePool) Get() *GlyphCache {
	return p.pool.Get().(*GlyphCache)
}

// Put returns a cache to the pool.
func (p *GlyphCachePool) Put(c *GlyphCache) {
	if c != nil {
		c.Clear()
		p.pool.Put(c)
	}
}

// globalGlyphCache is the default shared glyph cache.
var globalGlyphCache atomic.Pointer[GlyphCache]

func init() {
	globalGlyphCache.Store(NewGlyphCache())
}

// GetGlobalGlyphCache returns the global shared glyph cache.
// It is safe to call concurrently with SetGlobalGlyphCache.
//
// GetGlobalGlyphCache never returns nil: the initial cache is installed at
// package init and SetGlobalGlyphCache(nil) installs a fresh one.
func GetGlobalGlyphCache() *GlyphCache {
	return globalGlyphCache.Load()
}

// SetGlobalGlyphCache replaces the global glyph cache and returns the
// previous cache. If cache is nil a fresh default cache is installed.
// It is safe for concurrent use.
//
// Concurrency semantics: the swap is atomic, but the returned cache may still
// be in use by goroutines that loaded it before the swap. Do not call Clear()
// on the returned cache while those goroutines may be running — dispose of it
// only once you know no caller holds it. Caches are not reference counted.
func SetGlobalGlyphCache(cache *GlyphCache) *GlyphCache {
	if cache == nil {
		cache = NewGlyphCache()
	}
	return globalGlyphCache.Swap(cache)
}

// ClearGlobalGlyphCache clears the current global glyph cache in place and
// returns it. This is the supported way to invalidate every globally cached
// outline (e.g. after a font hot-reload) without invalidating pointers that
// renderers already hold.
//
// It is safe for concurrent use; see [GlyphCache.Clear] for the exact
// race semantics.
func ClearGlobalGlyphCache() *GlyphCache {
	cache := GetGlobalGlyphCache()
	cache.Clear()
	return cache
}
