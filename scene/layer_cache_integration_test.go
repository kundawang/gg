package scene

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/gogpu/gg"
)

// buildMixedLayerScene builds a scene with static layer blocks (HUD-like)
// plus root-level dynamic content. Each call produces the same commands so
// signatures are stable across frames.
func buildMixedLayerScene() *Scene {
	s := NewScene()

	// Root content: background.
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 0.9, G: 0.9, B: 0.95, A: 1}),
		NewRectShape(0, 0, 512, 512))

	// Static layer 1: a panel with circles.
	s.PushLayer(BlendNormal, 1, nil)
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 0.2, G: 0.3, B: 0.8, A: 1}),
		NewRectShape(40, 40, 180, 120))
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 1, G: 1, B: 0.2, A: 1}),
		NewCircleShape(130, 100, 30))
	s.PopLayer()

	// Static layer 2: a clipped strip with a stroked rounded rect.
	clip := NewRectShape(300, 300, 160, 160)
	s.PushLayer(BlendNormal, 1, clip)
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 0.1, G: 0.7, B: 0.2, A: 1}),
		NewRoundedRectShape(280, 340, 200, 80, 12))
	s.Stroke(DefaultStrokeStyle(), IdentityAffine(),
		SolidBrush(gg.RGBA{R: 0.8, G: 0.1, B: 0.1, A: 1}),
		NewCircleShape(380, 380, 50))
	s.PopLayer()

	// Root content drawn after the layers (dynamic marker).
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 0.9, G: 0.4, B: 0.1, A: 1}),
		NewRectShape(240, 240, 32, 32))
	return s
}

// buildTransparentLayerScene is the same geometry without an opaque
// background: isolated-layer compositing must then be byte-identical to the
// full tile path because every blend starts from transparency.
func buildTransparentLayerScene() *Scene {
	s := NewScene()
	s.PushLayer(BlendNormal, 1, nil)
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 0.2, G: 0.3, B: 0.8, A: 1}),
		NewRectShape(40, 40, 180, 120))
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 1, G: 1, B: 0.2, A: 1}),
		NewCircleShape(130, 100, 30))
	s.PopLayer()
	return s
}

func renderFull(sc *Scene) *gg.Pixmap {
	r := NewRenderer(512, 512, WithWorkers(2))
	defer r.Close()
	target := gg.NewPixmap(512, 512)
	if err := r.Render(target, sc); err != nil {
		panic(err)
	}
	return target
}

func renderCached(sc *Scene, budget int64) (*gg.Pixmap, *Renderer) {
	r := NewRenderer(512, 512, WithWorkers(2), WithLayerCacheBudget(budget))
	target := gg.NewPixmap(512, 512)
	if err := r.Render(target, sc); err != nil {
		panic(err)
	}
	return target, r
}

// maxChannelDelta reports the largest per-channel absolute difference and
// the number of pixels with any differing channel between two equally-sized
// RGBA buffers.
func maxChannelDelta(a, b []byte) (maxDelta int, diffPixels int) {
	n := min(len(a), len(b)) / 4
	for p := 0; p < n; p++ {
		off := p * 4
		pixelDiffers := false
		for c := 0; c < 4; c++ {
			d := int(a[off+c]) - int(b[off+c])
			if d < 0 {
				d = -d
			}
			if d > maxDelta {
				maxDelta = d
			}
			if d > 0 {
				pixelDiffers = true
			}
		}
		if pixelDiffers {
			diffPixels++
		}
	}
	return maxDelta, diffPixels
}

func TestLayerCache_PixelsMatchFullRender(t *testing.T) {
	sc := buildMixedLayerScene()

	full := renderFull(sc)
	cached, r := renderCached(sc, 8*1024*1024)
	defer r.Close()

	// The cached path rasterizes each layer through one tightly-bounded
	// buffer instead of the 64x64 tile grid. The existing tile rasterizer
	// itself differs from single-buffer rasterization by at most 2/255 on
	// AA edges (verified against the full encoding, no cache involved), and
	// isolated-layer compositing adds at most one more level, so a 3-level
	// tolerance isolates cache correctness from pre-existing rasterizer
	// quantization.
	maxDelta, diffPixels := maxChannelDelta(full.Data(), cached.Data())
	if maxDelta > 3 {
		t.Fatalf("channel delta %d exceeds rasterizer AA tolerance", maxDelta)
	}
	if diffPixels > 256 {
		t.Fatalf("AA diff spread too wide: %d pixels", diffPixels)
	}
	st := r.PixelCacheStats()
	if st.Rebuilds != 2 {
		t.Fatalf("expected 2 layer rebuilds on cold frame, got %d", st.Rebuilds)
	}
}

func TestLayerCache_TransparentSceneNoBlendingDrift(t *testing.T) {
	sc := buildTransparentLayerScene()
	full := renderFull(sc)
	cached, r := renderCached(sc, 8*1024*1024)
	defer r.Close()
	// With no background there is no second compositing round, so the only
	// possible drift is the tile-grid AA quantization itself (<=2/255).
	maxDelta, diffPixels := maxChannelDelta(full.Data(), cached.Data())
	if maxDelta > 2 {
		t.Fatalf("transparent layer drift %d exceeds tile AA baseline", maxDelta)
	}
	if diffPixels > 16 {
		t.Fatalf("too many differing pixels without any background: %d", diffPixels)
	}
}

func TestLayerCache_WarmThenHit(t *testing.T) {
	sc := buildMixedLayerScene()
	r := NewRenderer(512, 512, WithWorkers(2), WithLayerCacheBudget(8*1024*1024))
	defer r.Close()

	n, err := r.WarmupLayers(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("warmed %d layers, want 2", n)
	}
	st := r.PixelCacheStats()
	if st.Rebuilds != 2 || st.Entries != 2 {
		t.Fatalf("warmup stats wrong: %+v", st)
	}

	// First real frame: both layers must hit.
	target := gg.NewPixmap(512, 512)
	if err := r.Render(target, sc); err != nil {
		t.Fatal(err)
	}
	st = r.PixelCacheStats()
	if st.Hits < 2 {
		t.Fatalf("expected >=2 hits after warmup, stats=%+v", st)
	}

	// Warm frame must stay within the same AA tolerance.
	full := renderFull(buildMixedLayerScene())
	maxDelta, _ := maxChannelDelta(full.Data(), target.Data())
	if maxDelta > 3 {
		t.Fatalf("warm frame delta %d exceeds tolerance", maxDelta)
	}
}

func TestLayerCache_RepeatedFramesReuse(t *testing.T) {
	sc := buildMixedLayerScene()
	r := NewRenderer(512, 512, WithWorkers(2), WithLayerCacheBudget(8*1024*1024))
	defer r.Close()

	var first []byte
	for frame := 0; frame < 4; frame++ {
		target := gg.NewPixmap(512, 512)
		if err := r.Render(target, sc); err != nil {
			t.Fatal(err)
		}
		if frame == 0 {
			first = append([]byte(nil), target.Data()...)
			continue
		}
		// Cached frames must be byte-identical to the first frame:
		// reused pixmaps mean zero recomputation drift.
		if !bytes.Equal(first, target.Data()) {
			t.Fatalf("frame %d differs from first cached frame", frame)
		}
	}
	st := r.PixelCacheStats()
	if st.Rebuilds != 2 {
		t.Fatalf("static layers rebuilt %d times across 4 frames, want 2", st.Rebuilds)
	}
	if st.Hits != 6 { // 3 warm frames x 2 layers
		t.Fatalf("hits = %d, want 6", st.Hits)
	}
}

func TestLayerCache_InvalidationRebuilds(t *testing.T) {
	sc := buildMixedLayerScene()
	r := NewRenderer(512, 512, WithWorkers(2), WithLayerCacheBudget(8*1024*1024))
	defer r.Close()

	target1 := gg.NewPixmap(512, 512)
	if err := r.Render(target1, sc); err != nil {
		t.Fatal(err)
	}

	// Explicit per-layer invalidation forces one rebuild.
	r.InvalidateLayerCache(0)
	target2 := gg.NewPixmap(512, 512)
	if err := r.Render(target2, sc); err != nil {
		t.Fatal(err)
	}
	st := r.PixelCacheStats()
	if st.Rebuilds != 3 { // 2 cold + 1 rebuild
		t.Fatalf("rebuilds = %d, want 3", st.Rebuilds)
	}
	// Same scene content: output must remain identical.
	if !bytes.Equal(target1.Data(), target2.Data()) {
		t.Fatal("rebuild after invalidation changed pixels for unchanged content")
	}

	// Explicit clear forces both layers to rebuild.
	r.InvalidateLayerCacheAll()
	_ = r.Render(gg.NewPixmap(512, 512), sc)
	st = r.PixelCacheStats()
	if st.Rebuilds != 5 {
		t.Fatalf("rebuilds after clear = %d, want 5", st.Rebuilds)
	}
}

func TestLayerCache_ImageMutationInvalidates(t *testing.T) {
	// A layer containing only an image: mutating its pixels must change the
	// content signature and force a rebuild.
	img := NewImage(64, 64)
	img.Data = make([]byte, 64*64*4)
	for i := range img.Data {
		img.Data[i] = 200
	}

	build := func() *Scene {
		s := NewScene()
		s.PushLayer(BlendNormal, 1, nil)
		s.DrawImage(img, TranslateAffine(100, 100))
		s.PopLayer()
		return s
	}

	r := NewRenderer(512, 512, WithWorkers(2), WithLayerCacheBudget(8*1024*1024))
	defer r.Close()

	t1 := gg.NewPixmap(512, 512)
	if err := r.Render(t1, build()); err != nil {
		t.Fatal(err)
	}
	rebuildsBefore := r.PixelCacheStats().Rebuilds

	// Same encoding, mutated pixels.
	for i := 0; i < len(img.Data); i += 4 {
		img.Data[i] = 10 // change red channel
	}
	img.NotifyPixelsChanged()

	t2 := gg.NewPixmap(512, 512)
	if err := r.Render(t2, build()); err != nil {
		t.Fatal(err)
	}
	if r.PixelCacheStats().Rebuilds != rebuildsBefore+1 {
		t.Fatal("image mutation did not force a layer rebuild")
	}
	if bytes.Equal(t1.Data(), t2.Data()) {
		t.Fatal("mutated image did not change rendered output")
	}
}

func TestLayerCache_LowBudgetFallback(t *testing.T) {
	sc := buildMixedLayerScene()
	full := renderFull(buildMixedLayerScene())

	// Budget far smaller than one layer: the plan is ineligible and the
	// renderer deterministically falls back to the full tile pipeline,
	// producing byte-identical output.
	target, r := renderCached(sc, 256)
	defer r.Close()
	if !bytes.Equal(full.Data(), target.Data()) {
		maxDelta, diffPixels := maxChannelDelta(full.Data(), target.Data())
		t.Fatalf("low-budget fallback must be bit-exact: delta=%d diffs=%d",
			maxDelta, diffPixels)
	}
	if st := r.PixelCacheStats(); st.Entries != 0 {
		t.Fatalf("nothing should be resident, entries=%d", st.Entries)
	}

	// Zero budget via runtime shrink: deterministic disabled fallback.
	r2 := NewRenderer(512, 512, WithWorkers(2), WithLayerCacheBudget(8*1024*1024))
	defer r2.Close()
	_ = r2.Render(gg.NewPixmap(512, 512), sc)
	r2.SetLayerCacheBudget(0)
	if !r2.PixelCacheStats().Disabled {
		t.Fatal("zero budget must report disabled")
	}
	t3 := gg.NewPixmap(512, 512)
	if err := r2.Render(t3, sc); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full.Data(), t3.Data()) {
		t.Fatal("disabled-cache output must be byte-identical to full render")
	}
}

func TestLayerCache_DeviceScaleIsolation(t *testing.T) {
	sc := buildMixedLayerScene()
	r := NewRenderer(512, 512, WithWorkers(2),
		WithLayerCacheBudget(16*1024*1024), WithDeviceScale(1))
	defer r.Close()

	if err := r.Render(gg.NewPixmap(512, 512), sc); err != nil {
		t.Fatal(err)
	}

	// Rebuild the renderer at scale 2: keys differ, no cross-scale reuse.
	r2 := NewRenderer(512, 512, WithWorkers(2),
		WithLayerPixelCache(r.pixelCache), WithDeviceScale(2))
	defer r2.Close()
	if err := r2.Render(gg.NewPixmap(512, 512), sc); err != nil {
		t.Fatal(err)
	}
	st := r2.PixelCacheStats()
	if st.Rebuilds != 4 { // 2 at scale 1 + 2 at scale 2
		t.Fatalf("scale change rebuilds = %d, want 4 (no scale mixing)", st.Rebuilds)
	}
	if st.Entries != 4 {
		t.Fatalf("both scales should be resident, entries=%d", st.Entries)
	}
}

func TestLayerCache_NonEligibleFallsBackToTiles(t *testing.T) {
	// A layer with alpha < 1 is not representable via isolated blit with
	// identical compositing; the renderer must use the full tile pipeline.
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 0.9, G: 0.9, B: 0.95, A: 1}),
		NewRectShape(0, 0, 512, 512))
	s.PushLayer(BlendNormal, 0.5, nil)
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 1, G: 0, B: 0, A: 1}),
		NewRectShape(50, 50, 100, 100))
	s.PopLayer()

	full := renderFull(s)
	target, r := renderCached(s, 8*1024*1024)
	defer r.Close()
	if !bytes.Equal(full.Data(), target.Data()) {
		maxDelta, diffPixels := maxChannelDelta(full.Data(), target.Data())
		t.Fatalf("fallback must be bit-exact: delta=%d diffs=%d", maxDelta, diffPixels)
	}
	if st := r.PixelCacheStats(); st.Entries != 0 {
		t.Fatalf("non-eligible scene must not populate cache: %+v", st)
	}
}

func TestLayerCache_WarmupCancelDoesNotCorrupt(t *testing.T) {
	sc := buildMixedLayerScene()
	r := NewRenderer(512, 512, WithWorkers(2), WithLayerCacheBudget(8*1024*1024))
	defer r.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Already canceled.
	if _, err := r.WarmupLayers(ctx, sc); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Subsequent normal render must stay within AA tolerance.
	target := gg.NewPixmap(512, 512)
	if err := r.Render(target, sc); err != nil {
		t.Fatal(err)
	}
	full := renderFull(buildMixedLayerScene())
	if maxDelta, _ := maxChannelDelta(full.Data(), target.Data()); maxDelta > 3 {
		t.Fatal("render after canceled warmup exceeds AA tolerance")
	}
}
