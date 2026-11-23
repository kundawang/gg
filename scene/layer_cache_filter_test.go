package scene

import (
	"context"
	"testing"

	"github.com/gogpu/gg"
)

// buildSingleLayerScene builds one cacheable layer over a flat background.
func buildSingleLayerScene() *Scene {
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 0.95, G: 0.95, B: 0.95, A: 1}),
		NewRectShape(0, 0, 256, 256))
	s.PushLayer(BlendNormal, 1, nil)
	s.Fill(FillNonZero, IdentityAffine(),
		SolidBrush(gg.RGBA{R: 0.9, G: 0.2, B: 0.2, A: 1}),
		NewRectShape(60, 60, 120, 120))
	s.PopLayer()
	return s
}

func TestLayerCache_FilterChangesSignature(t *testing.T) {
	sc := buildSingleLayerScene()
	shared := NewLayerPixelCache(16 * 1024 * 1024)

	blurSpec := FilterSpec{Type: FilterBlur, Params: [4]float32{3}}
	rBlur := NewRenderer(256, 256, WithWorkers(2),
		WithLayerPixelCache(shared), WithLayerFilter(0, blurSpec))
	defer rBlur.Close()

	outBlur := gg.NewPixmap(256, 256)
	if err := rBlur.Render(outBlur, sc); err != nil {
		t.Fatal(err)
	}
	st := rBlur.PixelCacheStats()
	if st.Rebuilds != 1 {
		t.Fatalf("filtered cold frame rebuilds = %d, want 1", st.Rebuilds)
	}

	// Unfiltered renderer sharing the same cache must not hit the blur
	// entry: FilterHash is part of the signature.
	rPlain := NewRenderer(256, 256, WithWorkers(2), WithLayerPixelCache(shared))
	defer rPlain.Close()
	outPlain := gg.NewPixmap(256, 256)
	if err := rPlain.Render(outPlain, sc); err != nil {
		t.Fatal(err)
	}
	st = rPlain.PixelCacheStats()
	if st.Rebuilds != 2 {
		t.Fatalf("different filter signature must rebuild, total=%d want 2", st.Rebuilds)
	}
	if st.Entries != 2 {
		t.Fatalf("both filter variants should be resident, entries=%d", st.Entries)
	}
	// Blur must visibly differ from the unfiltered output.
	maxDelta, diffPixels := maxChannelDelta(outBlur.Data(), outPlain.Data())
	if maxDelta == 0 || diffPixels == 0 {
		t.Fatal("blur filter produced no visible change")
	}

	// Re-rendering with the identical blur spec must hit (stable pixels).
	outBlur2 := gg.NewPixmap(256, 256)
	if err := rBlur.Render(outBlur2, sc); err != nil {
		t.Fatal(err)
	}
	if _, diffPixels := maxChannelDelta(outBlur.Data(), outBlur2.Data()); diffPixels != 0 {
		t.Fatalf("identical filter spec must be byte-stable across frames, diffs=%d", diffPixels)
	}
}

func TestLayerCache_FilterParameterChangeRebuilds(t *testing.T) {
	sc := buildSingleLayerScene()
	cache := NewLayerPixelCache(16 * 1024 * 1024)

	r3 := NewRenderer(256, 256, WithWorkers(2),
		WithLayerPixelCache(cache),
		WithLayerFilter(0, FilterSpec{Type: FilterBlur, Params: [4]float32{3}}))
	defer r3.Close()
	_ = r3.Render(gg.NewPixmap(256, 256), sc)

	// Same filter type, different sigma: new signature, forced rebuild.
	r6 := NewRenderer(256, 256, WithWorkers(2),
		WithLayerPixelCache(cache),
		WithLayerFilter(0, FilterSpec{Type: FilterBlur, Params: [4]float32{6}}))
	defer r6.Close()
	_ = r6.Render(gg.NewPixmap(256, 256), sc)

	st := r6.PixelCacheStats()
	if st.Rebuilds != 2 {
		t.Fatalf("filter parameter change must rebuild, total=%d want 2", st.Rebuilds)
	}
}

func TestLayerCache_DropShadowAndColorMatrix(t *testing.T) {
	sc := buildSingleLayerScene()
	cache := NewLayerPixelCache(16 * 1024 * 1024)

	shadow := FilterSpec{
		Type:   FilterDropShadow,
		Params: [4]float32{4, 5, 2, 0},
		RGBA:   gg.RGBA{R: 0, G: 0, B: 0, A: 0.6},
	}
	matrix := FilterSpec{Type: FilterColorMatrix}
	// Identity-ish matrix: pass-through RGB, unchanged alpha.
	matrix.Matrix = [20]float32{
		1, 0, 0, 0, 0,
		0, 1, 0, 0, 0,
		0, 0, 1, 0, 0,
		0, 0, 0, 1, 0,
	}

	r := NewRenderer(256, 256, WithWorkers(2),
		WithLayerPixelCache(cache), WithLayerFilter(0, shadow, matrix))
	defer r.Close()

	out := gg.NewPixmap(256, 256)
	if err := r.Render(out, sc); err != nil {
		t.Fatal(err)
	}
	// Warmup + render round trip must succeed and cache exactly one entry.
	if _, err := r.WarmupLayers(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	st := r.PixelCacheStats()
	if st.Entries != 1 {
		t.Fatalf("chained filter layer entries = %d, want 1", st.Entries)
	}
}

func TestLayerCache_GeometryChangeInvalidates(t *testing.T) {
	// The same layer slot with changed shape coordinates must get a new
	// content hash and rebuild; untouched sibling layers stay hot.
	build := func(rectW float32) *Scene {
		s := NewScene()
		s.PushLayer(BlendNormal, 1, nil)
		s.Fill(FillNonZero, IdentityAffine(),
			SolidBrush(gg.RGBA{R: 0.2, G: 0.6, B: 0.9, A: 1}),
			NewRectShape(40, 40, rectW, 120))
		s.PopLayer()
		// A second, untouched layer.
		s.PushLayer(BlendNormal, 1, nil)
		s.Fill(FillNonZero, IdentityAffine(),
			SolidBrush(gg.RGBA{R: 0.9, G: 0.9, B: 0.2, A: 1}),
			NewCircleShape(200, 200, 24))
		s.PopLayer()
		return s
	}

	r := NewRenderer(256, 256, WithWorkers(2), WithLayerCacheBudget(16*1024*1024))
	defer r.Close()
	_ = r.Render(gg.NewPixmap(256, 256), build(120))

	// Change only layer 0's geometry; layer 1's signature is unaffected.
	_ = r.Render(gg.NewPixmap(256, 256), build(180))
	st := r.PixelCacheStats()
	if st.Rebuilds != 3 { // 2 cold + 1 precise rebuild
		t.Fatalf("geometry change rebuilds = %d, want 3 (sibling stays hot)", st.Rebuilds)
	}
	if st.Hits != 1 {
		t.Fatalf("untouched sibling must hit once, hits=%d", st.Hits)
	}
}

func TestLayerCache_ClipExpansionInvalidates(t *testing.T) {
	// Two layers whose only difference is the clip shape must get distinct
	// content signatures: clip geometry participates in the encoding hash.
	build := func(clip Shape) *Scene {
		s := NewScene()
		s.PushLayer(BlendNormal, 1, clip)
		s.Fill(FillNonZero, IdentityAffine(),
			SolidBrush(gg.RGBA{R: 0.2, G: 0.6, B: 0.9, A: 1}),
			NewRectShape(40, 40, 120, 120))
		s.PopLayer()
		return s
	}

	r := NewRenderer(256, 256, WithWorkers(2), WithLayerCacheBudget(16*1024*1024))
	defer r.Close()
	_ = r.Render(gg.NewPixmap(256, 256), build(NewRectShape(20, 20, 150, 150)))
	_ = r.Render(gg.NewPixmap(256, 256), build(NewRectShape(60, 60, 90, 90)))

	st := r.PixelCacheStats()
	if st.Rebuilds != 2 {
		t.Fatalf("clip change must force rebuild, total=%d want 2", st.Rebuilds)
	}
}
