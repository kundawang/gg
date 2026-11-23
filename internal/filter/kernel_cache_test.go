package filter

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/scene"
)

// ---------------------------------------------------------------------------
// Basic caching behavior and stats
// ---------------------------------------------------------------------------

func TestKernelCacheHitMissAndGeneration(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(8))

	k1 := c.get(5.0)
	s := c.Stats()
	if s.Misses != 1 || s.Generations != 1 || s.Hits != 0 || s.Entries != 1 {
		t.Fatalf("after first get: %+v", s)
	}

	k2 := c.get(5.0)
	s = c.Stats()
	if s.Hits != 1 || s.Misses != 1 || s.Generations != 1 || s.Entries != 1 {
		t.Fatalf("after second get: %+v", s)
	}

	assertKernelsEqual(t, k1, k2)
	if &k1[0] == &k2[0] {
		t.Fatal("cache must return private copies, got shared backing array")
	}
}

func TestKernelCacheReturnedSliceIsImmutable(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(4))
	want := GaussianKernel(5.0)

	k := c.get(5.0)
	k[0] = 999 // caller mutation must not leak into the cache

	again := c.get(5.0)
	if again[0] == 999 {
		t.Fatal("mutation of returned kernel leaked into cached entry")
	}
	assertKernelsEqual(t, again, want)
}

func TestKernelCacheEvictionKeepsInUseSliceValid(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(1))
	want := GaussianKernel(5.0)

	inUse := c.get(5.0)

	// Different radius evicts the entry backing inUse.
	_ = c.get(6.0)
	if c.Stats().Entries != 1 || c.Stats().Evictions != 1 {
		t.Fatalf("unexpected stats: %+v", c.Stats())
	}

	// The caller's slice must survive eviction with original contents.
	assertKernelsEqual(t, inUse, want)
}

func TestKernelCacheLRUOrder(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(3))

	_ = c.get(1)
	_ = c.get(2)
	_ = c.get(3)
	// Touch 1 so recency order becomes 2,3,1 (oldest first).
	_ = c.get(1)
	// Inserting 4 must evict 2, the least-recently-used entry.
	_ = c.get(4)

	if c.Stats().Evictions != 1 {
		t.Fatalf("Evictions = %d, want 1", c.Stats().Evictions)
	}
	if s := c.Stats(); s.Entries != 3 {
		t.Fatalf("Entries = %d, want 3", s.Entries)
	}
	if got := c.get(2); len(got) != len(GaussianKernel(2)) {
		t.Fatal("evicted radius 2 should be regenerated")
	}
	// 1, 3, 4 remain cached.
	if c.Stats().Hits != 1 {
		t.Fatalf("Hits = %d, want 1 (only radius 1 reuse)", c.Stats().Hits)
	}
}

func TestKernelCacheQuantizationBuckets(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(8))

	_ = c.get(5.0)
	// round(5.004 * 100) = 500 -> same bucket, cache hit.
	_ = c.get(5.004)
	if c.Stats().Hits != 1 {
		t.Fatalf("Hits = %d, want 1 for same 0.01 bucket", c.Stats().Hits)
	}

	// round(5.005 * 100) = 501 -> new bucket, miss.
	_ = c.get(5.005)
	if c.Stats().Misses != 2 || c.Stats().Entries != 2 {
		t.Fatalf("unexpected stats: %+v", c.Stats())
	}

	if got := quantizeRadius(5.0); got != 500 {
		t.Fatalf("quantizeRadius(5) = %d, want 500", got)
	}
}

// ---------------------------------------------------------------------------
// Boundary radii
// ---------------------------------------------------------------------------

func TestKernelCacheIdentityRadii(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(4))
	cases := map[string]float64{
		"zero":     0,
		"negative": -7,
		"nan":      math.NaN(),
		"posInf":   math.Inf(1),
		"negInf":   math.Inf(-1),
	}

	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			k := c.get(r)
			if len(k) != 1 || k[0] != 1 {
				t.Fatalf("get(%s) = %v, want [1]", name, k)
			}
			// Identity results are not cached and move no counters.
			if s := c.Stats(); s.Entries != 0 || s.Misses != 0 || s.Hits != 0 {
				t.Fatalf("identity radius must not touch cache state: %+v", s)
			}
		})
	}
}

func TestGaussianKernelNonFiniteAndHugeInput(t *testing.T) {
	for _, r := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0} {
		k := GaussianKernel(r)
		if len(k) != 1 || k[0] != 1 {
			t.Fatalf("GaussianKernel(%v) = %v, want [1]", r, k)
		}
	}

	// Huge finite sigma is clamped: bounded size, deterministic result, and
	// identical to an explicit sigma at the clamp.
	huge := GaussianKernel(1e9)
	clamped := GaussianKernel(maxGaussianSigma)
	maxLen := 2*int(math.Ceil(maxGaussianSigma*3)) + 1
	if len(huge) != maxLen {
		t.Fatalf("huge sigma kernel len = %d, want %d", len(huge), maxLen)
	}
	assertKernelsEqual(t, huge, clamped)
}

func TestKernelCacheHugeSigmaSharesOneEntry(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(4))

	k1 := c.get(1e7)
	k2 := c.get(1e8) // both clamp to maxGaussianSigma -> same key
	if c.Stats().Entries != 1 {
		t.Fatalf("Entries = %d, want 1 for clamped huge sigma", c.Stats().Entries)
	}
	if c.Stats().Hits != 1 {
		t.Fatalf("Hits = %d, want 1", c.Stats().Hits)
	}
	assertKernelsEqual(t, k1, k2)
}

// ---------------------------------------------------------------------------
// Zero capacity and Clear
// ---------------------------------------------------------------------------

func TestKernelCacheZeroCapacityIsPassThrough(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(0))
	if c.Stats().Capacity != 0 {
		t.Fatalf("Capacity = %d, want 0", c.Stats().Capacity)
	}

	k1 := c.get(5)
	k2 := c.get(5)
	if &k1[0] == &k2[0] {
		t.Fatal("pass-through cache must return freshly generated slices")
	}
	assertKernelsEqual(t, k1, GaussianKernel(5))

	s := c.Stats()
	if s.Entries != 0 || s.Bytes != 0 || s.Hits != 0 || s.Misses != 0 || s.Generations != 0 {
		t.Fatalf("pass-through cache must retain and count nothing: %+v", s)
	}

	// Clear must remain defined on a pass-through cache.
	c.Clear()
	if c.Stats().Clears != 1 {
		t.Fatalf("Clears = %d, want 1", c.Stats().Clears)
	}
}

func TestKernelCacheNegativeCapacityTreatedAsZero(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(-3))
	if c.Stats().Capacity != 0 {
		t.Fatalf("Capacity = %d, want 0", c.Stats().Capacity)
	}
}

func TestKernelCacheClearIsReentrantAndSafe(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(4))
	_ = c.get(1)
	_ = c.get(2)
	before := c.Stats()
	if before.Entries != 2 || before.Bytes == 0 {
		t.Fatalf("unexpected pre-clear stats: %+v", before)
	}

	c.Clear()
	c.Clear() // repeated clear on an empty cache must be a defined no-op

	s := c.Stats()
	if s.Entries != 0 || s.Bytes != 0 || s.Clears != 2 {
		t.Fatalf("after repeated clear: %+v", s)
	}

	// Cache remains fully functional after clearing.
	k := c.get(1)
	assertKernelsEqual(t, k, GaussianKernel(1))
	if c.Stats().Generations != before.Generations+1 {
		t.Fatal("generation after clear not recorded")
	}
}

// ---------------------------------------------------------------------------
// Observability
// ---------------------------------------------------------------------------

type recordingObserver struct {
	mu     sync.Mutex
	events []CacheEvent
}

func (o *recordingObserver) OnKernelCacheEvent(e CacheEvent) {
	o.mu.Lock()
	o.events = append(o.events, e)
	o.mu.Unlock()
}

func (o *recordingObserver) snapshot() []CacheEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]CacheEvent, len(o.events))
	copy(out, o.events)
	return out
}

func TestKernelCacheObserverEvents(t *testing.T) {
	obs := &recordingObserver{}
	c := newKernelCache(
		WithKernelCacheCapacity(2),
		WithKernelCacheObserver(obs),
	)

	_ = c.get(5) // miss + generated
	_ = c.get(5) // hit
	_ = c.get(6)
	_ = c.get(7) // evicts radius 5, then generated
	c.Clear()    // cleared, reports remaining entries/bytes

	events := obs.snapshot()
	wantTypes := []CacheEventType{
		CacheEventMiss, CacheEventGenerated,
		CacheEventHit,
		CacheEventMiss, CacheEventGenerated,
		CacheEventMiss, CacheEventEvicted, CacheEventGenerated,
		CacheEventCleared,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events %v, want %d", len(events), events, len(wantTypes))
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("event %d = %d, want %d (all: %v)", i, events[i].Type, want, events)
		}
	}

	hit := events[2]
	if hit.Quantized != quantizeRadius(5) || hit.Version != gaussianKernelVersion ||
		hit.KernelLen != len(GaussianKernel(5)) {
		t.Fatalf("hit event fields wrong: %+v", hit)
	}

	evicted := events[6]
	if evicted.Quantized != quantizeRadius(5) || evicted.KernelLen != len(GaussianKernel(5)) {
		t.Fatalf("eviction event fields wrong: %+v", evicted)
	}

	cleared := events[8]
	if cleared.Entries != 2 || cleared.Bytes <= 0 {
		t.Fatalf("clear event fields wrong: %+v", cleared)
	}
}

func TestKernelCacheByteAccounting(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(4))
	_ = c.get(1)
	_ = c.get(2)

	want := int64(0)
	for _, r := range []float64{1, 2} {
		want += kernelBytes(len(GaussianKernel(r)))
	}
	if c.Stats().Bytes != want {
		t.Fatalf("Bytes = %d, want %d", c.Stats().Bytes, want)
	}

	c.Clear()
	if c.Stats().Bytes != 0 {
		t.Fatalf("Bytes after clear = %d, want 0", c.Stats().Bytes)
	}
}

// ---------------------------------------------------------------------------
// Singleflight: same radius concurrently generates once
// ---------------------------------------------------------------------------

func TestKernelCacheConcurrentSameRadiusGeneratesOnce(t *testing.T) {
	var genCount int64
	block := make(chan struct{})
	c := newKernelCache(
		WithKernelCacheCapacity(4),
		withKernelGenerator(func(radius float64) []float32 {
			atomic.AddInt64(&genCount, 1)
			<-block
			return GaussianKernel(radius)
		}),
	)

	const waiters = 16
	var wg sync.WaitGroup
	results := make([][]float32, waiters)
	start := make(chan struct{})
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx] = c.get(5)
		}(i)
	}
	close(start)

	waitFor(t, func() bool {
		s := c.Stats()
		return s.Misses == waiters
	}, "all goroutines register as misses")
	close(block)
	wg.Wait()

	if atomic.LoadInt64(&genCount) != 1 {
		t.Fatalf("generator ran %d times, want 1", genCount)
	}
	s := c.Stats()
	if s.Generations != 1 || s.Entries != 1 {
		t.Fatalf("unexpected stats: %+v", s)
	}
	want := GaussianKernel(5)
	for i, k := range results {
		assertKernelsEqual(t, k, want)
		if i > 0 && &results[i-1][0] == &k[0] {
			t.Fatal("waiters must each receive a private copy")
		}
	}
}

func TestKernelCacheGeneratorPanicReleasesWaitersAndRecovers(t *testing.T) {
	var attempts int64
	c := newKernelCache(
		WithKernelCacheCapacity(4),
		withKernelGenerator(func(radius float64) []float32 {
			if atomic.AddInt64(&attempts, 1) == 1 {
				panic("injected failure")
			}
			return GaussianKernel(radius)
		}),
	)

	assertPanic(t, func() { c.get(3) }, "injected failure")

	// The in-flight slot must be cleaned: the retry succeeds.
	k := c.get(3)
	assertKernelsEqual(t, k, GaussianKernel(3))
	s := c.Stats()
	if s.Entries != 1 || s.Generations != 1 {
		t.Fatalf("cache did not recover after panic: %+v", s)
	}
}

func TestKernelCachePanicPropagatesToAllWaiters(t *testing.T) {
	block := make(chan struct{})
	var genCount int64
	c := newKernelCache(
		WithKernelCacheCapacity(4),
		withKernelGenerator(func(radius float64) []float32 {
			atomic.AddInt64(&genCount, 1)
			<-block
			panic("injected failure")
		}),
	)

	const waiters = 8
	var wg sync.WaitGroup
	panicCount := int64(0)
	start := make(chan struct{})
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			defer func() {
				if recover() != nil {
					atomic.AddInt64(&panicCount, 1)
				}
			}()
			_ = c.get(4)
		}()
	}
	close(start)
	waitFor(t, func() bool { return c.Stats().Misses == waiters }, "waiters join")
	close(block)
	wg.Wait()

	if atomic.LoadInt64(&panicCount) != waiters {
		t.Fatalf("panicCount = %d, want %d", panicCount, waiters)
	}
	if atomic.LoadInt64(&genCount) != 1 {
		t.Fatalf("generator ran %d times, want 1", genCount)
	}
	if c.Stats().Entries != 0 || c.Stats().Generations != 0 {
		t.Fatalf("failed generation must not be cached: %+v", c.Stats())
	}
}

// ---------------------------------------------------------------------------
// Mixed concurrent workload under -race
// ---------------------------------------------------------------------------

func TestKernelCacheConcurrentAccessAndClear(t *testing.T) {
	c := newKernelCache(WithKernelCacheCapacity(8))
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < runtime.NumCPU()*2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				r := float64((j + id) % 16)
				k := c.get(r)
				if len(k) == 0 {
					t.Errorf("empty kernel for radius %v", r)
					return
				}
			}
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c.Clear()
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = c.Stats()
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Custom caches do not change default GaussianKernel results
// ---------------------------------------------------------------------------

func TestCustomCacheDoesNotAffectDefaultKernel(t *testing.T) {
	// A tiny, separately configured cache must not influence the
	// package-level cache backing CachedGaussianKernel.
	_ = newKernelCache(
		WithKernelCacheCapacity(1),
		WithKernelCacheMaxSigma(2),
		WithKernelCacheObserver(CacheObserverFunc(func(CacheEvent) {})),
	)

	for _, r := range []float64{0.5, 1, 2, 5, 20, 100} {
		cached := CachedGaussianKernel(r)
		uncached := GaussianKernel(r)
		assertKernelsEqual(t, cached, uncached)
	}
}

// ---------------------------------------------------------------------------
// Filter pixel parity: cached path vs. uncached reference convolution
// ---------------------------------------------------------------------------

func TestBlurFilterCachedAndUncachedProduceSamePixels(t *testing.T) {
	radii := []float64{0, -1, math.NaN(), math.Inf(1), 0.5, 1.0, 2.0, 5.0, 20.0, 1e6}

	for _, r := range radii {
		t.Run(radiusCaseName(r), func(t *testing.T) {
			src := makePatternPixmap(24, 18)
			bounds := scene.Rect{MinX: 0, MinY: 0, MaxX: 24, MaxY: 18}

			cachedDst := gg.NewPixmap(24, 18)
			NewBlurFilter(r).Apply(src, cachedDst, bounds)

			refDst := gg.NewPixmap(24, 18)
			referenceBlur(src, refDst, bounds, r, r)

			assertPixmapsEqual(t, cachedDst, refDst, 1)
		})
	}
}

func TestBlurFilterAnisotropicCachedPixelsMatchReference(t *testing.T) {
	src := makePatternPixmap(24, 18)
	bounds := scene.Rect{MinX: 0, MinY: 0, MaxX: 24, MaxY: 18}

	cachedDst := gg.NewPixmap(24, 18)
	NewBlurFilterXY(7, 2).Apply(src, cachedDst, bounds)

	refDst := gg.NewPixmap(24, 18)
	referenceBlur(src, refDst, bounds, 7, 2)

	assertPixmapsEqual(t, cachedDst, refDst, 1)
}

func TestBlurFilterConcurrentApplicationsAreStable(t *testing.T) {
	src := makePatternPixmap(32, 24)
	bounds := scene.Rect{MinX: 0, MinY: 0, MaxX: 32, MaxY: 24}
	f := NewBlurFilter(5)

	base := gg.NewPixmap(32, 24)
	f.Apply(src, base, bounds)

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dst := gg.NewPixmap(32, 24)
			f.Apply(src, dst, bounds)
			if err := pixmapsDiff(base, dst, 1); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkKernelCacheGet(b *testing.B) {
	c := newKernelCache(WithKernelCacheCapacity(4))
	_ = c.get(10) // warm

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.get(10)
	}
}

func BenchmarkKernelCacheGetParallel(b *testing.B) {
	c := newKernelCache(WithKernelCacheCapacity(16))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = c.get(10)
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func assertKernelsEqual(t *testing.T, got, want []float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("kernel len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("kernel[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func assertPanic(t *testing.T, fn func(), wantSubstring string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic, got none")
		}
		if msg, ok := r.(string); !ok || msg != wantSubstring {
			t.Fatalf("panic = %v, want %q", r, wantSubstring)
		}
	}()
	fn()
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func radiusCaseName(r float64) string {
	switch {
	case math.IsNaN(r):
		return "NaN"
	case math.IsInf(r, 1):
		return "+Inf"
	case math.IsInf(r, -1):
		return "-Inf"
	case r == 0:
		return "zero"
	case r < 0:
		return "negative"
	case r > maxGaussianSigma:
		return "huge"
	default:
		return formatFloat(r)
	}
}

// makePatternPixmap builds a deterministic non-uniform image so blur output
// varies across pixels.
func makePatternPixmap(w, h int) *gg.Pixmap {
	p := gg.NewPixmap(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p.SetPixel(x, y, gg.RGBA2(
				float64((x*7)%256)/255,
				float64((y*11)%256)/255,
				float64((x*y)%256)/255,
				float64(64+(x+y)*4)/255,
			))
		}
	}
	return p
}

// referenceBlur mirrors blurHorizontal/blurVertical using the uncached
// GaussianKernel directly, so pixel differences would expose cache
// corruption or kernel divergence.
func referenceBlur(src, dst *gg.Pixmap, bounds scene.Rect, rx, ry float64) {
	minX := clampInt(int(bounds.MinX), 0, src.Width())
	maxX := clampInt(int(bounds.MaxX), 0, src.Width())
	minY := clampInt(int(bounds.MinY), 0, src.Height())
	maxY := clampInt(int(bounds.MaxY), 0, src.Height())

	width := maxX - minX
	height := maxY - minY
	temp := make([]float32, width*height*4)

	kernelX := GaussianKernel(rx)
	kernelY := GaussianKernel(ry)

	for y := 0; y < height; y++ {
		srcY := minY + y
		for x := 0; x < width; x++ {
			srcX := minX + x
			var r, g, b, a float32
			if rx <= 0 || !isFinitePositive(rx) {
				idx := (srcY*src.Width() + srcX) * 4
				d := src.Data()
				r, g, b, a = float32(d[idx]), float32(d[idx+1]), float32(d[idx+2]), float32(d[idx+3])
			} else {
				for k := range kernelX {
					kx := srcX + k - len(kernelX)/2
					if kx < 0 {
						kx = 0
					} else if kx >= src.Width() {
						kx = src.Width() - 1
					}
					idx := (srcY*src.Width() + kx) * 4
					d := src.Data()
					w := kernelX[k]
					r += float32(d[idx]) * w
					g += float32(d[idx+1]) * w
					b += float32(d[idx+2]) * w
					a += float32(d[idx+3]) * w
				}
			}
			ti := (y*width + x) * 4
			temp[ti], temp[ti+1], temp[ti+2], temp[ti+3] = r, g, b, a
		}
	}

	for y := 0; y < height; y++ {
		dstY := minY + y
		for x := 0; x < width; x++ {
			dstX := minX + x
			var r, g, b, a float32
			if ry <= 0 || !isFinitePositive(ry) {
				ti := (y*width + x) * 4
				r, g, b, a = temp[ti], temp[ti+1], temp[ti+2], temp[ti+3]
			} else {
				for k := range kernelY {
					ky := y + k - len(kernelY)/2
					if ky < 0 {
						ky = 0
					} else if ky >= height {
						ky = height - 1
					}
					ti := (ky*width + x) * 4
					w := kernelY[k]
					r += temp[ti] * w
					g += temp[ti+1] * w
					b += temp[ti+2] * w
					a += temp[ti+3] * w
				}
			}
			idx := (dstY*dst.Width() + dstX) * 4
			d := dst.Data()
			d[idx] = clampUint8(r)
			d[idx+1] = clampUint8(g)
			d[idx+2] = clampUint8(b)
			d[idx+3] = clampUint8(a)
		}
	}
}

func assertPixmapsEqual(t *testing.T, got, want *gg.Pixmap, tolerance uint8) {
	t.Helper()
	if err := pixmapsDiff(got, want, tolerance); err != nil {
		t.Fatal(err)
	}
}

func pixmapsDiff(got, want *gg.Pixmap, tolerance uint8) error {
	if got.Width() != want.Width() || got.Height() != want.Height() {
		return fmt.Errorf("size mismatch: %dx%d vs %dx%d",
			got.Width(), got.Height(), want.Width(), want.Height())
	}
	g, w := got.Data(), want.Data()
	for i := range g {
		diff := int(g[i]) - int(w[i])
		if diff < 0 {
			diff = -diff
		}
		if diff > int(tolerance) {
			return fmt.Errorf("byte %d: got %d, want %d", i, g[i], w[i])
		}
	}
	return nil
}
