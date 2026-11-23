package filter

import (
	"context"
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/scene"
)

// pixelsEqual compares two pixmaps byte for byte, reporting the first
// differing offset.
func pixelsEqual(a, b *gg.Pixmap) (bool, int) {
	if a.Width() != b.Width() || a.Height() != b.Height() {
		return false, -1
	}
	ad, bd := a.Data(), b.Data()
	for i := range ad {
		if ad[i] != bd[i] {
			return false, i
		}
	}
	return true, -1
}

// buildPixmapImage creates an opaque image with a bright rectangle on a dark
// background so blur and color matrix both have visible, non-uniform effects.
func buildPixmapImage(w, h int) *gg.Pixmap {
	pm := gg.NewPixmap(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := gg.RGBA2(0.05, 0.05, 0.1, 1)
			if x >= w/4 && x < 3*w/4 && y >= h/4 && y < 3*h/4 {
				c = gg.RGBA2(0.9, 0.6, 0.2, 1)
			}
			pm.SetPixel(x, y, c)
		}
	}
	return pm
}

// TestGraphColorMatrixMatchesChain checks that a non-expanding filter in a
// graph produces exactly the same pixels as the direct FilterChain path,
// because neither path shifts the origin.
func TestGraphColorMatrixMatchesChain(t *testing.T) {
	const size = 24
	src := buildPixmapImage(size, size)
	bounds := scene.Rect{MinX: 0, MinY: 0, MaxX: size, MaxY: size}

	chain := scene.NewFilterChain(NewGrayscaleFilter())
	dst := gg.NewPixmap(size, size)
	chain.Apply(src, dst, bounds)

	g := scene.NewFilterGraph()
	if err := g.AddSource("src"); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if err := g.AddNode(scene.FilterNode{
		ID:     "gray",
		Op:     scene.FilterOp{Filter: NewGrayscaleFilter()},
		Inputs: []scene.FilterResourceID{"src"},
		Output: "out",
	}); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	res, err := cg.Execute(context.Background(), map[scene.FilterResourceID]*gg.Pixmap{"src": src},
		scene.FilterGraphOptions{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := res.Output("out")
	if got.Width() != size || got.Height() != size {
		t.Fatalf("graph output %dx%d, want %dx%d", got.Width(), got.Height(), size, size)
	}
	if res.Origin != [2]int{0, 0} {
		t.Fatalf("Origin = %v, want (0,0) for a non-expanding filter", res.Origin)
	}
	if ok, off := pixelsEqual(dst, got); !ok {
		t.Fatalf("graph color matrix differs from chain at byte offset %d", off)
	}
}

// TestGraphBlurExpandsAndPreservesContent verifies that a bounds-expanding
// filter grows the buffer, moves the origin, and keeps the source content at
// the expected offset.
func TestGraphBlurExpandsAndPreservesContent(t *testing.T) {
	const size = 32
	src := buildPixmapImage(size, size)
	const radius = 2.0
	expand := int(radius * 3) // blur expands by ceil(radius*3)

	g := scene.NewFilterGraph()
	_ = g.AddSource("src")
	if err := g.AddNode(scene.FilterNode{
		ID: "blur", Op: scene.FilterOp{Filter: NewBlurFilter(radius)},
		Inputs: []scene.FilterResourceID{"src"}, Output: "out",
	}); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	res, err := cg.Execute(context.Background(), map[scene.FilterResourceID]*gg.Pixmap{"src": src},
		scene.FilterGraphOptions{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output("out")
	want := size + 2*expand
	if out.Width() != want || out.Height() != want {
		t.Fatalf("blur output %dx%d, want %dx%d", out.Width(), out.Height(), want, want)
	}
	if res.Origin != [2]int{-expand, -expand} {
		t.Fatalf("Origin = %v, want (%d,%d)", res.Origin, -expand, -expand)
	}

	// A blurred bright pixel must still be far brighter than the dark
	// background at the source content's shifted position.
	bright := out.GetPixel(expand+size/2, expand+size/2)
	dark := out.GetPixel(0, 0)
	if bright.R <= dark.R+0.1 {
		t.Fatalf("content not preserved at offset %d: bright=%+v dark=%+v", expand, bright, dark)
	}
}

// TestGraphParallelBranchesMatchSerial checks a diamond graph with real
// filters. Worker counts above one must reproduce the serial (workers=1)
// pixels exactly, and the merged result must differ from either branch alone.
func TestGraphParallelBranchesMatchSerial(t *testing.T) {
	const size = 24
	src := buildPixmapImage(size, size)

	g := scene.NewFilterGraph()
	_ = g.AddSource("src")
	mustNode(t, g, scene.FilterNode{
		ID: "blur", Op: scene.FilterOp{Filter: NewBlurFilter(1.5)},
		Inputs: []scene.FilterResourceID{"src"}, Output: "blurOut",
	})
	mustNode(t, g, scene.FilterNode{
		ID: "gray", Op: scene.FilterOp{Filter: NewGrayscaleFilter()},
		Inputs: []scene.FilterResourceID{"src"}, Output: "grayOut",
	})
	mustNode(t, g, scene.FilterNode{
		ID: "merge", Op: scene.OverOp(),
		Inputs: []scene.FilterResourceID{"blurOut", "grayOut"}, Output: "out",
	})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	sources := map[scene.FilterResourceID]*gg.Pixmap{"src": src}
	var baseline *gg.Pixmap
	for _, workers := range []int{1, 2, 4, 8} {
		res, err := cg.Execute(context.Background(), sources, scene.FilterGraphOptions{Workers: workers})
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		out := res.Output("out")
		if baseline == nil {
			baseline = out
			continue
		}
		if ok, off := pixelsEqual(baseline, out); !ok {
			t.Fatalf("workers=%d differs from serial baseline at byte offset %d", workers, off)
		}
	}

	// The merge output must not equal a single grayscale branch: the blurred
	// layer contributes on top.
	grayOnly := gg.NewPixmap(size, size)
	NewGrayscaleFilter().Apply(src, grayOnly, scene.Rect{MinX: 0, MinY: 0, MaxX: size, MaxY: size})
	if ok, _ := pixelsEqual(baseline, grayOnly); ok {
		t.Fatal("merged output unexpectedly identical to the grayscale branch")
	}
}

// TestGraphDropShadowExpandsOutput verifies that a drop shadow grows the
// output buffer and shifts the origin in the expected direction.
func TestGraphDropShadowExpandsOutput(t *testing.T) {
	src := gg.NewPixmap(16, 16)
	for y := 6; y < 10; y++ {
		for x := 6; x < 10; x++ {
			src.SetPixel(x, y, gg.White)
		}
	}

	shadow := NewSimpleDropShadow(2, 2, 1)
	g := scene.NewFilterGraph()
	_ = g.AddSource("src")
	mustNode(t, g, scene.FilterNode{
		ID: "shadow", Op: scene.FilterOp{Filter: shadow},
		Inputs: []scene.FilterResourceID{"src"}, Output: "out",
	})
	cg, err := g.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	res, err := cg.Execute(context.Background(), map[scene.FilterResourceID]*gg.Pixmap{"src": src}, scene.FilterGraphOptions{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output("out")
	if out.Width() <= 16 || out.Height() <= 16 {
		t.Fatalf("shadow output %dx%d did not expand", out.Width(), out.Height())
	}
	if res.Origin[0] >= 0 || res.Origin[1] >= 0 {
		t.Fatalf("Origin = %v, want negative on both axes (blur spreads left/up)", res.Origin)
	}

	// Original white square survives at its shifted position.
	ox, oy := -res.Origin[0], -res.Origin[1]
	c := out.GetPixel(ox+7, oy+7)
	if c.R < 0.9 || c.G < 0.9 || c.B < 0.9 {
		t.Fatalf("original content not preserved at (%d,%d): %+v", ox+7, oy+7, c)
	}
}

func mustNode(t *testing.T, g *scene.FilterGraph, n scene.FilterNode) {
	t.Helper()
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode %q: %v", n.ID, err)
	}
}
