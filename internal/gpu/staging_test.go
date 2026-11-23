//go:build !nogpu

package gpu

import (
	"errors"
	"sync"
	"testing"
)

func TestStagingPoolReuseSameBucket(t *testing.T) {
	pool := NewStagingPool()
	defer pool.Close()

	b1, err := pool.Acquire(TextureFormatRGBA8, 64, 64, 0)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if got, want := b1.BytesPerRow(), alignBytesPerRow(64*4); got != want {
		t.Errorf("BytesPerRow() = %d, want %d", got, want)
	}
	b1.Release()

	b2, err := pool.Acquire(TextureFormatRGBA8, 64, 64, 0)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if b2 != b1 {
		t.Error("expected buffer reuse for identical format/size/stride")
	}
	b2.Release()

	m := pool.Metrics.Snapshot()
	if m.Allocations != 1 {
		t.Errorf("Allocations = %d, want 1", m.Allocations)
	}
	if m.Reuses != 1 {
		t.Errorf("Reuses = %d, want 1", m.Reuses)
	}
	if pool.IdleCount() != 1 {
		t.Errorf("IdleCount() = %d, want 1", pool.IdleCount())
	}
}

func TestStagingPoolBucketing(t *testing.T) {
	pool := NewStagingPool()
	defer pool.Close()

	// Different formats must not share buffers.
	rgba, _ := pool.Acquire(TextureFormatRGBA8, 16, 16, 0)
	r8, _ := pool.Acquire(TextureFormatR8, 16, 16, 0)
	bgra, _ := pool.Acquire(TextureFormatBGRA8, 16, 16, 0)
	if rgba == r8 || rgba == bgra || r8 == bgra {
		t.Error("different formats must use different buckets")
	}
	rgba.Release()
	r8.Release()
	bgra.Release()

	// R8 16x16 (rowBytes 16) and RGBA 4x16 (rowBytes 16) share stride but
	// differ in format — still distinct buckets.
	r8b, _ := pool.Acquire(TextureFormatR8, 16, 16, 0)
	if r8b != r8 {
		t.Error("expected R8 buffer reuse")
	}
	r8b.Release()

	// Size-class bucketing: a smaller request reuses a larger size-class
	// buffer only if format and aligned stride match.
	big, _ := pool.Acquire(TextureFormatRGBA8, 64, 64, 0) // bpr 256, class >= 64*256
	big.Release()
	small, _ := pool.Acquire(TextureFormatRGBA8, 64, 8, 0) // same bpr 256, smaller class
	if small == big {
		t.Log("size-class bucketing allowed reuse of larger class (acceptable)")
	}
	small.Release()
}

func TestStagingPoolRowAlignment(t *testing.T) {
	pool := NewStagingPool()
	defer pool.Close()

	// 10 RGBA pixels = 40 bytes/row → aligned to 256.
	buf, err := pool.Acquire(TextureFormatRGBA8, 10, 3, 0)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if buf.BytesPerRow() != 256 {
		t.Errorf("BytesPerRow() = %d, want 256 (aligned from 40)", buf.BytesPerRow())
	}
	if buf.RowBytes() != 40 {
		t.Errorf("RowBytes() = %d, want 40", buf.RowBytes())
	}
	// Buffer size: 256*(3-1) + 40 = 552 → size class 1024.
	want := stagingBufferSize(256, 40, 3)
	if want != 552 {
		t.Fatalf("stagingBufferSize() = %d, want 552", want)
	}
	if len(buf.Bytes()) < want {
		t.Errorf("buffer len = %d, need >= %d", len(buf.Bytes()), want)
	}
	buf.Release()
}

func TestStagingBufferRefcount(t *testing.T) {
	pool := NewStagingPool()
	defer pool.Close()

	b1, _ := pool.Acquire(TextureFormatRGBA8, 32, 32, 0)
	b1.Retain() // async user holds a second reference
	b1.Release()
	// Still referenced — must not be recycled.
	if pool.IdleCount() != 0 {
		t.Error("buffer recycled while still referenced")
	}
	b2, _ := pool.Acquire(TextureFormatRGBA8, 32, 32, 0)
	if b2 == b1 {
		t.Error("pool handed out a buffer that is still in use")
	}
	b2.Release()
	b1.Release() // last reference drops → recycled
	if pool.IdleCount() != 2 {
		t.Errorf("IdleCount() = %d, want 2", pool.IdleCount())
	}
}

func TestStagingBufferPoison(t *testing.T) {
	pool := NewStagingPool()
	defer pool.Close()

	b1, _ := pool.Acquire(TextureFormatRGBA8, 32, 32, 0)
	b1.Poison()
	b1.Release()

	if pool.IdleCount() != 0 {
		t.Error("poisoned buffer was returned to the pool")
	}
	m := pool.Metrics.Snapshot()
	if m.Poisoned != 1 {
		t.Errorf("Poisoned = %d, want 1", m.Poisoned)
	}
	// Next acquire must allocate fresh.
	b2, _ := pool.Acquire(TextureFormatRGBA8, 32, 32, 0)
	if b2 == b1 {
		t.Error("poisoned buffer was reused")
	}
	if m.Allocations != 1 {
		t.Errorf("Allocations = %d, want 1 before fresh acquire", m.Allocations)
	}
	b2.Release()
}

func TestStagingPoolDeviceLost(t *testing.T) {
	pool := NewStagingPool()

	inFlight, _ := pool.Acquire(TextureFormatRGBA8, 32, 32, 0)
	idle, _ := pool.Acquire(TextureFormatRGBA8, 32, 32, 0)
	idle.Release()
	if pool.IdleCount() != 1 {
		t.Fatalf("IdleCount() = %d, want 1", pool.IdleCount())
	}

	pool.NotifyDeviceLost()

	// Idle buffers drained; new acquisitions fail.
	if pool.IdleCount() != 0 {
		t.Error("idle buffers not drained on device loss")
	}
	if _, err := pool.Acquire(TextureFormatRGBA8, 32, 32, 0); !errors.Is(err, ErrStagingDeviceLost) {
		t.Errorf("Acquire() after device loss error = %v, want %v", err, ErrStagingDeviceLost)
	}

	// In-flight buffer released after loss is discarded, never reused.
	inFlight.Release()
	if m := pool.Metrics.Snapshot(); m.LiveBytes != 0 {
		t.Errorf("LiveBytes = %d after device loss, want 0", m.LiveBytes)
	}

	// After Reset the pool works again, but nothing stale remains.
	pool.Reset()
	b, err := pool.Acquire(TextureFormatRGBA8, 32, 32, 0)
	if err != nil {
		t.Fatalf("Acquire() after Reset error = %v", err)
	}
	if b == inFlight || b == idle {
		t.Error("stale pre-loss buffer reused after Reset")
	}
	b.Release()
	pool.Close()
}

func TestStagingPoolClose(t *testing.T) {
	pool := NewStagingPool()
	b, _ := pool.Acquire(TextureFormatRGBA8, 16, 16, 0)
	pool.Close()

	if _, err := pool.Acquire(TextureFormatRGBA8, 16, 16, 0); !errors.Is(err, ErrStagingPoolClosed) {
		t.Errorf("Acquire() after Close error = %v, want %v", err, ErrStagingPoolClosed)
	}
	// Releasing an in-use buffer after close must not panic or recycle.
	b.Release()
	if pool.IdleCount() != 0 {
		t.Error("buffer recycled into closed pool")
	}
	// Close is idempotent.
	pool.Close()
}

func TestStagingPoolInvalidSizes(t *testing.T) {
	pool := NewStagingPool()
	defer pool.Close()

	if _, err := pool.Acquire(TextureFormatRGBA8, 0, 10, 0); !errors.Is(err, ErrInvalidDimensions) {
		t.Errorf("zero width error = %v, want %v", err, ErrInvalidDimensions)
	}
	if _, err := pool.Acquire(TextureFormatRGBA8, 10, -1, 0); !errors.Is(err, ErrInvalidDimensions) {
		t.Errorf("negative height error = %v, want %v", err, ErrInvalidDimensions)
	}
	if _, err := pool.Acquire(TextureFormatRGBA8, MaxTextureDimension+1, 1, 0); !errors.Is(err, ErrTextureTooLarge) {
		t.Errorf("oversized width error = %v, want %v", err, ErrTextureTooLarge)
	}
	// Stride smaller than a row of pixels.
	if _, err := pool.Acquire(TextureFormatRGBA8, 10, 10, 8); !errors.Is(err, ErrInvalidDimensions) {
		t.Errorf("short stride error = %v, want %v", err, ErrInvalidDimensions)
	}
}

func TestStagingPoolMetricsPeak(t *testing.T) {
	pool := NewStagingPool()
	defer pool.Close()

	b1, _ := pool.Acquire(TextureFormatRGBA8, 64, 64, 0)
	b2, _ := pool.Acquire(TextureFormatRGBA8, 64, 64, 0)
	m := pool.Metrics.Snapshot()
	if m.LiveBytes <= 0 || m.PeakLiveBytes != m.LiveBytes {
		t.Errorf("LiveBytes=%d Peak=%d, want equal and positive", m.LiveBytes, m.PeakLiveBytes)
	}
	b1.Release()
	b2.Release()
	m = pool.Metrics.Snapshot()
	if m.PeakLiveBytes < m.LiveBytes {
		t.Errorf("Peak %d < Live %d after release", m.PeakLiveBytes, m.LiveBytes)
	}
}

func TestStagingPoolConcurrent(t *testing.T) {
	pool := NewStagingPool()
	defer pool.Close()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			format := TextureFormatRGBA8
			if g%2 == 1 {
				format = TextureFormatR8
			}
			for i := 0; i < 200; i++ {
				buf, err := pool.Acquire(format, 32, 32, 0)
				if err != nil {
					t.Errorf("Acquire() error = %v", err)
					return
				}
				if i%7 == 0 {
					buf.Poison()
				}
				buf.Release()
			}
		}(g)
	}
	wg.Wait()
}

func BenchmarkStagingPoolAcquireRelease(b *testing.B) {
	pool := NewStagingPool()
	defer pool.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf, err := pool.Acquire(TextureFormatRGBA8, 256, 256, 0)
		if err != nil {
			b.Fatal(err)
		}
		buf.Release()
	}
}

func BenchmarkStagingPoolAcquireParallel(b *testing.B) {
	pool := NewStagingPool()
	defer pool.Close()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf, err := pool.Acquire(TextureFormatRGBA8, 256, 256, 0)
			if err != nil {
				b.Fatal(err)
			}
			buf.Release()
		}
	})
}
