package scene

import (
	"context"
	"testing"

	"github.com/gogpu/gg"
)

// BenchmarkLayerCache_Cold measures a first frame where every static layer
// must be rasterized and inserted. It is directly comparable to
// BenchmarkLayerCache_FullRender: the miss path pays full rasterization plus
// cache bookkeeping.
func BenchmarkLayerCache_Cold(b *testing.B) {
	scenes := make([]*Scene, b.N)
	for i := range scenes {
		scenes[i] = buildBenchmarkMixedScene()
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := NewRenderer(1024, 1024, WithWorkers(4), WithLayerCacheBudget(64*1024*1024))
		target := gg.NewPixmap(1024, 1024)
		_ = r.Render(target, scenes[i])
		if got := r.PixelCacheStats().Rebuilds; got != benchmarkStaticLayerCount {
			b.Fatalf("rebuilds=%d, want %d", got, benchmarkStaticLayerCount)
		}
		r.Close()
	}
}

// BenchmarkLayerCache_Warm measures steady-state frames where all static
// layers are served from the cache and only root/dynamic content is
// rasterized through tiles.
func BenchmarkLayerCache_Warm(b *testing.B) {
	sc := buildBenchmarkMixedScene()
	r := NewRenderer(1024, 1024, WithWorkers(4), WithLayerCacheBudget(64*1024*1024))
	defer r.Close()
	if _, err := r.WarmupLayers(context.Background(), sc); err != nil {
		b.Fatal(err)
	}
	target := gg.NewPixmap(1024, 1024)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.Render(target, sc)
	}
}

// BenchmarkLayerCache_Invalidation measures a frame after one static layer
// is invalidated: exactly one layer rebuild, everything else stays hot.
func BenchmarkLayerCache_Invalidation(b *testing.B) {
	sc := buildBenchmarkMixedScene()
	r := NewRenderer(1024, 1024, WithWorkers(4), WithLayerCacheBudget(64*1024*1024))
	defer r.Close()
	_ = r.Render(gg.NewPixmap(1024, 1024), sc)
	target := gg.NewPixmap(1024, 1024)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.InvalidateLayerCache(0)
		_ = r.Render(target, sc)
	}
}

// BenchmarkLayerCache_LowBudget measures the deterministic fallback when the
// byte budget cannot hold any layer: the full tile pipeline runs and output
// stays byte-identical to uncached rendering.
func BenchmarkLayerCache_LowBudget(b *testing.B) {
	sc := buildBenchmarkMixedScene()
	r := NewRenderer(1024, 1024, WithWorkers(4), WithLayerCacheBudget(128))
	defer r.Close()
	target := gg.NewPixmap(1024, 1024)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.Render(target, sc)
	}
	if st := r.PixelCacheStats(); st.Entries != 0 {
		b.Fatalf("low budget must keep cache empty, entries=%d", st.Entries)
	}
}

// BenchmarkLayerCache_FullRender is the control: no layer cache at all, the
// original tile-only pipeline rasterizing the identical scene every frame.
func BenchmarkLayerCache_FullRender(b *testing.B) {
	sc := buildBenchmarkMixedScene()
	r := NewRenderer(1024, 1024, WithWorkers(4))
	defer r.Close()
	target := gg.NewPixmap(1024, 1024)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.MarkAllDirty()
		_ = r.Render(target, sc)
	}
}

// BenchmarkLayerCache_WarmupOnly measures pre-rasterization cost without a
// final composite, i.e. work that can be scheduled off the critical frame.
func BenchmarkLayerCache_WarmupOnly(b *testing.B) {
	sc := buildBenchmarkMixedScene()
	r := NewRenderer(1024, 1024, WithWorkers(4), WithLayerCacheBudget(64*1024*1024))
	defer r.Close()
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.InvalidateLayerCacheAll()
		if _, err := r.WarmupLayers(ctx, sc); err != nil {
			b.Fatal(err)
		}
	}
}

// benchmarkStaticLayerCount must match the number of pushed layers in
// buildBenchmarkMixedScene.
const benchmarkStaticLayerCount = 6

// buildBenchmarkMixedScene constructs a multi-layer static/dynamic scene:
// an opaque background plus six independent static HUD layers (panels,
// gauges, clipped strips) and small root-level dynamic markers. Geometry is
// chosen to stress both fill/stroke/clip paths and offscreen allocation.
func buildBenchmarkMixedScene() *Scene {
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 0.12, G: 0.12, B: 0.16, A: 1}),
		NewRectShape(0, 0, 1024, 1024))

	panelColors := []gg.RGBA{
		{R: 0.85, G: 0.3, B: 0.3, A: 1},
		{R: 0.3, G: 0.75, B: 0.4, A: 1},
		{R: 0.3, G: 0.45, B: 0.9, A: 1},
		{R: 0.85, G: 0.7, B: 0.25, A: 1},
		{R: 0.65, G: 0.35, B: 0.8, A: 1},
		{R: 0.25, G: 0.75, B: 0.8, A: 1},
	}
	positions := [][2]float32{
		{40, 40},
		{380, 40},
		{720, 40},
		{40, 560},
		{380, 560},
		{720, 560},
	}

	for i, pos := range positions {
		// Alternate clipped and unclipped layers to exercise both paths.
		var clip Shape
		if i%2 == 1 {
			clip = NewRoundedRectShape(pos[0]-10, pos[1]-10, 280, 200, 16)
		}
		s.PushLayer(BlendNormal, 1, clip)
		c := panelColors[i]
		s.Fill(FillNonZero, IdentityAffine(), SolidBrush(c),
			NewRoundedRectShape(pos[0], pos[1], 240, 160, 12))
		// Gauge: stroked circle plus filled center.
		s.Stroke(DefaultStrokeStyle(), IdentityAffine(),
			SolidBrush(gg.RGBA{R: 1, G: 1, B: 1, A: 0.9}),
			NewCircleShape(pos[0]+120, pos[1]+80, 46))
		s.Fill(FillNonZero, IdentityAffine(),
			SolidBrush(gg.RGBA{R: 1, G: 1, B: 1, A: 0.85}),
			NewCircleShape(pos[0]+120, pos[1]+80, 26))
		// A few indicator bars.
		for bar := 0; bar < 4; bar++ {
			bx := pos[0] + 20 + float32(bar)*50
			s.Fill(FillNonZero, IdentityAffine(),
				SolidBrush(gg.RGBA{R: 0.1, G: 0.1, B: 0.15, A: 0.8}),
				NewRectShape(bx, pos[1]+130, 36, 18))
		}
		s.PopLayer()
	}

	// Dynamic root markers (cheap, redrawn every frame in every mode).
	for i := 0; i < 8; i++ {
		x := float32(60 + i*120)
		s.Fill(FillNonZero, IdentityAffine(),
			SolidBrush(gg.RGBA{R: 1, G: 0.6, B: 0.1, A: 1}),
			NewCircleShape(x, 520, 7))
	}
	return s
}

// TestBenchmarkSceneShape sanity-checks the benchmark fixture so a
// regression in scene construction cannot silently invalidate comparisons.
func TestBenchmarkSceneShape(t *testing.T) {
	sc := buildBenchmarkMixedScene()
	enc := sc.Encoding()
	segs := segmentLayers(enc)
	if len(segs) != benchmarkStaticLayerCount {
		t.Fatalf("segments = %d, want %d", len(segs), benchmarkStaticLayerCount)
	}
	r := NewRenderer(1024, 1024, WithWorkers(2), WithLayerCacheBudget(64*1024*1024))
	defer r.Close()
	if err := r.Render(gg.NewPixmap(1024, 1024), sc); err != nil {
		t.Fatal(err)
	}
	st := r.PixelCacheStats()
	if st.Rebuilds != benchmarkStaticLayerCount || st.Hits != 0 {
		t.Fatalf("cold frame stats wrong: %+v", st)
	}
	if err := r.Render(gg.NewPixmap(1024, 1024), sc); err != nil {
		t.Fatal(err)
	}
	st = r.PixelCacheStats()
	if st.Hits != benchmarkStaticLayerCount {
		t.Fatalf("second frame hits = %d, want %d", st.Hits, benchmarkStaticLayerCount)
	}
}
