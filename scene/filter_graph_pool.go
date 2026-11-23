package scene

import (
	"sync"

	"github.com/gogpu/gg"
)

// filterGraphPool caches pixmaps between graph executions. It is deliberately
// bounded: a graph that allocates one huge frame must not pin that memory in
// the pool forever.
//
// The pool is shared by every Execute call on one CompiledFilterGraph, so all
// access is guarded by a mutex.
type filterGraphPool struct {
	mu sync.Mutex

	// free maps a buffer dimension key to its cached pixmaps.
	free map[uint64][]*gg.Pixmap

	// cachedBytes tracks how many bytes the pool currently retains.
	cachedBytes int64

	// maxCachedBytes is the retention ceiling. Zero means defaultPoolCacheLimit.
	maxCachedBytes int64
}

// defaultPoolCacheLimit bounds retained pool memory.
const defaultPoolCacheLimit int64 = 64 << 20 // 64 MiB

func newFilterGraphPool() *filterGraphPool {
	return &filterGraphPool{
		free:           make(map[uint64][]*gg.Pixmap),
		maxCachedBytes: defaultPoolCacheLimit,
	}
}

// key packs width and height into one map key.
func filterPoolKey(w, h int) uint64 {
	return uint64(uint32(w))<<32 | uint64(uint32(h))
}

// get returns a cleared pixmap of exactly w x h, reusing a cached one when
// available.
func (p *filterGraphPool) get(w, h int) *gg.Pixmap {
	if p == nil {
		return gg.NewPixmap(w, h)
	}
	key := filterPoolKey(w, h)
	p.mu.Lock()
	if list := p.free[key]; len(list) > 0 {
		pm := list[len(list)-1]
		p.free[key] = list[:len(list)-1]
		p.cachedBytes -= int64(pm.Width()) * int64(pm.Height()) * 4
		p.mu.Unlock()
		pm.Clear(gg.Transparent)
		return pm
	}
	p.mu.Unlock()
	return gg.NewPixmap(w, h)
}

// put returns a pixmap to the pool, dropping it when the retention ceiling
// would be exceeded.
func (p *filterGraphPool) put(pm *gg.Pixmap) {
	if p == nil || pm == nil {
		return
	}
	bytes := int64(pm.Width()) * int64(pm.Height()) * 4
	if bytes <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cachedBytes+bytes > p.maxCachedBytes {
		return // let GC reclaim it
	}
	key := filterPoolKey(pm.Width(), pm.Height())
	p.free[key] = append(p.free[key], pm)
	p.cachedBytes += bytes
}
