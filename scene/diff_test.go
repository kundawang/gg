package scene

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gogpu/gg"
)

// ---------------------------------------------------------------------------
// Test fixture builders
// ---------------------------------------------------------------------------

func solidColor(r, g, b float64) Brush {
	return SolidBrush(gg.RGBA{R: r, G: g, B: b, A: 1})
}

// fillRectEnc builds an encoding containing one filled rectangle drawn at the
// given origin and color. Rectangles are built from explicit paths so the
// geometry is fully visible in the tag stream.
func fillRectEnc(x, y, w, h float32, brush Brush) *Encoding {
	enc := NewEncoding()
	p := gg.NewPath()
	p.MoveTo(float64(x), float64(y))
	p.LineTo(float64(x+w), float64(y))
	p.LineTo(float64(x+w), float64(y+h))
	p.LineTo(float64(x), float64(y+h))
	p.Close()
	enc.EncodePath(p)
	enc.EncodeFill(brush, FillNonZero)
	return enc
}

func textEnc(fontID uint64, size float32, brush Brush, str string) *Encoding {
	enc := NewEncoding()
	enc.brushes = append(enc.brushes, brush)
	run := GlyphRunData{
		FontSourceID: fontID,
		FontSize:     size,
		GlyphCount:   1,
		OriginX:      10,
		OriginY:      20,
		BrushIndex:   0,
		TextLen:      uint16(len(str)),
	}
	glyphs := []GlyphEntry{{GlyphID: 1, X: 0, Y: 0}}
	enc.EncodeText(run, glyphs, str)
	return enc
}

// ---------------------------------------------------------------------------
// 1. Nil inputs, empty scenes, no-mutation
// ---------------------------------------------------------------------------

func TestDiff_NilInputs(t *testing.T) {
	enc := NewEncoding()
	if _, err := DiffEncodings(nil, enc); !errors.Is(err, ErrNilEncoding) {
		t.Fatalf("nil old: err = %v, want ErrNilEncoding", err)
	}
	if _, err := DiffEncodings(enc, nil); !errors.Is(err, ErrNilEncoding) {
		t.Fatalf("nil next: err = %v, want ErrNilEncoding", err)
	}
	if _, err := DiffEncodings(nil, nil); !errors.Is(err, ErrNilEncoding) {
		t.Fatalf("both nil: err = %v, want ErrNilEncoding", err)
	}
	if _, err := DiffScenes(nil, NewScene()); !errors.Is(err, ErrNilEncoding) {
		t.Fatalf("nil scene: err = %v, want ErrNilEncoding", err)
	}
}

func TestDiff_EmptyScenes(t *testing.T) {
	// Both empty.
	d, err := DiffEncodings(NewEncoding(), NewEncoding())
	if err != nil {
		t.Fatalf("empty/empty: %v", err)
	}
	if !d.Empty() {
		t.Fatalf("empty/empty diff = %+v, want empty", d)
	}

	// Old empty, next has content -> all added.
	next := fillRectEnc(0, 0, 10, 10, solidColor(1, 0, 0))
	d, err = DiffEncodings(NewEncoding(), next)
	if err != nil {
		t.Fatalf("empty/next: %v", err)
	}
	if len(d.Added) != 1 || len(d.Removed) != 0 || len(d.Changed) != 0 {
		t.Fatalf("empty/next = add:%d rem:%d chg:%d, want 1/0/0",
			len(d.Added), len(d.Removed), len(d.Changed))
	}

	// Old has content, next empty -> all removed.
	d, err = DiffEncodings(next, NewEncoding())
	if err != nil {
		t.Fatalf("old/empty: %v", err)
	}
	if len(d.Removed) != 1 || len(d.Added) != 0 || len(d.Changed) != 0 {
		t.Fatalf("old/empty = add:%d rem:%d chg:%d, want 0/1/0",
			len(d.Added), len(d.Removed), len(d.Changed))
	}
}

func TestDiff_DoesNotMutateInputs(t *testing.T) {
	old := fillRectEnc(0, 0, 10, 10, solidColor(1, 0, 0))
	next := fillRectEnc(0, 0, 20, 20, solidColor(0, 1, 0))

	oldTags := append([]Tag(nil), old.Tags()...)
	oldPath := append([]float32(nil), old.PathData()...)
	oldDraw := append([]uint32(nil), old.DrawData()...)
	newTags := append([]Tag(nil), next.Tags()...)

	if _, err := DiffEncodings(old, next); err != nil {
		t.Fatalf("diff: %v", err)
	}
	assertSlicesEqual(t, "old.tags", old.Tags(), oldTags)
	assertSlicesEqual(t, "old.pathData", old.PathData(), oldPath)
	assertSlicesEqual(t, "old.drawData", old.DrawData(), oldDraw)
	assertSlicesEqual(t, "next.tags", next.Tags(), newTags)

	// Scene-level: snapshot path must not alter scene internals.
	s1, s2 := NewScene(), NewScene()
	s1.Fill(FillNonZero, IdentityAffine(), solidColor(1, 0, 0), NewRectShape(0, 0, 10, 10))
	s2.Fill(FillNonZero, IdentityAffine(), solidColor(0, 0, 1), NewRectShape(0, 0, 10, 10))
	versionBefore := s1.Version()
	tagLenBefore := len(s1.encoding.Tags())
	if _, err := DiffScenes(s1, s2); err != nil {
		t.Fatalf("DiffScenes: %v", err)
	}
	if s1.Version() != versionBefore {
		t.Errorf("scene version changed %d -> %d", versionBefore, s1.Version())
	}
	if len(s1.encoding.Tags()) != tagLenBefore {
		t.Errorf("scene root encoding modified: tags %d -> %d",
			tagLenBefore, len(s1.encoding.Tags()))
	}
}

func TestDiff_SceneSnapshotDoesNotFlattenOrMutate(t *testing.T) {
	// A scene with an open (unflattened) layer: DiffScenes must neither flatten
	// it nor disturb the layer stack, and must still see the layer's content.
	open := NewScene()
	open.PushLayer(BlendNormal, 1.0, nil)
	open.Fill(FillNonZero, IdentityAffine(), solidColor(1, 0, 0), NewRectShape(0, 0, 10, 10))
	open.PopLayer()
	open.PushLayer(BlendNormal, 1.0, nil)
	open.Fill(FillNonZero, IdentityAffine(), solidColor(0, 1, 0), NewRectShape(20, 20, 10, 10))

	depthBefore := open.LayerDepth()
	versionBefore := open.Version()

	flat := NewScene()
	flat.Fill(FillNonZero, IdentityAffine(), solidColor(0, 1, 0), NewRectShape(20, 20, 10, 10))
	d, err := DiffScenes(open, flat)
	if err != nil {
		t.Fatalf("DiffScenes: %v", err)
	}
	// The open layer's content must be visible through the snapshot: the red
	// Fill (0..10) is removed, while the green Fill (20..30) matches. Assert
	// the removed set includes that Fill and does not include the green one.
	var sawRedFill bool
	for _, r := range d.Removed {
		if r.Tag == TagFill && r.OldBounds.MaxX == 10 && r.OldBounds.MaxY == 10 {
			sawRedFill = true
		}
		if r.Tag == TagFill && r.OldBounds.MinX == 20 {
			t.Errorf("green fill (20..30) wrongly reported removed: %+v", r)
		}
	}
	if !sawRedFill {
		t.Errorf("open-layer red fill not visible in snapshot diff: %+v", d.Removed)
	}
	if open.LayerDepth() != depthBefore {
		t.Errorf("layer stack changed: depth %d -> %d", depthBefore, open.LayerDepth())
	}
	if open.Version() != versionBefore {
		t.Errorf("scene version changed %d -> %d", versionBefore, open.Version())
	}
}

func TestDiff_ErrorPathsDoNotMutateInputs(t *testing.T) {
	// A malformed new encoding must be rejected without touching either input.
	good := fillRectEnc(0, 0, 10, 10, solidColor(1, 0, 0))
	bad := NewEncoding()
	bad.tags = append(bad.tags, TagFill)
	bad.drawData = append(bad.drawData, 0) // truncated: missing style word

	goodTags := append([]Tag(nil), good.Tags()...)
	badTags := append([]Tag(nil), bad.Tags()...)
	badDraw := append([]uint32(nil), bad.DrawData()...)

	if _, err := DiffEncodings(good, bad); !errors.Is(err, ErrMalformedEncoding) {
		t.Fatalf("err = %v, want ErrMalformedEncoding", err)
	}
	assertSlicesEqual(t, "good.tags", good.Tags(), goodTags)
	assertSlicesEqual(t, "bad.tags", bad.Tags(), badTags)
	assertSlicesEqual(t, "bad.drawData", bad.DrawData(), badDraw)
}

func TestDiff_DoesNotTouchSharedCache(t *testing.T) {
	// The diff is a pure read: a pre-populated LayerCache must be unaffected by
	// repeated diffing (no entries added, removed, or reordered).
	cache := NewLayerCache(1)
	enc := fillRectEnc(0, 0, 10, 10, solidColor(1, 0, 0))
	hash := enc.Hash()
	// Populate the shared cache, then snapshot its observable state.
	pm := gg.NewPixmap(4, 4)
	cache.Put(hash, pm, 1)
	before := cache.Stats()

	for range 3 {
		d, err := DiffEncodings(enc, enc.Clone())
		if err != nil || !d.Empty() {
			t.Fatalf("unexpected diff result: %+v err=%v", d, err)
		}
	}
	after := cache.Stats()
	if after.Entries != before.Entries || after.Size != before.Size ||
		after.Hits != before.Hits || after.Evictions != before.Evictions {
		t.Errorf("LayerCache changed during diff: before=%+v after=%+v", before, after)
	}
	if _, ok := cache.Get(hash); !ok {
		t.Error("pre-populated cache entry disappeared after diff")
	}
}

// ---------------------------------------------------------------------------
// 2. Insertion / deletion / move fixed frames
// ---------------------------------------------------------------------------

func TestDiff_Insertion(t *testing.T) {
	red := solidColor(1, 0, 0)
	blue := solidColor(0, 0, 1)
	old := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{20, 20, 10, 10, red},
	)
	next := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{10, 10, 5, 5, blue}, // inserted in the middle
		rectFill{20, 20, 10, 10, red},
	)
	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Added) != 1 || len(d.Removed) != 0 || len(d.Changed) != 0 {
		t.Fatalf("insertion = add:%d rem:%d chg:%d, want 1/0/0",
			len(d.Added), len(d.Removed), len(d.Changed))
	}
	if d.Added[0].Tag != TagFill {
		t.Errorf("added tag = %s, want Fill", d.Added[0].TagName)
	}
	if d.Added[0].NewIndex < 0 || d.Added[0].OldIndex != -1 {
		t.Errorf("added indices old=%d next=%d, want old=-1", d.Added[0].OldIndex, d.Added[0].NewIndex)
	}
}

func TestDiff_Deletion(t *testing.T) {
	red := solidColor(1, 0, 0)
	blue := solidColor(0, 0, 1)
	old := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{10, 10, 5, 5, blue},
		rectFill{20, 20, 10, 10, red},
	)
	next := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{20, 20, 10, 10, red},
	)
	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Removed) != 1 || len(d.Added) != 0 || len(d.Changed) != 0 {
		t.Fatalf("deletion = add:%d rem:%d chg:%d, want 0/1/0",
			len(d.Added), len(d.Removed), len(d.Changed))
	}
	if d.Removed[0].OldIndex < 0 || d.Removed[0].NewIndex != -1 {
		t.Errorf("removed indices old=%d next=%d, want next=-1", d.Removed[0].OldIndex, d.Removed[0].NewIndex)
	}
}

func TestDiff_Move(t *testing.T) {
	red := solidColor(1, 0, 0)
	// Two distinct-but-same-paint rectangles, swapped in host order. Because
	// their geometry differs, both are independently identifiable and the swap
	// is reported as moved changes rather than remove+add.
	old := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{40, 40, 10, 10, red},
	)
	next := multiFillEnc(
		rectFill{40, 40, 10, 10, red},
		rectFill{0, 0, 10, 10, red},
	)
	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Added) != 0 || len(d.Removed) != 0 {
		t.Fatalf("swap produced add/remove: add:%d rem:%d", len(d.Added), len(d.Removed))
	}
	if len(d.Changed) != 2 {
		t.Fatalf("move chg:%d, want 2 (both rectangles reordered)", len(d.Changed))
	}
	for _, c := range d.Changed {
		if !c.Moved {
			t.Errorf("change at next index %d not marked moved", c.NewIndex)
		}
	}
	// Host-order stability: changed entries sorted by NewIndex ascending.
	for i := 1; i < len(d.Changed); i++ {
		if d.Changed[i-1].NewIndex >= d.Changed[i].NewIndex {
			t.Errorf("changed not in host order: %d before %d",
				d.Changed[i-1].NewIndex, d.Changed[i].NewIndex)
		}
	}
}

func TestDiff_InsertionDoesNotShiftMoveFlag(t *testing.T) {
	// Inserting a command before an unchanged one shifts its ordinal but must
	// not mark the survivor as moved (its rank among content anchors is stable).
	red := solidColor(1, 0, 0)
	blue := solidColor(0, 0, 1)
	old := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{20, 20, 10, 10, red},
	)
	next := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{10, 10, 5, 5, blue},
		rectFill{20, 20, 10, 10, red},
	)
	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Added) != 1 || len(d.Removed) != 0 || len(d.Changed) != 0 {
		t.Fatalf("insertion = add:%d rem:%d chg:%d, want 1/0/0",
			len(d.Added), len(d.Removed), len(d.Changed))
	}
}

// ---------------------------------------------------------------------------
// 3. Color-only and resource-only changes
// ---------------------------------------------------------------------------

func TestDiff_ColorOnlyChange(t *testing.T) {
	old := fillRectEnc(0, 0, 10, 10, solidColor(1, 0, 0))
	next := fillRectEnc(0, 0, 10, 10, solidColor(0, 1, 0))
	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Changed) != 1 || len(d.Added) != 0 || len(d.Removed) != 0 {
		t.Fatalf("color change = add:%d rem:%d chg:%d, want 0/0/1",
			len(d.Added), len(d.Removed), len(d.Changed))
	}
	c := d.Changed[0]
	if !containsStr(c.Aspects, AspectPaint) || !containsStr(c.Aspects, AspectResource) {
		t.Errorf("aspects = %v, want paint+resource", c.Aspects)
	}
	if containsStr(c.Aspects, AspectGeometry) || containsStr(c.Aspects, AspectStyle) {
		t.Errorf("color-only change wrongly flagged geometry/style: %v", c.Aspects)
	}
	if len(c.Resources) != 1 || c.Resources[0].Kind != ResourceBrush {
		t.Fatalf("resources = %+v, want one brush diff", c.Resources)
	}
	if c.Moved {
		t.Errorf("same-position change marked moved")
	}
}

func TestDiff_SameColorDifferentBrushSlotNoFalsePositive(t *testing.T) {
	// Two encodings place the identical solid color in different brush slots.
	old := NewEncoding()
	old.brushes = append(old.brushes, solidColor(0.2, 0.4, 0.6))
	p := gg.NewPath()
	p.Rectangle(0, 0, 10, 10)
	old.EncodePath(p)
	old.EncodeFill(old.brushes[0], FillNonZero)

	next := NewEncoding()
	// Slot 0 occupied by an unrelated brush first.
	next.brushes = append(next.brushes, solidColor(0.9, 0.9, 0.9), solidColor(0.2, 0.4, 0.6))
	p2 := gg.NewPath()
	p2.Rectangle(0, 0, 10, 10)
	next.EncodePath(p2)
	next.EncodeFill(next.brushes[1], FillNonZero)

	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !d.Empty() {
		t.Fatalf("identical content in different brush slots reported changes: %+v", d.Changed)
	}
}

func TestDiff_SharedTemporarySliceNoFalsePositive(t *testing.T) {
	// Build two encodings that end up sharing backing arrays, then verify
	// content equality is decided on values, not slice identity.
	vals := []float32{0, 0, 10, 0, 10, 10, 0, 10}
	tags := []Tag{TagBeginPath, TagMoveTo, TagLineTo, TagLineTo, TagLineTo, TagClosePath, TagEndPath, TagFill}

	// One backing array, two value-equal views: old takes the first copy, next
	// overwrites the same array with identical values.
	shared := make([]float32, len(vals))
	copy(shared, vals)

	old := NewEncoding()
	old.pathData = append(old.pathData, shared...)
	old.tags = append([]Tag(nil), tags...)
	old.brushes = append(old.brushes, solidColor(1, 0, 0))
	old.drawData = []uint32{0, 0} // brush index + fill style

	next := NewEncoding()
	copy(shared, vals) // identical rewrite of the shared temporary slice
	next.pathData = append(next.pathData, shared...)
	next.tags = append([]Tag(nil), tags...)
	next.brushes = append(next.brushes, solidColor(1, 0, 0))
	next.drawData = []uint32{0, 0}

	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !d.Empty() {
		t.Fatalf("value-equal encodings sharing slice storage reported a diff: %+v", d)
	}
}

func TestDiff_ImageResourceOnlyChange(t *testing.T) {
	old := NewEncoding()
	old.EncodeImage(0, TranslateAffine(5, 5))
	next := NewEncoding()
	next.EncodeImage(1, TranslateAffine(5, 5)) // same geometry, different image

	d, err := DiffEncodingsWithOptions(old, next, DiffOptions{
		OldImageCount: 2, NewImageCount: 2,
	})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Added)+len(d.Removed) != 0 {
		t.Fatalf("image swap produced add/remove: %+v / %+v", d.Added, d.Removed)
	}
	if len(d.Changed) != 1 {
		t.Fatalf("image swap changed = %d, want 1", len(d.Changed))
	}
	c := d.Changed[0]
	if !containsStr(c.Aspects, AspectImage) || !containsStr(c.Aspects, AspectResource) {
		t.Errorf("aspects = %v, want image+resource", c.Aspects)
	}
	var found bool
	for _, r := range c.Resources {
		if r.Kind == ResourceImage && r.OldIndex == 0 && r.NewIndex == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("image resource diff missing: %+v", c.Resources)
	}
}

func TestDiff_InvalidImageReference(t *testing.T) {
	good := NewEncoding()
	good.EncodeImage(0, IdentityAffine())
	bad := NewEncoding()
	bad.EncodeImage(7, IdentityAffine()) // registry only has 1 image

	if _, err := DiffEncodingsWithOptions(good, bad, DiffOptions{
		OldImageCount: 1, NewImageCount: 1,
	}); !errors.Is(err, ErrInvalidResource) {
		t.Fatalf("err = %v, want ErrInvalidResource", err)
	}
}

func TestDiff_InvalidBrushReference(t *testing.T) {
	good := fillRectEnc(0, 0, 10, 10, solidColor(1, 0, 0))
	bad := NewEncoding()
	p := gg.NewPath()
	p.Rectangle(0, 0, 10, 10)
	bad.EncodePath(p)
	// Brush index 5 with zero brushes present.
	bad.tags = append(bad.tags, TagFill)
	bad.drawData = append(bad.drawData, 5, 0)

	if _, err := DiffEncodings(good, bad); !errors.Is(err, ErrInvalidResource) {
		t.Fatalf("err = %v, want ErrInvalidResource", err)
	}
}

// ---------------------------------------------------------------------------
// 4. Transform / clip / layer(filter) / text changes locatable to a command
// ---------------------------------------------------------------------------

func TestDiff_TransformChange(t *testing.T) {
	old := NewEncoding()
	old.EncodeTransform(TranslateAffine(0, 0))
	next := NewEncoding()
	next.EncodeTransform(TranslateAffine(100, 0))

	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Changed) != 1 {
		t.Fatalf("transform changed = %d, want 1", len(d.Changed))
	}
	c := d.Changed[0]
	if c.Tag != TagTransform || !containsStr(c.Aspects, AspectTransform) {
		t.Errorf("transform change = tag %s aspects %v", c.TagName, c.Aspects)
	}
}

func TestDiff_ClipChange(t *testing.T) {
	old := clipEnc(0, 0, 10, 10)
	next := clipEnc(0, 0, 20, 20)
	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	var clipChange *CommandChange
	for i := range d.Changed {
		if d.Changed[i].Tag == TagBeginClip {
			clipChange = &d.Changed[i]
		}
	}
	if clipChange == nil {
		t.Fatalf("no BeginClip change found; changed = %+v", d.Changed)
	}
	if !containsStr(clipChange.Aspects, AspectClip) ||
		!containsStr(clipChange.Aspects, AspectGeometry) {
		t.Errorf("clip aspects = %v, want clip+geometry", clipChange.Aspects)
	}
}

func TestDiff_LayerFilterChange(t *testing.T) {
	// PushLayer encodes blend mode (the filter/compositing parameter).
	old := NewEncoding()
	old.EncodePushLayer(BlendNormal, 1.0)
	old.EncodePopLayer()
	next := NewEncoding()
	next.EncodePushLayer(BlendMultiply, 0.5)
	next.EncodePopLayer()

	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	var layerChange *CommandChange
	for i := range d.Changed {
		if d.Changed[i].Tag == TagPushLayer {
			layerChange = &d.Changed[i]
		}
	}
	if layerChange == nil {
		t.Fatalf("no PushLayer change found; changed = %+v", d.Changed)
	}
	if !containsStr(layerChange.Aspects, AspectLayer) {
		t.Errorf("layer aspects = %v, want layer", layerChange.Aspects)
	}
}

func TestDiff_TextResourceChange(t *testing.T) {
	brush := solidColor(0, 0, 0)
	old := textEnc(100, 14, brush, "A")
	next := textEnc(200, 14, brush, "A") // different font id only

	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Changed) != 1 {
		t.Fatalf("text font change = %d, want 1", len(d.Changed))
	}
	c := d.Changed[0]
	if c.Tag != TagText {
		t.Fatalf("tag = %s, want Text", c.TagName)
	}
	var fontDiffFound bool
	for _, r := range c.Resources {
		if r.Kind == ResourceFont {
			fontDiffFound = true
		}
	}
	if !fontDiffFound || !containsStr(c.Aspects, AspectText) {
		t.Errorf("text font change: aspects=%v resources=%+v", c.Aspects, c.Resources)
	}
}

func TestDiff_TextContentChange(t *testing.T) {
	brush := solidColor(0, 0, 0)
	old := textEnc(100, 14, brush, "A")
	next := textEnc(100, 18, brush, "B") // glyph/string + size change

	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Changed) != 1 || !containsStr(d.Changed[0].Aspects, AspectText) {
		t.Fatalf("text content change not located: %+v", d.Changed)
	}
}

// ---------------------------------------------------------------------------
// 5. Bounds merging
// ---------------------------------------------------------------------------

func TestDiff_ChangedBoundsUnion(t *testing.T) {
	red := solidColor(1, 0, 0)
	old := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{100, 100, 10, 10, red},
	)
	// Move first rect; delete second.
	next := fillRectEnc(50, 0, 10, 10, red)

	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	b := d.ChangedBounds()
	// Must cover old position (0..10), next position (50..60), and removed (100..110).
	if b.MinX != 0 || b.MaxX < 110 || b.MinY != 0 || b.MaxY < 10 {
		t.Errorf("union bounds = %+v, want MinX=0 MaxX>=110 MinY=0 MaxY>=10", b)
	}
}

func TestDiff_BoundsOnAddedAndRemoved(t *testing.T) {
	red := solidColor(1, 0, 0)
	old := fillRectEnc(0, 0, 10, 10, red)

	// An insertion: a second rect added at a different location.
	withAdd := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{30, 30, 8, 8, red},
	)
	d, err := DiffEncodings(old, withAdd)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Added) != 1 {
		t.Fatalf("added = %d, want 1", len(d.Added))
	}
	if d.Added[0].NewBounds.MaxX != 38 || d.Added[0].NewBounds.MaxY != 38 {
		t.Errorf("added bounds = %+v, want max (38,38)", d.Added[0].NewBounds)
	}

	// A removal: the second rect gone again.
	d2, err := DiffEncodings(withAdd, old)
	if err != nil {
		t.Fatalf("diff2: %v", err)
	}
	if len(d2.Removed) != 1 || d2.Removed[0].OldBounds.MinX != 30 {
		t.Errorf("removed = %+v", d2.Removed)
	}

	// A same-tag command that moves to unrelated coordinates is a changed
	// entry whose bounds cover both the vacated and the next area.
	moved := fillRectEnc(30, 30, 8, 8, red)
	d3, err := DiffEncodings(old, moved)
	if err != nil {
		t.Fatalf("diff3: %v", err)
	}
	if len(d3.Changed) != 1 {
		t.Fatalf("changed = %d, want 1", len(d3.Changed))
	}
	u := d3.ChangedBounds()
	if u.MinX != 0 || u.MaxX != 38 || u.MinY != 0 || u.MaxY != 38 {
		t.Errorf("changed union bounds = %+v, want (0,0)-(38,38)", u)
	}
}

// ---------------------------------------------------------------------------
// 6. JSON stability and cross-version comparison
// ---------------------------------------------------------------------------

func TestDiff_JSONStableAndDeterministic(t *testing.T) {
	red := solidColor(1, 0, 0)
	blue := solidColor(0, 0, 1)
	old := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{20, 20, 10, 10, red},
	)
	next := multiFillEnc(
		rectFill{0, 0, 10, 10, blue}, // color change
		rectFill{20, 20, 10, 10, red},
		rectFill{40, 40, 10, 10, red}, // addition
	)

	d1, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff1: %v", err)
	}
	d2, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("diff2: %v", err)
	}

	j1, err := json.Marshal(d1)
	if err != nil {
		t.Fatalf("marshal 1: %v", err)
	}
	j2, err := json.Marshal(d2)
	if err != nil {
		t.Fatalf("marshal 2: %v", err)
	}
	if !bytes.Equal(j1, j2) {
		t.Errorf("non-deterministic JSON:\n%s\nvs\n%s", j1, j2)
	}

	// Round-trip preserves the structure.
	var rt SceneDiff
	if err := json.Unmarshal(j1, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rt.Added) != len(d1.Added) || len(rt.Changed) != len(d1.Changed) ||
		len(rt.Removed) != len(d1.Removed) {
		t.Errorf("round-trip lengths differ: %+v vs %+v", rt, d1)
	}
	// Re-marshaling the round-tripped value yields identical bytes.
	j3, err := json.Marshal(&rt)
	if err != nil {
		t.Fatalf("marshal 3: %v", err)
	}
	if !bytes.Equal(j3, j1) {
		t.Errorf("round-trip JSON differs:\n%s\nvs\n%s", j3, j1)
	}
}

func TestDiff_CrossVersionComparison(t *testing.T) {
	// An "old version" encoding and an independently built "next version"
	// encoding must still diff correctly even though they share no pointers.
	v1 := fillRectEnc(0, 0, 10, 10, solidColor(1, 0, 0))
	v2Bytes := v1.Clone() // independent allocation, same logical content
	d, err := DiffEncodings(v1, v2Bytes)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !d.Empty() {
		t.Fatalf("cloned independent versions reported changes: %+v", d)
	}
}

// ---------------------------------------------------------------------------
// 7. Duplicate IDs and oversize scenes
// ---------------------------------------------------------------------------

func TestDiff_DuplicateGeometryAllowed(t *testing.T) {
	red := solidColor(1, 0, 0)
	// Two identical rectangles: duplicate structural IDs must be legal and
	// aligned by occurrence, not rejected.
	old := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{0, 0, 10, 10, red},
	)
	next := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{0, 0, 10, 10, red},
		rectFill{0, 0, 10, 10, red},
	)
	d, err := DiffEncodings(old, next)
	if err != nil {
		t.Fatalf("duplicate geometry should be allowed, got err: %v", err)
	}
	if len(d.Added) != 1 || len(d.Removed) != 0 || len(d.Changed) != 0 {
		t.Fatalf("dup geometry = add:%d rem:%d chg:%d, want 1/0/0",
			len(d.Added), len(d.Removed), len(d.Changed))
	}
}

func TestDiff_DuplicateFontIDConflictingPayload(t *testing.T) {
	enc := NewEncoding()
	enc.brushes = append(enc.brushes, solidColor(0, 0, 0))
	enc.EncodeText(GlyphRunData{FontSourceID: 7, FontSize: 12, GlyphCount: 1, BrushIndex: 0, TextLen: 1},
		[]GlyphEntry{{GlyphID: 1}}, "A")
	enc.EncodeText(GlyphRunData{FontSourceID: 7, FontSize: 18, GlyphCount: 1, BrushIndex: 0, TextLen: 1},
		[]GlyphEntry{{GlyphID: 1}}, "A")

	if _, err := DiffEncodings(NewEncoding(), enc); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("err = %v, want ErrDuplicateID", err)
	}
}

func TestDiff_OversizeScene(t *testing.T) {
	enc := NewEncoding()
	// Emit MaxDiffCommands+1 state tags (each is one tag, no stream payload
	// concerns beyond a draw word for AntiAlias; use transform tags which only
	// consume the transform stream, sized accordingly).
	for range MaxDiffCommands + 1 {
		enc.EncodeTransform(IdentityAffine())
	}
	_, err := DiffEncodings(NewEncoding(), enc)
	if !errors.Is(err, ErrSceneTooLarge) {
		t.Fatalf("err = %v, want ErrSceneTooLarge", err)
	}
}

func TestDiff_MalformedStream(t *testing.T) {
	// A Fill tag promising 2 draw words but with a truncated draw stream.
	enc := NewEncoding()
	enc.tags = []Tag{TagFill}
	enc.drawData = []uint32{0} // missing style word
	if _, err := DiffEncodings(NewEncoding(), enc); !errors.Is(err, ErrMalformedEncoding) {
		t.Fatalf("err = %v, want ErrMalformedEncoding", err)
	}
}

// ---------------------------------------------------------------------------
// 8. Identical scene diff is empty; existing scene behavior untouched
// ---------------------------------------------------------------------------

func TestDiff_IdenticalSceneIsEmpty(t *testing.T) {
	red := solidColor(1, 0, 0)
	blue := solidColor(0, 0, 1)
	enc := multiFillEnc(
		rectFill{0, 0, 10, 10, red},
		rectFill{20, 20, 30, 30, blue},
	)
	d, err := DiffEncodings(enc, enc.Clone())
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !d.Empty() {
		t.Fatalf("identical scene reported changes: %+v", d)
	}
	if !d.ChangedBounds().IsEmpty() {
		t.Errorf("empty diff bounds = %+v, want empty rect", d.ChangedBounds())
	}
}

func TestDiff_SceneLevelEndToEnd(t *testing.T) {
	s1, s2 := NewScene(), NewScene()
	s1.Fill(FillNonZero, IdentityAffine(), solidColor(1, 0, 0), NewRectShape(0, 0, 10, 10))
	s2.Fill(FillNonZero, IdentityAffine(), solidColor(0, 1, 0), NewRectShape(0, 0, 10, 10))

	d, err := DiffScenes(s1, s2)
	if err != nil {
		t.Fatalf("DiffScenes: %v", err)
	}
	if len(d.Changed) != 1 {
		t.Fatalf("scene-level changed = %d, want 1", len(d.Changed))
	}
	// Image registry validation with a real image.
	img := NewImage(16, 16)
	s3 := NewScene()
	s3.DrawImage(img, IdentityAffine())
	s4 := NewScene()
	s4.DrawImage(img, IdentityAffine())
	d2, err := DiffScenes(s3, s4)
	if err != nil {
		t.Fatalf("image DiffScenes: %v", err)
	}
	if !d2.Empty() {
		t.Fatalf("identical image scenes differ: %+v", d2)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type rectFill struct {
	x, y, w, h float32
	brush      Brush
}

func multiFillEnc(rects ...rectFill) *Encoding {
	enc := NewEncoding()
	for _, r := range rects {
		p := gg.NewPath()
		p.MoveTo(float64(r.x), float64(r.y))
		p.LineTo(float64(r.x+r.w), float64(r.y))
		p.LineTo(float64(r.x+r.w), float64(r.y+r.h))
		p.LineTo(float64(r.x), float64(r.y+r.h))
		p.Close()
		enc.EncodePath(p)
		enc.EncodeFill(r.brush, FillNonZero)
	}
	return enc
}

func clipEnc(x, y, w, h float32) *Encoding {
	s := NewScene()
	s.PushClip(NewRectShape(x, y, w, h))
	s.PopClip()
	return s.Encoding()
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func assertSlicesEqual[T comparable](t *testing.T, name string, got, want []T) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s length = %d, want %d", name, len(got), len(want))
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %v, want %v", name, i, got[i], want[i])
			return
		}
	}
}
