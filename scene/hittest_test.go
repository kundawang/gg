package scene

import (
	"math"
	"sync"
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
	"golang.org/x/image/font/gofont/goregular"
)

// --- Fixtures --------------------------------------------------------------

func testSceneFont(t *testing.T) *text.FontSource {
	t.Helper()
	source, err := text.NewFontSource(goregular.TTF)
	if err != nil {
		t.Fatalf("NewFontSource: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source
}

func opaqueImage(w, h int) *Image {
	img := NewImage(w, h)
	img.Data = make([]byte, w*h*4)
	for i := 0; i < w*h; i++ {
		img.Data[i*4+0] = 200
		img.Data[i*4+3] = 255
	}
	return img
}

func transparentImage(w, h int) *Image {
	img := NewImage(w, h)
	img.Data = make([]byte, w*h*4) // all-zero alpha
	return img
}

// --- Basic fills -----------------------------------------------------------

func TestHitTest_FillContains(t *testing.T) {
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(10, 20, 100, 50))

	inside := s.HitTest(50, 40)
	if inside == nil {
		t.Fatal("expected a hit inside the rectangle")
	}
	if inside.Kind != HitKindFill {
		t.Errorf("Kind = %v, want %v", inside.Kind, HitKindFill)
	}
	if inside.LocalX != 50 || inside.LocalY != 40 {
		t.Errorf("Local = (%v,%v), want (50,40)", inside.LocalX, inside.LocalY)
	}

	if s.HitTest(200, 200) != nil {
		t.Error("expected no hit outside the rectangle")
	}
}

func TestHitTest_NegativeCoordinates(t *testing.T) {
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(-100, -100, 50, 50))

	if s.HitTest(-75, -75) == nil {
		t.Error("expected hit inside the negative-coordinate rectangle")
	}
	if s.HitTest(-99, -99) == nil {
		t.Error("expected hit near the negative-coordinate top-left corner")
	}
	if s.HitTest(0, 0) != nil {
		t.Error("expected no hit at the origin, outside the rectangle")
	}
}

func TestHitTest_BoundaryTangencyIsHit(t *testing.T) {
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 100, 100))

	// Corners, edges and interior all count; only the strict exterior misses.
	for _, p := range [][2]float32{
		{0, 0}, {100, 0}, {0, 100}, {100, 100}, // corners
		{50, 0}, {0, 50}, {100, 50}, {50, 100}, // edge midpoints
		{50, 50}, // interior
	} {
		if s.HitTest(p[0], p[1]) == nil {
			t.Errorf("tangent point %v should hit", p)
		}
	}
	for _, p := range [][2]float32{{-0.5, 50}, {100.5, 50}, {50, -0.5}, {50, 100.5}} {
		if s.HitTest(p[0], p[1]) != nil {
			t.Errorf("exterior point %v should miss", p)
		}
	}
}

func TestHitTest_EmptyAndMoveOnlyPathsMiss(t *testing.T) {
	s := NewScene()

	// Empty path: Fill returns before encoding anything.
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewPathShape(NewPath()))

	// MoveTo-only path: encoded, but has zero winding and no boundary.
	p := NewPath()
	p.MoveTo(50, 50)
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewPathShape(p))

	if hit := s.HitTest(50, 50); hit != nil {
		t.Errorf("move-only/degenerate path should not hit, got %+v", hit)
	}
}

func TestHitTest_FillRules(t *testing.T) {
	// Outer square with a same-winding inner square: non-zero fills it,
	// even-odd punches a hole.
	build := func(style FillStyle) *Scene {
		p := NewPath()
		p.Rectangle(0, 0, 100, 100)
		p.Rectangle(25, 25, 50, 50)
		s := NewScene()
		s.Fill(style, IdentityAffine(), SolidBrush(gg.Red), NewPathShape(p))
		return s
	}

	nonZero := build(FillNonZero)
	if nonZero.HitTest(50, 50) == nil {
		t.Error("non-zero should fill the nested region")
	}

	evenOdd := build(FillEvenOdd)
	if evenOdd.HitTest(50, 50) != nil {
		t.Error("even-odd should punch a hole in the nested region")
	}
	if evenOdd.HitTest(10, 10) == nil {
		t.Error("even-odd should still fill the outer band")
	}
}

func TestHitTest_RoundRect(t *testing.T) {
	s := NewScene()
	rect := Rect{MinX: 0, MinY: 0, MaxX: 100, MaxY: 100}
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRoundRectShape(rect, 20, 20))

	if s.HitTest(50, 50) == nil {
		t.Error("center of rounded rect should hit")
	}
	// The sharp corner is outside the rounded shape.
	if s.HitTest(1, 1) != nil {
		t.Error("corner of rounded rect should miss")
	}
}

// --- Overlap and ordering --------------------------------------------------

func TestHitTest_OverlappingPathsOrder(t *testing.T) {
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 100, 100))
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Blue), NewRectShape(50, 50, 100, 100))

	both := s.HitTestAll(75, 75)
	if len(both) != 2 {
		t.Fatalf("HitTestAll returned %d hits, want 2", len(both))
	}
	// Topmost first: the later-drawn blue square.
	if both[0].ID <= both[1].ID {
		t.Errorf("hits not topmost-first: IDs %d, %d", both[0].ID, both[1].ID)
	}

	top := s.HitTest(75, 75)
	if top == nil || top.ID != both[0].ID {
		t.Errorf("HitTest ID = %v, want topmost %d", top, both[0].ID)
	}

	single := s.HitTestAll(10, 10)
	if len(single) != 1 {
		t.Errorf("expected exactly one hit at (10,10), got %d", len(single))
	}
	if len(s.HitTestAll(300, 300)) != 0 {
		t.Error("expected no hits far outside")
	}
}

func TestHitTest_IDsAreTagIndices(t *testing.T) {
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 10, 10))

	hit := s.HitTest(5, 5)
	if hit == nil {
		t.Fatal("expected a hit")
	}
	enc := s.Encoding()
	if int(hit.ID) >= len(enc.Tags()) || enc.Tags()[hit.ID] != TagFill {
		t.Errorf("hit ID %d does not index a TagFill in the tag stream", hit.ID)
	}
}

// --- Transforms and local coordinates --------------------------------------

func TestHitTest_TransformedLocalCoordinates(t *testing.T) {
	s := NewScene()
	tf := TranslateAffine(100, 100).Multiply(RotateAffine(math.Pi / 2))
	s.Fill(FillNonZero, tf, SolidBrush(gg.Red), NewRectShape(0, 0, 20, 10))

	// Local (10, 5) maps to world: R90(10,5) + (100,100) = (-5,10)+(100,100).
	wx, wy := tf.TransformPoint(10, 5)
	hit := s.HitTest(wx, wy)
	if hit == nil {
		t.Fatalf("expected hit at world (%.2f, %.2f)", wx, wy)
	}
	if math.Abs(float64(hit.LocalX-10)) > 1e-3 || math.Abs(float64(hit.LocalY-5)) > 1e-3 {
		t.Errorf("Local = (%v,%v), want (10,5)", hit.LocalX, hit.LocalY)
	}
}

func TestHitTest_SingularTransformDoesNotHitOrPanic(t *testing.T) {
	s := NewScene()
	s.Fill(FillNonZero, ScaleAffine(0, 0), SolidBrush(gg.Red), NewRectShape(0, 0, 100, 100))
	if hit := s.HitTest(10, 10); hit != nil {
		t.Errorf("singular transform should not hit, got %+v", hit)
	}
}

// --- Scene origin ----------------------------------------------------------

func TestHitTest_SceneOrigin(t *testing.T) {
	s := NewScene()
	s.SetOrigin(30, 40)
	if x, y := s.Origin(); x != 30 || y != 40 {
		t.Fatalf("Origin = (%v,%v), want (30,40)", x, y)
	}
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 10, 10))

	// Query points are in parent space.
	if s.HitTest(35, 45) == nil {
		t.Error("expected hit at origin-shifted point (35,45)")
	}
	if s.HitTest(5, 5) != nil {
		t.Error("expected no hit at unshifted (5,5)")
	}

	// The origin must not alter geometry or bounds.
	if b := s.Bounds(); b.MinX != 0 || b.MinY != 0 || b.MaxX != 10 || b.MaxY != 10 {
		t.Errorf("bounds changed by origin: %+v", b)
	}
}

// --- Clipping --------------------------------------------------------------

func TestHitTest_RectClip(t *testing.T) {
	s := NewScene()
	s.PushClip(NewRectShape(50, 50, 100, 100))
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 200, 200))
	s.PopClip()

	if s.HitTest(75, 75) == nil {
		t.Error("expected hit inside the clip")
	}
	if s.HitTest(25, 25) != nil {
		t.Error("expected miss outside the clip even though the fill covers it")
	}
}

func TestHitTest_NestedClipIntersection(t *testing.T) {
	s := NewScene()
	s.PushClip(NewRectShape(0, 0, 100, 100))
	s.PushClip(NewRectShape(50, 50, 100, 100))
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 200, 200))
	s.PopClip()
	s.PopClip()

	if s.HitTest(75, 75) == nil {
		t.Error("expected hit in the intersection of both clips")
	}
	if s.HitTest(25, 25) != nil {
		t.Error("expected miss outside the second (inner) clip")
	}
	if s.HitTest(125, 125) != nil {
		t.Error("expected miss outside the first (outer) clip")
	}
}

func TestHitTest_NonRectClipUsesGeometry(t *testing.T) {
	s := NewScene()
	s.PushClip(NewCircleShape(50, 50, 20))
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 100, 100))
	s.PopClip()

	if s.HitTest(50, 50) == nil {
		t.Error("expected hit at the circle center")
	}
	// (35,35) is inside the circle's bounding box but outside the circle.
	if s.HitTest(35, 35) != nil {
		t.Error("expected miss outside the clip circle geometry")
	}
}

func TestHitTest_EmptyEncodedClipBlocksEverything(t *testing.T) {
	// Scene.PushClip skips empty shapes (the renderer would too), so exercise
	// the encoding path directly: BeginClip with no preceding path clips all.
	enc := NewEncoding()
	enc.EncodeBeginClip()
	enc.EncodeFill(SolidBrush(gg.Red), FillNonZero)
	enc.EncodeEndClip()

	if enc.HitTest(50, 50) != nil {
		t.Error("a BeginClip with no path should block all hits")
	}
}

func TestHitTest_SceneEmptyClipShapeIsNotEncoded(t *testing.T) {
	// Mirrors the renderer: an empty clip shape never becomes a clip command, so
	// it does not affect hits either.
	s := NewScene()
	s.PushClip(NewPathShape(NewPath()))
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 100, 100))
	s.PopClip()

	if s.HitTest(50, 50) == nil {
		t.Error("an unencoded empty clip should not block hits")
	}
}

// --- Stroke ----------------------------------------------------------------

func TestHitTest_StrokeWidthAndEndpoints(t *testing.T) {
	style := &StrokeStyle{Width: 10, MiterLimit: 10, Cap: LineCapButt, Join: LineJoinMiter}
	s := NewScene()
	s.Stroke(style, IdentityAffine(), SolidBrush(gg.Black), NewLineShape(0, 0, 100, 0))

	// On the segment.
	if s.HitTest(50, 0) == nil {
		t.Error("expected hit on the stroke centerline")
	}
	// Inside the half-width band.
	if s.HitTest(50, 4) == nil {
		t.Error("expected hit within half the stroke width")
	}
	// Outside the band.
	if s.HitTest(50, 6) != nil {
		t.Error("expected miss beyond half the stroke width")
	}
	// Butt cap: beyond the endpoint is a miss.
	if s.HitTest(106, 0) != nil {
		t.Error("butt cap should not extend past the endpoint")
	}
}

func TestHitTest_StrokeRoundCapExtends(t *testing.T) {
	style := &StrokeStyle{Width: 10, MiterLimit: 10, Cap: LineCapRound, Join: LineJoinRound}
	s := NewScene()
	s.Stroke(style, IdentityAffine(), SolidBrush(gg.Black), NewLineShape(0, 0, 100, 0))

	if s.HitTest(104, 0) == nil {
		t.Error("round cap should reach beyond the endpoint")
	}
	if s.HitTest(108, 0) != nil {
		t.Error("round cap should not reach 8px past a 5px radius endpoint")
	}
}

func TestHitTest_StrokeScaledByTransform(t *testing.T) {
	style := &StrokeStyle{Width: 2, MiterLimit: 10, Cap: LineCapButt, Join: LineJoinMiter}
	s := NewScene()
	// Scale 10x: device stroke width becomes 20 local units.
	s.Stroke(style, ScaleAffine(10, 10), SolidBrush(gg.Black), NewLineShape(0, 0, 10, 0))

	// Local point (5, 0.9) is within half the scaled width (10 units).
	hit := s.HitTest(50, 9)
	if hit == nil {
		t.Fatal("expected hit within the transform-scaled stroke band")
	}
	if math.Abs(float64(hit.LocalX-5)) > 1e-3 || math.Abs(float64(hit.LocalY-0.9)) > 1e-3 {
		t.Errorf("Local = (%v,%v), want (5,0.9)", hit.LocalX, hit.LocalY)
	}
}

// --- Images ----------------------------------------------------------------

func TestHitTest_ImageRectangleAndLocalSpace(t *testing.T) {
	s := NewScene()
	img := opaqueImage(20, 10)
	tf := TranslateAffine(100, 50).Multiply(ScaleAffine(2, 2)) // 40x20 at (100,50)
	s.DrawImage(img, tf)

	hit := s.HitTest(100+20, 50+10) // image (10,5) scaled by 2
	if hit == nil {
		t.Fatal("expected hit inside the image")
	}
	if hit.Kind != HitKindImage {
		t.Errorf("Kind = %v, want Image", hit.Kind)
	}
	if math.Abs(float64(hit.LocalX-10)) > 1e-3 || math.Abs(float64(hit.LocalY-5)) > 1e-3 {
		t.Errorf("Local = (%v,%v), want (10,5) in image space", hit.LocalX, hit.LocalY)
	}
	if s.HitTest(100+41, 50+10) != nil {
		t.Error("expected miss just outside the image")
	}
}

func TestHitTest_TransparentImage(t *testing.T) {
	s := NewScene()
	img := transparentImage(10, 10)
	s.DrawImage(img, IdentityAffine())

	if s.HitTest(5, 5) != nil {
		t.Error("fully transparent image should miss by default")
	}
	hit := s.HitTestWithOptions(5, 5, HitOptions{IncludeTransparentImages: true})
	if hit == nil {
		t.Error("IncludeTransparentImages should make the transparent image hittable")
	}
}

func TestHitTest_PartiallyTransparentImage(t *testing.T) {
	s := NewScene()
	img := transparentImage(10, 10)
	// Opaque only in the top-left 5x5 quadrant.
	for y := 0; y < 5; y++ {
		for x := 0; x < 5; x++ {
			img.Data[(y*10+x)*4+3] = 255
		}
	}
	s.DrawImage(img, IdentityAffine())

	if s.HitTest(2, 2) == nil {
		t.Error("opaque pixel should hit")
	}
	if s.HitTest(8, 8) != nil {
		t.Error("transparent pixel should miss")
	}
}

// --- Text ------------------------------------------------------------------

func TestHitTest_RotatedText(t *testing.T) {
	source := testSceneFont(t)
	face := source.Face(20)
	parsed := source.Parsed()

	gid := parsed.GlyphIndex('H')
	gb := parsed.GlyphBounds(gid, 20)
	if gb.Empty() {
		t.Fatal("font reported empty bounds for 'H'")
	}
	// Pick the glyph box center in glyph space.
	gx := float32((gb.MinX + gb.MaxX) / 2)
	gy := float32((gb.MinY + gb.MaxY) / 2)

	const angle = math.Pi / 2
	tf := TranslateAffine(100, 100).Multiply(RotateAffine(angle))

	s := NewScene()
	s.PushTransform(tf)
	if err := s.DrawText("H", face, 0, 0, SolidBrush(gg.Black)); err != nil {
		t.Fatalf("DrawText: %v", err)
	}
	s.PopTransform()

	wx, wy := tf.TransformPoint(gx, gy)
	hit := s.HitTest(wx, wy)
	if hit == nil {
		t.Fatalf("expected hit on rotated glyph at world (%.2f,%.2f)", wx, wy)
	}
	if hit.Kind != HitKindText {
		t.Errorf("Kind = %v, want Text", hit.Kind)
	}
	if math.Abs(float64(hit.LocalX-gx)) > 0.5 || math.Abs(float64(hit.LocalY-gy)) > 0.5 {
		t.Errorf("Local = (%v,%v), want glyph-space (%v,%v)", hit.LocalX, hit.LocalY, gx, gy)
	}
}

func TestHitTest_TextOriginOffset(t *testing.T) {
	source := testSceneFont(t)
	face := source.Face(20)
	parsed := source.Parsed()

	gid := parsed.GlyphIndex('M')
	gb := parsed.GlyphBounds(gid, 20)
	if gb.Empty() {
		t.Skip("font has no bounds for 'M'")
	}
	gx := float32((gb.MinX + gb.MaxX) / 2)
	gy := float32((gb.MinY + gb.MaxY) / 2)

	s := NewScene()
	const ox, oy = 40, 60
	if err := s.DrawText("M", face, ox, oy, SolidBrush(gg.Black)); err != nil {
		t.Fatalf("DrawText: %v", err)
	}

	// World point = origin + glyph-space point. Local reported by the hit is
	// glyph-run space (glyph position + origin), per the documented contract.
	hit := s.HitTest(ox+gx, oy+gy)
	if hit == nil {
		t.Fatal("expected hit inside the text run")
	}
	if math.Abs(float64(hit.LocalX-(ox+gx))) > 0.5 || math.Abs(float64(hit.LocalY-(oy+gy))) > 0.5 {
		t.Errorf("Local = (%v,%v), want (%v,%v)", hit.LocalX, hit.LocalY, ox+gx, oy+gy)
	}

	// Far away is a miss.
	if s.HitTest(ox+gx+500, oy+gy) != nil {
		t.Error("expected miss far outside the text run")
	}
}

func TestHitTest_TextWithoutFontFallsBackToAdvance(t *testing.T) {
	enc := NewEncoding()
	enc.brushes = append(enc.brushes, SolidBrush(gg.Black))
	run := GlyphRunData{
		FontSize: 10, GlyphCount: 1, OriginX: 0, OriginY: 0, BrushIndex: 0, TextLen: 0,
	}
	enc.EncodeText(run, []GlyphEntry{{GlyphID: 1, X: 0, Y: 0}}, "")

	// No font registry: the fallback box spans the glyph advance.
	if enc.HitTest(1, -1) == nil {
		t.Error("expected fallback advance box to be hittable")
	}
	if enc.HitTest(1000, 0) != nil {
		t.Error("expected miss outside the fallback box")
	}
}

// --- Filters ---------------------------------------------------------------

type growFilter struct{ amount float32 }

func (f growFilter) Apply(_, _ *gg.Pixmap, _ Rect) {}
func (f growFilter) ExpandBounds(in Rect) Rect {
	return Rect{
		MinX: in.MinX - f.amount,
		MinY: in.MinY - f.amount,
		MaxX: in.MaxX + f.amount,
		MaxY: in.MaxY + f.amount,
	}
}

func TestHitTest_FilterExpandsLayerBounds(t *testing.T) {
	s := NewScene()
	chain := NewFilterChain(growFilter{amount: 10})
	s.PushLayerFiltered(BlendNormal, 1, nil, chain)
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(50, 50, 10, 10))
	s.PopLayer()

	// Inside the shape.
	if s.HitTest(55, 55) == nil {
		t.Error("expected hit on the shape")
	}
	// Inside the filter-grown halo but outside the shape.
	hit := s.HitTest(45, 55)
	if hit == nil {
		t.Error("expected hit in the filter-expanded region")
	}
	// Far outside the halo.
	if s.HitTest(20, 55) != nil {
		t.Error("expected miss outside the expanded halo")
	}
}

func TestHitTest_FilterDoesNotChangeEncodingHash(t *testing.T) {
	plain := NewScene()
	plain.PushLayer(BlendNormal, 1, nil)
	plain.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 10, 10))
	plain.PopLayer()

	filtered := NewScene()
	filtered.PushLayerFiltered(BlendNormal, 1, nil, NewFilterChain(growFilter{amount: 10}))
	filtered.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 10, 10))
	filtered.PopLayer()

	if plain.Encoding().Hash() != filtered.Encoding().Hash() {
		t.Error("filter metadata must not alter the render stream hash")
	}
}

// --- Robustness ------------------------------------------------------------

func TestHitTest_NonFinitePoints(t *testing.T) {
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 100, 100))

	nan := float32(math.NaN())
	inf := float32(math.Inf(1))
	cases := [][2]float32{{nan, 50}, {50, nan}, {inf, 50}, {50, inf}}
	for _, c := range cases {
		if got := s.HitTest(c[0], c[1]); got != nil {
			t.Errorf("non-finite point %v should not hit, got %+v", c, got)
		}
		if got := s.HitTestAll(c[0], c[1]); got != nil {
			t.Errorf("non-finite point %v should yield no hits", c)
		}
	}
}

func TestHitTest_DeepNestingDoesNotPanic(t *testing.T) {
	s := NewScene()
	const depth = 600
	for i := 0; i < depth; i++ {
		s.PushClip(NewRectShape(-1000, -1000, 5000, 5000))
	}
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 100, 100))
	for i := 0; i < depth; i++ {
		s.PopClip()
	}
	for i := 0; i < depth; i++ {
		s.PushLayer(BlendNormal, 1, nil)
	}
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 100, 100))
	for i := 0; i < depth; i++ {
		s.PopLayer()
	}

	if s.HitTest(50, 50) == nil {
		t.Error("expected hit despite deep nesting")
	}
}

func TestHitTest_QueryDoesNotModifyScene(t *testing.T) {
	source := testSceneFont(t)
	face := source.Face(14)

	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 100, 100))
	s.PushLayer(BlendMultiply, 0.5, NewCircleShape(50, 50, 30))
	s.Fill(FillNonZero, TranslateAffine(10, 10), SolidBrush(gg.Blue), NewCircleShape(50, 50, 20))
	s.DrawImage(opaqueImage(8, 8), TranslateAffine(5, 5))
	if err := s.DrawText("Hi", face, 20, 80, SolidBrush(gg.Black)); err != nil {
		t.Fatalf("DrawText: %v", err)
	}
	s.PopLayer()

	enc := s.Encoding()
	hashBefore := enc.Hash()
	tagsBefore := append([]Tag(nil), enc.Tags()...)
	pathBefore := append([]float32(nil), enc.PathData()...)
	drawBefore := append([]uint32(nil), enc.DrawData()...)
	textBefore := append([]byte(nil), enc.TextData()...)
	transBefore := append([]Affine(nil), enc.Transforms()...)
	boundsBefore := enc.Bounds()
	versionBefore := s.Version()
	taggedBefore := s.TaggedBounds()

	_ = s.HitTestAll(50, 50)
	_ = s.HitTest(50, 50)
	_ = s.HitTestAllWithOptions(50, 50, HitOptions{IncludeTransparentImages: true})

	if got := enc.Hash(); got != hashBefore {
		t.Error("encoding hash changed after hit queries")
	}
	if !equalTags(enc.Tags(), tagsBefore) {
		t.Error("tag order changed after hit queries")
	}
	if !equalFloats(enc.PathData(), pathBefore) {
		t.Error("path data changed after hit queries")
	}
	if !equalUint32s(enc.DrawData(), drawBefore) {
		t.Error("draw data changed after hit queries")
	}
	if len(enc.TextData()) != len(textBefore) {
		t.Error("text data length changed after hit queries")
	}
	if len(enc.Transforms()) != len(transBefore) {
		t.Error("transform stream changed after hit queries")
	}
	if enc.Bounds() != boundsBefore {
		t.Error("encoding bounds changed after hit queries")
	}
	if s.Version() != versionBefore {
		t.Error("scene version changed after hit queries")
	}
	if len(s.TaggedBounds()) != len(taggedBefore) {
		t.Error("command bounds count changed after hit queries")
	}
}

func TestHitTest_ConcurrentQueries(t *testing.T) {
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 100, 100))
	s.Fill(FillNonZero, TranslateAffine(20, 20), SolidBrush(gg.Blue), NewCircleShape(50, 50, 20))
	s.PushClip(NewRectShape(10, 10, 80, 80))
	s.Stroke(&StrokeStyle{Width: 4, MiterLimit: 10}, IdentityAffine(), SolidBrush(gg.Black), NewLineShape(0, 100, 100, 0))
	s.PopClip()

	tester := NewHitTesterWithResources(s.Encoding(), s.Images(), s.FontRegistry())
	want := tester.HitAll(50, 50)

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make(chan string, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				got := tester.HitAll(50, 50)
				if len(got) != len(want) {
					errs <- "concurrent hit count mismatch"
					return
				}
				for k := range got {
					if got[k].ID != want[k].ID || got[k].Kind != want[k].Kind {
						errs <- "concurrent hit order mismatch"
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// --- Encoding-only tester --------------------------------------------------

func TestHitTest_EncodingOnlyTesterIgnoresImagesAndFonts(t *testing.T) {
	enc := NewEncoding()
	enc.brushes = append(enc.brushes, SolidBrush(gg.Black))
	enc.EncodeImage(0, IdentityAffine())
	enc.EncodeText(GlyphRunData{FontSize: 10, GlyphCount: 1, TextLen: 0},
		[]GlyphEntry{{GlyphID: 1, X: 0, Y: 0}}, "")

	// Without resources: image is skipped, text falls back to the advance box.
	hits := enc.HitTestAll(5, -5)
	if len(hits) != 1 || hits[0].Kind != HitKindText {
		t.Errorf("expected only the text fallback hit, got %+v", hits)
	}
}

func TestHitTest_SceneOpenLayerIsVisibleToQuery(t *testing.T) {
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 10, 10))
	s.PushLayer(BlendNormal, 1, nil)
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Blue), NewRectShape(40, 40, 10, 10))
	// Do not pop: the open layer's content must still be hittable.

	if s.HitTest(5, 5) == nil {
		t.Error("expected hit on root content")
	}
	if s.HitTest(45, 45) == nil {
		t.Error("expected hit on open-layer content")
	}
}

// --- Helpers ---------------------------------------------------------------

func equalTags(a, b []Tag) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalFloats(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalUint32s(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Side table propagation ------------------------------------------------

func TestHitTest_FilterExpansionSurvivesAppend(t *testing.T) {
	child := NewScene()
	child.PushLayerFiltered(BlendNormal, 1, nil, NewFilterChain(growFilter{amount: 8}))
	child.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(50, 50, 10, 10))
	child.PopLayer()

	parent := NewScene()
	parent.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Blue), NewRectShape(0, 0, 5, 5))
	parent.Append(child)

	// Same query semantics as the standalone child.
	if parent.HitTest(45, 55) == nil {
		t.Error("expected filter halo to survive Append")
	}
	if parent.HitTest(20, 55) != nil {
		t.Error("expected miss outside the halo after Append")
	}
}

func TestHitTest_FilterExpansionSurvivesClone(t *testing.T) {
	s := NewScene()
	s.PushLayerFiltered(BlendNormal, 1, nil, NewFilterChain(growFilter{amount: 8}))
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(50, 50, 10, 10))
	s.PopLayer()

	clone := s.Encoding().Clone()
	if clone.HitTest(45, 55) == nil {
		t.Error("expected filter halo to survive Clone")
	}
	if len(clone.LayerExpand()) != 1 {
		t.Errorf("LayerExpand len = %d, want 1", len(clone.LayerExpand()))
	}
}

func TestHitTest_OriginResetBySceneReset(t *testing.T) {
	s := NewScene()
	s.SetOrigin(10, 20)
	s.Reset()
	if x, y := s.Origin(); x != 0 || y != 0 {
		t.Errorf("origin after Reset = (%v,%v), want (0,0)", x, y)
	}
}

func TestHitTest_StrokeJoinExtendsAtCorner(t *testing.T) {
	style := &StrokeStyle{Width: 10, MiterLimit: 10, Cap: LineCapButt, Join: LineJoinMiter}
	p := NewPath()
	p.MoveTo(0, 0)
	p.LineTo(50, 0)
	p.LineTo(50, 50)
	s := NewScene()
	s.Stroke(style, IdentityAffine(), SolidBrush(gg.Black), NewPathShape(p))

	// A miter join fills the outer corner just past (50,0).
	if s.HitTest(53, -3) == nil {
		t.Error("miter join should cover the outer corner")
	}
	if s.HitTest(60, -10) != nil {
		t.Error("expected miss well outside the join")
	}
}

func TestHitTest_TruncatedStreamsDoNotPanic(t *testing.T) {
	// Build a valid encoding, then truncate each stream at every prefix length.
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 100, 100))
	s.Stroke(&StrokeStyle{Width: 4, MiterLimit: 10}, TranslateAffine(5, 5),
		SolidBrush(gg.Black), NewLineShape(0, 0, 50, 50))
	s.PushLayer(BlendNormal, 1, NewCircleShape(50, 50, 20))
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Blue), NewCircleShape(50, 50, 10))
	s.PopLayer()
	base := s.Encoding()

	for tlen := 0; tlen <= len(base.tags); tlen++ {
		e := &Encoding{
			tags:       base.tags[:tlen],
			pathData:   base.pathData,
			drawData:   base.drawData,
			transforms: base.transforms,
			textData:   base.textData,
			brushes:    base.brushes,
			bounds:     base.bounds,
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic at tag prefix %d: %v", tlen, r)
				}
			}()
			_ = e.HitTestAll(25, 25)
		}()
	}

	// Truncate the data streams while keeping the tag stream intact.
	for plen := 0; plen <= len(base.pathData); plen++ {
		e := &Encoding{
			tags:        base.tags,
			pathData:    base.pathData[:plen],
			drawData:    base.drawData,
			transforms:  base.transforms,
			textData:    base.textData,
			brushes:     base.brushes,
			bounds:      base.bounds,
			layerExpand: base.layerExpand,
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic at path-data prefix %d: %v", plen, r)
				}
			}()
			_ = e.HitTestAll(25, 25)
		}()
	}
}

func TestHitTest_TransparentLayerIsNotPickable(t *testing.T) {
	s := NewScene()
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Red), NewRectShape(0, 0, 10, 10))
	s.PushLayer(BlendNormal, 0, nil) // fully transparent
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Blue), NewRectShape(40, 40, 10, 10))
	s.PopLayer()

	if s.HitTest(5, 5) == nil {
		t.Error("root content should still hit")
	}
	if s.HitTest(45, 45) != nil {
		t.Error("content in a fully transparent layer should not be pickable")
	}
}

func TestHitTest_DestinationBlendLayerIsNotPickable(t *testing.T) {
	s := NewScene()
	s.PushLayer(BlendDestination, 1, nil)
	s.Fill(FillNonZero, IdentityAffine(), SolidBrush(gg.Blue), NewRectShape(40, 40, 10, 10))
	s.PopLayer()

	if s.HitTest(45, 45) != nil {
		t.Error("content in a destination-blend layer should not be pickable")
	}
}
