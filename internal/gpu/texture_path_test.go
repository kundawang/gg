//go:build !nogpu

package gpu

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gogpu/gg"
)

// makeCheckerboard builds a premultiplied RGBA checkerboard with fully
// transparent, semi-transparent, and opaque cells — the classic alpha
// torture pattern for round-trip validation.
func makeCheckerboard(w, h, cell int) *gg.Pixmap {
	pm := gg.NewPixmap(w, h)
	d := pm.Data()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			even := ((x/cell)+(y/cell))%2 == 0
			switch {
			case even && (x+y)%3 == 0:
				// Fully transparent.
				d[i+0], d[i+1], d[i+2], d[i+3] = 0, 0, 0, 0
			case even:
				// Opaque white.
				d[i+0], d[i+1], d[i+2], d[i+3] = 255, 255, 255, 255
			case (x+y)%3 == 0:
				// Semi-transparent red (premultiplied).
				d[i+0], d[i+1], d[i+2], d[i+3] = 128, 0, 0, 128
			default:
				// Semi-transparent blue (premultiplied).
				d[i+0], d[i+1], d[i+2], d[i+3] = 0, 0, 64, 200
			}
		}
	}
	return pm
}

// assertPixelsEqual compares two pixmaps pixel-exactly.
func assertPixelsEqual(t *testing.T, got, want *gg.Pixmap, what string) {
	t.Helper()
	if got.Width() != want.Width() || got.Height() != want.Height() {
		t.Fatalf("%s: size %dx%d, want %dx%d", what, got.Width(), got.Height(), want.Width(), want.Height())
	}
	gd, wd := got.Data(), want.Data()
	for i := range wd {
		if gd[i] != wd[i] {
			px := i / 4
			t.Fatalf("%s: pixel (%d,%d) channel %d = %d, want %d",
				what, px%got.Width(), px/got.Width(), i%4, gd[i], wd[i])
		}
	}
}

func TestTextureCheckerboardRoundTrip(t *testing.T) {
	const w, h = 64, 48
	src := makeCheckerboard(w, h, 8)

	for _, format := range []TextureFormat{TextureFormatRGBA8, TextureFormatBGRA8} {
		t.Run(format.String(), func(t *testing.T) {
			tex, err := CreateTexture(nil, TextureConfig{Width: w, Height: h, Format: format, Label: "checker"})
			if err != nil {
				t.Fatalf("CreateTexture() error = %v", err)
			}
			defer tex.Close()

			if err := tex.UploadPixmap(src); err != nil {
				t.Fatalf("UploadPixmap() error = %v", err)
			}

			got, err := tex.DownloadPixmap()
			if err != nil {
				t.Fatalf("DownloadPixmap() error = %v", err)
			}
			assertPixelsEqual(t, got, src, format.String()+" round trip")

			m := tex.Metrics()
			if m.UploadCount != 1 || m.DownloadCount != 1 {
				t.Errorf("metrics = %+v, want 1 upload / 1 download", m)
			}
			if m.UploadBytes != uint64(w*h*4) {
				t.Errorf("UploadBytes = %d, want %d", m.UploadBytes, w*h*4)
			}
		})
	}
}

func TestTextureR8RoundTrip(t *testing.T) {
	const w, h = 32, 32
	src := makeCheckerboard(w, h, 4)

	tex, err := CreateTexture(nil, TextureConfig{Width: w, Height: h, Format: TextureFormatR8, Label: "mask"})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}
	defer tex.Close()

	if err := tex.UploadPixmap(src); err != nil {
		t.Fatalf("UploadPixmap() error = %v", err)
	}
	got, err := tex.DownloadPixmap()
	if err != nil {
		t.Fatalf("DownloadPixmap() error = %v", err)
	}

	// R8 keeps coverage (alpha) only; RGB expands to white.
	sd, gd := src.Data(), got.Data()
	for i := 0; i < w*h; i++ {
		if gd[i*4+3] != sd[i*4+3] {
			t.Fatalf("pixel %d alpha = %d, want %d", i, gd[i*4+3], sd[i*4+3])
		}
		if gd[i*4+0] != 255 || gd[i*4+1] != 255 || gd[i*4+2] != 255 {
			t.Fatalf("pixel %d rgb = (%d,%d,%d), want white", i, gd[i*4+0], gd[i*4+1], gd[i*4+2])
		}
	}
}

func TestTextureRegionUploadDownload(t *testing.T) {
	const w, h = 64, 64
	tex, err := CreateTexture(nil, TextureConfig{Width: w, Height: h, Format: TextureFormatRGBA8})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}
	defer tex.Close()

	// Base: opaque black.
	base := gg.NewPixmap(w, h)
	if err := tex.UploadPixmap(base); err != nil {
		t.Fatalf("UploadPixmap() error = %v", err)
	}

	// Upload a checkerboard patch at a non-zero origin.
	patch := makeCheckerboard(16, 16, 4)
	const ox, oy = 20, 12
	if err := tex.UploadRegion(ox, oy, patch); err != nil {
		t.Fatalf("UploadRegion() error = %v", err)
	}

	// Region readback must match the patch exactly.
	gotPatch, err := tex.DownloadRegion(ox, oy, 16, 16)
	if err != nil {
		t.Fatalf("DownloadRegion() error = %v", err)
	}
	assertPixelsEqual(t, gotPatch, patch, "region round trip")

	// Pixels outside the patch must remain black.
	full, err := tex.DownloadPixmap()
	if err != nil {
		t.Fatalf("DownloadPixmap() error = %v", err)
	}
	fd := full.Data()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x >= ox && x < ox+16 && y >= oy && y < oy+16 {
				continue
			}
			i := (y*w + x) * 4
			if fd[i] != 0 || fd[i+1] != 0 || fd[i+2] != 0 || fd[i+3] != 0 {
				t.Fatalf("pixel (%d,%d) outside patch modified: %v", x, y, fd[i:i+4])
			}
		}
	}
}

func TestTextureMultipleUploadsStableAllocs(t *testing.T) {
	const w, h = 128, 128
	tex, err := CreateTexture(nil, TextureConfig{Width: w, Height: h, Format: TextureFormatRGBA8})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}
	defer tex.Close()

	src := makeCheckerboard(w, h, 16)
	if err := tex.UploadPixmap(src); err != nil {
		t.Fatalf("first UploadPixmap() error = %v", err)
	}

	// Steady-state re-uploads (the "GPU preview re-uploads the same atlas"
	// hot path) must not allocate: the CPU mirror is reused.
	allocs := testing.AllocsPerRun(50, func() {
		if err := tex.UploadPixmap(src); err != nil {
			t.Fatalf("UploadPixmap() error = %v", err)
		}
	})
	if allocs != 0 {
		t.Errorf("steady-state UploadPixmap allocs = %.1f, want 0", allocs)
	}

	m := tex.Metrics()
	if m.UploadCount < 51 {
		t.Errorf("UploadCount = %d, want >= 51", m.UploadCount)
	}

	// Data integrity after many uploads.
	got, err := tex.DownloadPixmap()
	if err != nil {
		t.Fatalf("DownloadPixmap() error = %v", err)
	}
	assertPixelsEqual(t, got, src, "post-multi-upload")
}

func TestTextureUploadErrors(t *testing.T) {
	// Oversized dimensions.
	if _, err := CreateTexture(nil, TextureConfig{Width: MaxTextureDimension + 1, Height: 1, Format: TextureFormatRGBA8}); !errors.Is(err, ErrTextureTooLarge) {
		t.Errorf("oversized width error = %v, want %v", err, ErrTextureTooLarge)
	}
	// Byte-size overflow guard: 16384x16384 RGBA = 1 GiB > limit? exactly 1 GiB — allowed.
	// 16384x16384 with a hypothetical 8-byte format would overflow; test the
	// limit boundary via a valid-but-huge request instead.
	if _, err := CreateTexture(nil, TextureConfig{Width: 0, Height: 10, Format: TextureFormatRGBA8}); !errors.Is(err, ErrInvalidDimensions) {
		t.Errorf("zero width error = %v, want %v", err, ErrInvalidDimensions)
	}

	tex, err := CreateTexture(nil, TextureConfig{Width: 16, Height: 16, Format: TextureFormatRGBA8})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}
	defer tex.Close()

	// Empty pixmap upload.
	empty := gg.NewPixmap(1, 1)
	empty.Data()[3] = 0 // ensure non-nil data
	if err := tex.UploadRegion(0, 0, gg.NewPixmap(0, 0)); !errors.Is(err, ErrEmptyPixmap) {
		t.Errorf("empty region upload error = %v, want %v", err, ErrEmptyPixmap)
	}
	_ = empty

	// Region out of bounds.
	pm := gg.NewPixmap(8, 8)
	if err := tex.UploadRegion(12, 12, pm); !errors.Is(err, ErrInvalidDimensions) {
		t.Errorf("out-of-bounds region error = %v, want %v", err, ErrInvalidDimensions)
	}
	if err := tex.UploadRegion(-1, 0, pm); !errors.Is(err, ErrInvalidDimensions) {
		t.Errorf("negative origin error = %v, want %v", err, ErrInvalidDimensions)
	}

	// Format mismatch on raw upload.
	if err := tex.UploadRawRegion(0, 0, 4, 4, make([]byte, 4*4), 0, TextureFormatR8); !errors.Is(err, ErrTextureFormatMismatch) {
		t.Errorf("format mismatch error = %v, want %v", err, ErrTextureFormatMismatch)
	}
	// Raw upload with too-short data.
	if err := tex.UploadRawRegion(0, 0, 4, 4, make([]byte, 10), 0, TextureFormatRGBA8); !errors.Is(err, ErrInvalidDimensions) {
		t.Errorf("short raw data error = %v, want %v", err, ErrInvalidDimensions)
	}
}

func TestTextureUploadRawRegionStride(t *testing.T) {
	tex, err := CreateTexture(nil, TextureConfig{Width: 8, Height: 8, Format: TextureFormatRGBA8})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}
	defer tex.Close()

	// 4x4 patch with a padded source stride (8 pixels per row).
	const stride = 8 * 4
	data := make([]byte, stride*4)
	for px := 0; px < 4*4; px++ {
		row, col := px/4, px%4
		off := row*stride + col*4
		data[off+0] = uint8(px)       //nolint:gosec // px < 16
		data[off+3] = uint8(200 + px) //nolint:gosec // fits
	}
	if err := tex.UploadRawRegion(2, 2, 4, 4, data, stride, TextureFormatRGBA8); err != nil {
		t.Fatalf("UploadRawRegion() error = %v", err)
	}

	got, err := tex.DownloadRegion(2, 2, 4, 4)
	if err != nil {
		t.Fatalf("DownloadRegion() error = %v", err)
	}
	gd := got.Data()
	for px := 0; px < 16; px++ {
		if gd[px*4] != uint8(px) || gd[px*4+3] != uint8(200+px) { //nolint:gosec // test values fit
			t.Fatalf("pixel %d = (%d,%d), want (%d,%d)", px, gd[px*4], gd[px*4+3], px, 200+px)
		}
	}
}

func TestTextureConcurrentUploadDownload(t *testing.T) {
	const w, h = 64, 64
	tex, err := CreateTexture(nil, TextureConfig{Width: w, Height: h, Format: TextureFormatRGBA8})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}
	defer tex.Close()

	src := makeCheckerboard(w, h, 8)
	// Seed the CPU mirror so concurrent downloads always have data.
	if err := tex.UploadPixmap(src); err != nil {
		t.Fatalf("seed UploadPixmap() error = %v", err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := tex.UploadPixmap(src); err != nil {
					t.Errorf("UploadPixmap() error = %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				pm, err := tex.DownloadRegion(0, 0, 16, 16)
				if err != nil {
					t.Errorf("DownloadRegion() error = %v", err)
					return
				}
				_ = pm
			}
		}()
	}
	wg.Wait()
}

// --- Async upload queue ---

func TestUploadQueueAsync(t *testing.T) {
	pool := NewStagingPool()
	defer pool.Close()
	q := NewTextureUploadQueue(2, 16, pool)
	defer q.Close()

	tex, err := CreateTexture(nil, TextureConfig{Width: 32, Height: 32, Format: TextureFormatRGBA8})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}
	defer tex.Close()

	src := makeCheckerboard(32, 32, 8)

	const n = 8
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		if _, err := q.Submit(tex, 0, 0, src, func(err error) { done <- err }); err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatalf("upload %d error = %v", i, err)
		}
	}

	got, err := tex.DownloadPixmap()
	if err != nil {
		t.Fatalf("DownloadPixmap() error = %v", err)
	}
	assertPixelsEqual(t, got, src, "async queue round trip")

	m := q.Metrics.Snapshot()
	if m.Submitted != n || m.Completed != n || m.Failed != 0 {
		t.Errorf("queue metrics = %+v, want %d submitted/completed, 0 failed", m, n)
	}
}

func TestUploadQueueCancel(t *testing.T) {
	q := NewTextureUploadQueue(1, 4, nil)
	defer q.Close()

	tex, err := CreateTexture(nil, TextureConfig{Width: 8, Height: 8, Format: TextureFormatRGBA8})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}
	defer tex.Close()

	// Block the single worker with a slow upload.
	release := make(chan struct{})
	q.SetUploadFunc(func(t *GPUTexture, x, y int, pm *gg.Pixmap) error {
		<-release
		return t.UploadRegion(x, y, pm)
	})

	pm := gg.NewPixmap(8, 8)
	first := make(chan error, 1)
	if _, err := q.Submit(tex, 0, 0, pm, func(err error) { first <- err }); err != nil {
		t.Fatalf("Submit(1) error = %v", err)
	}
	// Wait until the worker is inside the upload function.
	time.Sleep(20 * time.Millisecond)

	second := make(chan error, 1)
	id2, err := q.Submit(tex, 0, 0, pm, func(err error) { second <- err })
	if err != nil {
		t.Fatalf("Submit(2) error = %v", err)
	}
	if !q.Cancel(id2) {
		t.Fatal("Cancel() returned false for pending task")
	}
	close(release)

	if err := <-first; err != nil {
		t.Errorf("first upload error = %v", err)
	}
	if err := <-second; !errors.Is(err, ErrUploadCanceled) {
		t.Errorf("canceled upload error = %v, want %v", err, ErrUploadCanceled)
	}

	m := q.Metrics.Snapshot()
	if m.Canceled != 1 || m.Completed != 1 {
		t.Errorf("metrics = %+v, want 1 canceled / 1 completed", m)
	}
}

func TestUploadQueueDeviceLost(t *testing.T) {
	pool := NewStagingPool()
	defer pool.Close()
	q := NewTextureUploadQueue(1, 8, pool)
	defer q.Close()

	tex, err := CreateTexture(nil, TextureConfig{Width: 8, Height: 8, Format: TextureFormatRGBA8})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}
	defer tex.Close()

	// Block the worker so the second task stays pending.
	release := make(chan struct{})
	q.SetUploadFunc(func(t *GPUTexture, x, y int, pm *gg.Pixmap) error {
		<-release
		return t.UploadRegion(x, y, pm)
	})

	pm := gg.NewPixmap(8, 8)
	first := make(chan error, 1)
	if _, err := q.Submit(tex, 0, 0, pm, func(err error) { first <- err }); err != nil {
		t.Fatalf("Submit(1) error = %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	second := make(chan error, 1)
	if _, err := q.Submit(tex, 0, 0, pm, func(err error) { second <- err }); err != nil {
		t.Fatalf("Submit(2) error = %v", err)
	}

	q.NotifyDeviceLost()

	// Pool must reject new staging buffers until reset.
	if _, err := pool.Acquire(TextureFormatRGBA8, 8, 8, 0); !errors.Is(err, ErrStagingDeviceLost) {
		t.Errorf("pool Acquire after device loss error = %v, want %v", err, ErrStagingDeviceLost)
	}

	close(release)
	if err := <-first; err != nil {
		t.Errorf("in-flight upload error = %v (should complete)", err)
	}
	if err := <-second; !errors.Is(err, ErrDeviceLost) {
		t.Errorf("pending upload error = %v, want %v", err, ErrDeviceLost)
	}
}

func TestUploadQueueFaultInjection(t *testing.T) {
	q := NewTextureUploadQueue(1, 4, nil)
	defer q.Close()

	tex, err := CreateTexture(nil, TextureConfig{Width: 8, Height: 8, Format: TextureFormatRGBA8})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}
	defer tex.Close()

	injected := errors.New("injected readback/write failure")
	q.SetUploadFunc(func(t *GPUTexture, x, y int, pm *gg.Pixmap) error {
		return injected
	})

	done := make(chan error, 1)
	if _, err := q.Submit(tex, 0, 0, gg.NewPixmap(8, 8), func(err error) { done <- err }); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if err := <-done; !errors.Is(err, injected) {
		t.Errorf("upload error = %v, want injected failure", err)
	}
	if m := q.Metrics.Snapshot(); m.Failed != 1 {
		t.Errorf("Failed = %d, want 1", m.Failed)
	}
}

func TestUploadQueueCloseWithPending(t *testing.T) {
	q := NewTextureUploadQueue(1, 8, nil)

	tex, err := CreateTexture(nil, TextureConfig{Width: 8, Height: 8, Format: TextureFormatRGBA8})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}

	release := make(chan struct{})
	q.SetUploadFunc(func(t *GPUTexture, x, y int, pm *gg.Pixmap) error {
		<-release
		return t.UploadRegion(x, y, pm)
	})

	pm := gg.NewPixmap(8, 8)
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		if _, err := q.Submit(tex, 0, 0, pm, func(err error) { results <- err }); err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
	}
	time.Sleep(20 * time.Millisecond)

	// Close the texture while uploads are in flight: the queue holds
	// references, so resources must stay valid until tasks finish.
	tex.Close()
	close(release)
	q.Close()

	// Every task must terminate with a well-defined outcome: success,
	// cancellation, or a clean ErrTextureReleased (texture closed mid-flight).
	// The point of this test is refcount safety — no crash, no use-after-free
	// (verified under -race).
	var completed, canceled, released int
	for i := 0; i < 4; i++ {
		err := <-results
		switch {
		case err == nil:
			completed++
		case errors.Is(err, ErrUploadCanceled):
			canceled++
		case errors.Is(err, ErrTextureReleased):
			released++
		default:
			t.Errorf("unexpected task error: %v", err)
		}
	}
	if completed+canceled+released != 4 {
		t.Errorf("completed=%d canceled=%d released=%d, want total 4", completed, canceled, released)
	}
	if !tex.IsReleased() {
		t.Error("texture should be released")
	}
}

func TestUploadQueueSubmitErrors(t *testing.T) {
	q := NewTextureUploadQueue(1, 4, nil)
	defer q.Close()

	if _, err := q.Submit(nil, 0, 0, gg.NewPixmap(8, 8), nil); !errors.Is(err, ErrNilTexture) {
		t.Errorf("nil texture error = %v, want %v", err, ErrNilTexture)
	}
	tex, err := CreateTexture(nil, TextureConfig{Width: 8, Height: 8, Format: TextureFormatRGBA8})
	if err != nil {
		t.Fatalf("CreateTexture() error = %v", err)
	}
	if _, err := q.Submit(tex, 0, 0, nil, nil); !errors.Is(err, ErrNilPixmap) {
		t.Errorf("nil pixmap error = %v, want %v", err, ErrNilPixmap)
	}
	tex.Close()
	if _, err := q.Submit(tex, 0, 0, gg.NewPixmap(8, 8), nil); !errors.Is(err, ErrTextureReleased) {
		t.Errorf("released texture error = %v, want %v", err, ErrTextureReleased)
	}
}

// --- Benchmarks ---

func BenchmarkTextureUploadRGBA8(b *testing.B) {
	tex, err := CreateTexture(nil, TextureConfig{Width: 512, Height: 512, Format: TextureFormatRGBA8})
	if err != nil {
		b.Fatal(err)
	}
	defer tex.Close()
	src := makeCheckerboard(512, 512, 32)
	if err := tex.UploadPixmap(src); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tex.UploadPixmap(src); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTextureUploadRegion(b *testing.B) {
	tex, err := CreateTexture(nil, TextureConfig{Width: 2048, Height: 2048, Format: TextureFormatRGBA8})
	if err != nil {
		b.Fatal(err)
	}
	defer tex.Close()
	patch := makeCheckerboard(256, 256, 32)
	if err := tex.UploadRegion(0, 0, patch); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tex.UploadRegion(256, 256, patch); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTextureDownload(b *testing.B) {
	tex, err := CreateTexture(nil, TextureConfig{Width: 512, Height: 512, Format: TextureFormatRGBA8})
	if err != nil {
		b.Fatal(err)
	}
	defer tex.Close()
	if err := tex.UploadPixmap(makeCheckerboard(512, 512, 32)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pm, err := tex.DownloadPixmap()
		if err != nil {
			b.Fatal(err)
		}
		_ = pm
	}
}

func BenchmarkUploadQueueSubmit(b *testing.B) {
	q := NewTextureUploadQueue(2, 1024, nil)
	defer q.Close()
	tex, err := CreateTexture(nil, TextureConfig{Width: 256, Height: 256, Format: TextureFormatRGBA8})
	if err != nil {
		b.Fatal(err)
	}
	defer tex.Close()
	src := makeCheckerboard(256, 256, 32)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		done := make(chan error, 1)
		if _, err := q.Submit(tex, 0, 0, src, func(err error) { done <- err }); err != nil {
			b.Fatal(err)
		}
		if err := <-done; err != nil {
			b.Fatal(err)
		}
	}
}
