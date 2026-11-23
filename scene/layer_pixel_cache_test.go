package scene

import (
	"context"
	"errors"
	"image"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gogpu/gg"
)

func makeKey(id uint32, scale float32) LayerKey {
	return LayerKey{
		ContentHash: 1000 + uint64(id),
		DeviceScale: QuantizeScale(scale),
		LayerID:     id,
		Alpha:       1,
	}
}

func TestLayerPixelCache_DisabledByZeroBudget(t *testing.T) {
	c := NewLayerPixelCache(0)
	if c.Enabled() {
		t.Fatal("zero budget must be disabled")
	}
	key := makeKey(0, 1)
	snap := c.Store(key, gg.NewPixmap(10, 10), 0, 0, Rect{})
	if snap == nil {
		t.Fatal("Store on disabled cache must still return a one-off snapshot")
	}
	if c.EntryCount() != 0 {
		t.Fatalf("disabled cache must not store entries, got %d", c.EntryCount())
	}
	if got := c.Lookup(key); got != nil {
		t.Fatal("disabled cache lookups must always miss")
	}
	st := c.Stats()
	if !st.Disabled || st.Misses != 1 {
		t.Fatalf("stats wrong: %+v", st)
	}
}

func TestLayerPixelCache_PutLookupImmutable(t *testing.T) {
	c := NewLayerPixelCache(1024 * 1024)
	pm := gg.NewPixmap(20, 10) // 800 bytes
	pm.FillRect(image.Rect(0, 0, 20, 10), 10, 20, 30, 255)
	key := makeKey(0, 1)

	snap := c.Store(key, pm, 5, 7, Rect{MinX: 5, MinY: 7, MaxX: 25, MaxY: 17})
	if snap == nil {
		t.Fatal("Store returned nil")
	}
	got := c.Lookup(key)
	if got == nil {
		t.Fatal("Lookup missed after Store")
	}
	if got.OriginX() != 5 || got.OriginY() != 7 {
		t.Fatalf("origin = (%d,%d), want (5,7)", got.OriginX(), got.OriginY())
	}
	// Bounds returned by value.
	b := got.Bounds()
	b.MinX = 999
	if got.Bounds().MinX != 5 {
		t.Fatal("Bounds must be a defensive copy")
	}
	// Pixels() must return a copy, not the internal slice.
	pixels := got.Pixels()
	pixels[0] = 255
	if got.Pixels()[0] == 255 {
		t.Fatal("Pixels() must copy; internal image is mutable")
	}
	st := c.Stats()
	if st.Hits != 1 || st.Rebuilds != 1 || st.Entries != 1 {
		t.Fatalf("stats wrong: %+v", st)
	}
}

func TestLayerPixelCache_DeviceScaleNotMixed(t *testing.T) {
	c := NewLayerPixelCache(1024 * 1024)
	base := makeKey(1, 1)
	scaled := base
	scaled.DeviceScale = QuantizeScale(2)

	c.Store(base, gg.NewPixmap(4, 4), 0, 0, Rect{})
	if c.Lookup(scaled) != nil {
		t.Fatal("scale-2 lookup must not hit scale-1 entry")
	}
	if c.Lookup(base) == nil {
		t.Fatal("same-scale lookup must hit")
	}
	// Quantization: near-equal scales share, different scales don't.
	noisy := base
	noisy.DeviceScale = QuantizeScale(1.00001)
	if c.Lookup(noisy) == nil {
		t.Fatal("quantized near-equal scales must share an entry")
	}
}

func TestLayerPixelCache_LRUEvictionObservable(t *testing.T) {
	// 50x50 RGBA = 10,000 bytes each; budget holds 3.
	c := NewLayerPixelCache(30000)
	for i := uint32(0); i < 3; i++ {
		c.Store(makeKey(i, 1), gg.NewPixmap(50, 50), 0, 0, Rect{})
	}
	// Touch layer 0 so layer 1 becomes the LRU.
	if c.Lookup(makeKey(0, 1)) == nil {
		t.Fatal("expected hit")
	}
	c.Store(makeKey(3, 1), gg.NewPixmap(50, 50), 0, 0, Rect{})

	if c.Lookup(makeKey(1, 1)) != nil {
		t.Fatal("LRU entry should have been evicted")
	}
	if c.Lookup(makeKey(0, 1)) == nil || c.Lookup(makeKey(3, 1)) == nil {
		t.Fatal("recent entries must remain")
	}
	st := c.Stats()
	if st.Evictions < 1 {
		t.Fatalf("expected observable eviction, stats=%+v", st)
	}
	if st.Size > st.MaxSize {
		t.Fatal("cache exceeded budget")
	}
}

func TestLayerPixelCache_PeakTracked(t *testing.T) {
	c := NewLayerPixelCache(40000)
	c.Store(makeKey(0, 1), gg.NewPixmap(50, 50), 0, 0, Rect{}) // 10k
	c.Store(makeKey(1, 1), gg.NewPixmap(50, 50), 0, 0, Rect{}) // 20k
	c.Invalidate(makeKey(1, 1))
	st := c.Stats()
	if st.PeakSize != 20000 {
		t.Fatalf("peak = %d, want 20000", st.PeakSize)
	}
	if st.Size != 10000 {
		t.Fatalf("size = %d, want 10000", st.Size)
	}
}

func TestLayerPixelCache_OversizedRejected(t *testing.T) {
	c := NewLayerPixelCache(1024) // 1KB
	// 100x100 = 40KB, exceeds entire budget.
	snap := c.Store(makeKey(0, 1), gg.NewPixmap(100, 100), 0, 0, Rect{})
	if snap == nil {
		t.Fatal("oversized store still returns a usable one-off snapshot")
	}
	if c.EntryCount() != 0 {
		t.Fatal("oversized pixmap must never enter the cache")
	}
	if c.Stats().Oversized != 1 {
		t.Fatal("oversized counter not incremented")
	}
}

func TestLayerPixelCache_InvalidateLayerAllScales(t *testing.T) {
	c := NewLayerPixelCache(1024 * 1024)
	for _, s := range []float32{1, 1.5, 2} {
		k := makeKey(7, s)
		c.Store(k, gg.NewPixmap(8, 8), 0, 0, Rect{})
	}
	c.Store(makeKey(8, 1), gg.NewPixmap(8, 8), 0, 0, Rect{})

	c.InvalidateLayer(7)
	for _, s := range []float32{1, 1.5, 2} {
		if c.Lookup(makeKey(7, s)) != nil {
			t.Fatalf("scale %v entry should be invalidated", s)
		}
	}
	if c.Lookup(makeKey(8, 1)) == nil {
		t.Fatal("other layers must survive InvalidateLayer")
	}
}

func TestLayerPixelCache_ExplicitClear(t *testing.T) {
	c := NewLayerPixelCache(1024 * 1024)
	for i := uint32(0); i < 5; i++ {
		c.Store(makeKey(i, 1), gg.NewPixmap(8, 8), 0, 0, Rect{})
	}
	c.InvalidateAll()
	if c.EntryCount() != 0 || c.Size() != 0 {
		t.Fatal("InvalidateAll must empty the cache")
	}
	if c.Stats().Evictions != 5 {
		t.Fatal("clear must be observable as evictions")
	}
}

func TestLayerPixelCache_WarmSingleFlightAndCancel(t *testing.T) {
	c := NewLayerPixelCache(1024 * 1024)
	key := makeKey(0, 1)
	release := make(chan struct{})
	var builds atomic.Int32

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Warm(context.Background(), key, func() (*gg.Pixmap, int, int, Rect) {
				builds.Add(1)
				<-release
				return gg.NewPixmap(4, 4), 0, 0, Rect{}
			})
		}()
	}
	// Give goroutines time to park on the single-flight.
	time.Sleep(20 * time.Millisecond)
	if c.InFlight() != 1 {
		t.Fatalf("InFlight = %d, want 1", c.InFlight())
	}
	close(release)
	wg.Wait()
	if builds.Load() != 1 {
		t.Fatalf("build ran %d times, want single-flight 1", builds.Load())
	}
	if c.EntryCount() != 1 {
		t.Fatal("warm result must be cached")
	}

	// Cancellation: a second key with a slow build, canceled while waiting.
	key2 := makeKey(1, 1)
	started := make(chan struct{})
	done2 := make(chan struct{})
	go func() {
		_, _ = c.Warm(context.Background(), key2, func() (*gg.Pixmap, int, int, Rect) {
			close(started)
			time.Sleep(100 * time.Millisecond)
			return gg.NewPixmap(4, 4), 0, 0, Rect{}
		})
		close(done2)
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Warm(ctx, key2, func() (*gg.Pixmap, int, int, Rect) {
		t.Fatal("canceled waiter must not trigger a second build")
		return nil, 0, 0, Rect{}
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	<-done2
	if c.Stats().Canceled != 1 {
		t.Fatal("cancel not counted")
	}
	// The shared build still completed and cached.
	if c.Lookup(key2) == nil {
		t.Fatal("shared build result must survive a canceled waiter")
	}
}

func TestLayerPixelCache_ConcurrentStress(t *testing.T) {
	c := NewLayerPixelCache(200000)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := makeKey(uint32((id*200+i)%50), 1)
				switch i % 4 {
				case 0:
					c.Store(key, gg.NewPixmap(10, 10), 0, 0, Rect{})
				case 1:
					_ = c.Lookup(key)
				case 2:
					_ = c.Stats()
				case 3:
					c.Invalidate(key)
				}
			}
		}(g)
	}
	wg.Wait()
	st := c.Stats()
	if st.Size < 0 || st.Size > st.MaxSize {
		t.Fatalf("invariant broken: %+v", st)
	}
}

func TestLayerPixelCache_BudgetShrinkEvicts(t *testing.T) {
	c := NewLayerPixelCache(1024 * 1024)
	for i := uint32(0); i < 10; i++ {
		c.Store(makeKey(i, 1), gg.NewPixmap(50, 50), 0, 0, Rect{}) // 10k each
	}
	c.SetBudget(25000)
	st := c.Stats()
	if st.Size > 25000 {
		t.Fatalf("size %d exceeds new budget", st.Size)
	}
	c.SetBudget(0)
	if c.Enabled() || c.EntryCount() != 0 {
		t.Fatal("zero budget must disable and clear")
	}
}

func TestFilterSpec_HashSensitivity(t *testing.T) {
	a := FilterSpec{Type: FilterBlur, Params: [4]float32{4}}
	b := FilterSpec{Type: FilterBlur, Params: [4]float32{5}}
	const offset = 14695981039346656037
	if a.Hash(offset) == b.Hash(offset) {
		t.Fatal("different blur sigmas must hash differently")
	}
	first := a.Hash(offset)
	if a.Hash(offset) != first {
		t.Fatal("hash must be deterministic")
	}
	if filterHashOf(nil) != 0 {
		t.Fatal("empty filter chain must hash to zero")
	}
}
