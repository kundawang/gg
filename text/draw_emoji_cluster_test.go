package text

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/gogpu/gg/text/emoji"
)

// --- Fake color emoji font ----------------------------------------------

// fakeEmojiFont implements ParsedFont, ColorFont and SequenceGlyphProvider
// for cluster rendering tests. It has no outline data (like a pure bitmap
// emoji font): outline rendering produces no pixels but never panics.
type fakeEmojiFont struct {
	glyphs     map[rune]uint16   // cmap
	advUnits   map[uint16]int    // advance in font units (upem = 1000)
	seqGlyphs  map[string]uint16 // whole-sequence → glyph (SequenceGlyphProvider)
	bitmapType map[uint16]bool   // gids whose GlyphType is bitmap
	colrType   map[uint16]bool   // gids whose GlyphType is COLR
	bitmaps    map[uint16]*emoji.BitmapGlyph

	// Call recording for fallback/cache verification.
	sequenceCalls int
	bitmapCalls   int
	bitmapPPEMs   []uint16
}

func newFakeEmojiFont() *fakeEmojiFont {
	return &fakeEmojiFont{
		glyphs:     make(map[rune]uint16),
		advUnits:   make(map[uint16]int),
		seqGlyphs:  make(map[string]uint16),
		bitmapType: make(map[uint16]bool),
		colrType:   make(map[uint16]bool),
		bitmaps:    make(map[uint16]*emoji.BitmapGlyph),
	}
}

// ParsedFont implementation.

func (f *fakeEmojiFont) Name() string     { return "FakeEmoji" }
func (f *fakeEmojiFont) FullName() string { return "FakeEmoji Regular" }
func (f *fakeEmojiFont) NumGlyphs() int   { return 256 }
func (f *fakeEmojiFont) UnitsPerEm() int  { return 1000 }

func (f *fakeEmojiFont) GlyphIndex(r rune) uint16 { return f.glyphs[r] }

func (f *fakeEmojiFont) GlyphAdvance(gid uint16, ppem float64) float64 {
	return float64(f.advUnits[gid]) * ppem / 1000
}

func (f *fakeEmojiFont) GlyphBounds(uint16, float64) Rect { return Rect{} }

func (f *fakeEmojiFont) Metrics(ppem float64) FontMetrics {
	return FontMetrics{Ascent: ppem * 0.8, Descent: -ppem * 0.2}
}

// ColorFont implementation.

func (f *fakeEmojiFont) HasColorTables() bool { return true }

func (f *fakeEmojiFont) GlyphType(gid uint16) GlyphType {
	if f.colrType[gid] {
		return GlyphTypeCOLR
	}
	if f.bitmapType[gid] {
		return GlyphTypeBitmap
	}
	return GlyphTypeOutline
}

func (f *fakeEmojiFont) BitmapGlyph(gid uint16, ppem uint16) (*emoji.BitmapGlyph, error) {
	f.bitmapCalls++
	f.bitmapPPEMs = append(f.bitmapPPEMs, ppem)
	if b, ok := f.bitmaps[gid]; ok {
		return b, nil
	}
	return nil, emoji.ErrGlyphNotInBitmap
}

func (f *fakeEmojiFont) COLRGlyph(uint16, int) (*emoji.COLRGlyph, error) {
	return nil, emoji.ErrGlyphNotInCOLR
}

// SequenceGlyphProvider implementation.

func (f *fakeEmojiFont) SequenceGlyph(codepoints []rune) (uint16, bool) {
	f.sequenceCalls++
	gid, ok := f.seqGlyphs[string(codepoints)]
	return gid, ok
}

// --- Test helpers --------------------------------------------------------

// addBitmapGlyph registers a bitmap glyph with a solid-color PNG.
func (f *fakeEmojiFont) addBitmapGlyph(gid uint16, advUnits int, col color.RGBA) {
	const size = 8
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for i := range img.Pix {
		img.Pix[i] = 255 // opaque
	}
	// Fill with the solid color.
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.SetRGBA(x, y, col)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	f.bitmapType[gid] = true
	f.bitmaps[gid] = &emoji.BitmapGlyph{
		GlyphID: gid,
		Data:    buf.Bytes(),
		Format:  emoji.FormatPNG,
		Width:   size,
		Height:  size,
		OriginX: 0,
		OriginY: 8, // bitmap sits on the baseline
		PPEM:    16,
	}
	f.advUnits[gid] = advUnits
}

// newFakeEmojiSource wraps a parsed font in a FontSource.
func newFakeEmojiSource(parsed ParsedFont) *FontSource {
	src := &FontSource{parsed: parsed, name: parsed.Name()}
	src.addr = src
	return src
}

// pixelIsColor reports whether the pixel at (x, y) is exactly col.
func pixelIsColor(img *image.RGBA, x, y int, col color.RGBA) bool {
	if !(image.Point{X: x, Y: y}.In(img.Rect)) {
		return false
	}
	got := img.RGBAAt(x, y)
	return got == col
}

// pixelIsEmpty reports whether the pixel at (x, y) is fully transparent.
func pixelIsEmpty(img *image.RGBA, x, y int) bool {
	if !(image.Point{X: x, Y: y}.In(img.Rect)) {
		return true
	}
	_, _, _, a := img.At(x, y).RGBA()
	return a == 0
}

var (
	fakeRed    = color.RGBA{R: 255, G: 0, B: 0, A: 255}
	fakeGreen  = color.RGBA{G: 255, B: 0, A: 255}
	fakeBlue   = color.RGBA{B: 255, A: 255}
	fakeYellow = color.RGBA{R: 255, G: 255, A: 255}
	fakeCyan   = color.RGBA{G: 255, B: 255, A: 255}
	fakePurple = color.RGBA{R: 255, B: 255, A: 255}
)

// newClusterFixture builds a fake font with a standard set of emoji:
//
//	😀 gid 1 (bitmap, green)          single emoji
//	👨 gid 10, 👩 gid 11, 👧 gid 12    family members (bitmaps)
//	👨‍👩‍👧 gid 100 (bitmap, red)         ZWJ family cluster
//	💪 gid 20 (bitmap)                modifier base
//	💪🏻 gid 101, 💪🏿 gid 102           skin tone clusters
//	🇺🇸 gid 103 (bitmap)              flag cluster
//	1️⃣ gid 104 (bitmap)               keycap cluster
//	🇩🇪 gid 105 (outline, no bitmap)  whole-cluster outline fallback
//	🇫🇷 gid 106 (COLR, no layers)     whole-cluster COLR fallback
//	🤖 gid 50 (bitmap type, no data)  missing bitmap fallback
//	A gid 40, B gid 41 (outline)      text
func newClusterFixture() *fakeEmojiFont {
	f := newFakeEmojiFont()

	// Text glyphs (no outlines in the fake: no pixels, but advances apply).
	f.glyphs['A'] = 40
	f.glyphs['B'] = 41
	f.glyphs['1'] = 30
	f.advUnits[40] = 500
	f.advUnits[41] = 500
	f.advUnits[30] = 500
	f.advUnits[0] = 500 // .notdef

	// Single emoji.
	f.glyphs['😀'] = 1
	f.addBitmapGlyph(1, 1000, fakeGreen)

	// Family members (drawn only on the per-member fallback path).
	f.glyphs['👨'] = 10
	f.glyphs['👩'] = 11
	f.glyphs['👧'] = 12
	f.addBitmapGlyph(10, 1000, fakeBlue)
	f.addBitmapGlyph(11, 1000, fakeYellow)
	f.addBitmapGlyph(12, 1000, fakeCyan)

	// ZWJ family cluster.
	f.seqGlyphs["👨‍👩‍👧"] = 100
	f.addBitmapGlyph(100, 1000, fakeRed)

	// Skin tone clusters.
	f.glyphs['💪'] = 20
	f.addBitmapGlyph(20, 1000, fakeBlue)
	f.seqGlyphs["💪🏻"] = 101
	f.seqGlyphs["💪🏿"] = 102
	f.addBitmapGlyph(101, 1000, fakeYellow)
	f.addBitmapGlyph(102, 1000, fakePurple)

	// Flag cluster.
	f.seqGlyphs["🇺🇸"] = 103
	f.addBitmapGlyph(103, 1000, fakeRed)

	// Keycap cluster (combining enclosing keycap).
	f.seqGlyphs["1️⃣"] = 104
	f.addBitmapGlyph(104, 1000, fakeCyan)

	// Whole-cluster outline fallback (resolves, but has no color data).
	f.seqGlyphs["🇩🇪"] = 105
	f.advUnits[105] = 1000

	// Whole-cluster COLR fallback (resolves, COLR rendering not implemented).
	f.seqGlyphs["🇫🇷"] = 106
	f.colrType[106] = true
	f.advUnits[106] = 1000

	// Missing bitmap data (GlyphType says bitmap, extraction fails).
	f.glyphs['🤖'] = 50
	f.bitmapType[50] = true
	f.advUnits[50] = 1000

	return f
}

// --- Cluster rendering tests ---------------------------------------------

func TestDrawWithEmoji_ZWJFamilyCluster(t *testing.T) {
	f := newClusterFixture()
	face := newFakeEmojiSource(f).Face(16)
	dst := image.NewRGBA(image.Rect(0, 0, 200, 50))

	// Family cluster followed by a single emoji to verify advance.
	DrawWithEmoji(dst, "👨‍👩‍👧😀", face, 0, 40, color.Black)

	// The whole-cluster glyph must be drawn exactly once...
	if f.bitmapCalls != 2 {
		t.Errorf("bitmapCalls = %d, want 2 (cluster + single emoji)", f.bitmapCalls)
	}
	if f.sequenceCalls != 1 {
		t.Errorf("sequenceCalls = %d, want 1 (only the ZWJ cluster)", f.sequenceCalls)
	}

	// ...at the cluster origin (red bitmap at x 0..8, baseline 40, top at 32).
	if !pixelIsColor(dst, 4, 36, fakeRed) {
		t.Error("cluster bitmap not drawn at origin")
	}

	// Members must NOT be drawn individually: nothing at x 8..16.
	if !pixelIsEmpty(dst, 12, 36) {
		t.Error("cluster member drawn separately (double draw)")
	}

	// Advance: the following 😀 starts at x = 16 (1000 units at 16px).
	if !pixelIsColor(dst, 20, 36, fakeGreen) {
		t.Error("following emoji not positioned after cluster advance")
	}
}

func TestDrawWithEmoji_SkinToneClusters(t *testing.T) {
	f := newClusterFixture()
	face := newFakeEmojiSource(f).Face(16)
	dst := image.NewRGBA(image.Rect(0, 0, 200, 50))

	// Two different skin tones of the same base, then the bare base.
	DrawWithEmoji(dst, "💪🏻💪🏿💪", face, 0, 40, color.Black)

	// Light skin tone cluster at x 0..8.
	if !pixelIsColor(dst, 4, 36, fakeYellow) {
		t.Error("light skin tone cluster not drawn at x=0")
	}
	// Dark skin tone cluster at x 16..24 — proves the cache did not
	// confuse the two sequences (same base, different modifier).
	if !pixelIsColor(dst, 20, 36, fakePurple) {
		t.Error("dark skin tone cluster not drawn at x=16 (cache confusion?)")
	}
	// Bare base emoji at x 32..40.
	if !pixelIsColor(dst, 36, 36, fakeBlue) {
		t.Error("bare base emoji not drawn at x=32")
	}

	// The modifier must not be drawn as a separate glyph.
	if f.bitmapCalls != 3 {
		t.Errorf("bitmapCalls = %d, want 3 (one per cluster)", f.bitmapCalls)
	}
}

func TestDrawWithEmoji_ClusterCacheHit(t *testing.T) {
	f := newClusterFixture()
	face := newFakeEmojiSource(f).Face(16)
	dst := image.NewRGBA(image.Rect(0, 0, 200, 50))

	DrawWithEmoji(dst, "💪🏻", face, 0, 40, color.Black)
	DrawWithEmoji(dst, "💪🏻", face, 0, 40, color.Black)

	if f.sequenceCalls != 1 {
		t.Errorf("sequenceCalls = %d, want 1 (second draw must hit the cache)", f.sequenceCalls)
	}
	if f.bitmapCalls != 2 {
		t.Errorf("bitmapCalls = %d, want 2 (bitmap still drawn each time)", f.bitmapCalls)
	}
}

func TestDrawWithEmoji_FlagCluster(t *testing.T) {
	f := newClusterFixture()
	face := newFakeEmojiSource(f).Face(16)
	dst := image.NewRGBA(image.Rect(0, 0, 200, 50))

	// Two flags in a row: Segment merges them into one emoji run;
	// each must still render as its own cluster.
	DrawWithEmoji(dst, "🇺🇸🇺🇸", face, 0, 40, color.Black)

	if f.bitmapCalls != 2 {
		t.Errorf("bitmapCalls = %d, want 2 (one per flag)", f.bitmapCalls)
	}
	if !pixelIsColor(dst, 4, 36, fakeRed) {
		t.Error("first flag not drawn at x=0")
	}
	if !pixelIsColor(dst, 20, 36, fakeRed) {
		t.Error("second flag not drawn at x=16 (wrong cluster advance)")
	}
}

func TestDrawWithEmoji_KeycapCluster(t *testing.T) {
	f := newClusterFixture()
	face := newFakeEmojiSource(f).Face(16)
	dst := image.NewRGBA(image.Rect(0, 0, 200, 50))

	DrawWithEmoji(dst, "1️⃣", face, 0, 40, color.Black)

	// The keycap (base + FE0F + combining mark) renders as one bitmap;
	// the combining mark is not drawn separately.
	if f.bitmapCalls != 1 {
		t.Errorf("bitmapCalls = %d, want 1", f.bitmapCalls)
	}
	if !pixelIsColor(dst, 4, 36, fakeCyan) {
		t.Error("keycap bitmap not drawn at origin")
	}
	if !pixelIsEmpty(dst, 12, 36) {
		t.Error("combining keycap drawn separately")
	}
}

func TestDrawWithEmoji_PerMemberFallback(t *testing.T) {
	f := newClusterFixture()
	// Remove the whole-cluster resource: the font has member glyphs only.
	delete(f.seqGlyphs, "👨‍👩‍👧")
	face := newFakeEmojiSource(f).Face(16)
	dst := image.NewRGBA(image.Rect(0, 0, 200, 50))

	DrawWithEmoji(dst, "👨‍👩‍👧", face, 0, 40, color.Black)

	// Explicit fallback: each visible member drawn once, in order;
	// the ZWJ produces neither pixels nor advance.
	if f.bitmapCalls != 3 {
		t.Errorf("bitmapCalls = %d, want 3 (one per visible member)", f.bitmapCalls)
	}
	if !pixelIsColor(dst, 4, 36, fakeBlue) {
		t.Error("member 1 (👨) not drawn at x=0")
	}
	if !pixelIsColor(dst, 20, 36, fakeYellow) {
		t.Error("member 2 (👩) not drawn at x=16")
	}
	if !pixelIsColor(dst, 36, 36, fakeCyan) {
		t.Error("member 3 (👧) not drawn at x=32")
	}
	// ZWJ must not produce a glyph: nothing between the members.
	if !pixelIsEmpty(dst, 12, 36) {
		t.Error("unexpected pixels between members (ZWJ drawn?)")
	}
}

func TestDrawWithEmoji_WholeClusterOutlineFallback(t *testing.T) {
	f := newClusterFixture()
	face := newFakeEmojiSource(f).Face(16)
	dst := image.NewRGBA(image.Rect(0, 0, 200, 50))

	// 🇩🇪 resolves to a single outline glyph: the fake has no outlines,
	// so nothing is painted, but the advance must still apply.
	// 🇫🇷 resolves to a COLR glyph: same expectation (COLR is Phase 2).
	DrawWithEmoji(dst, "🇩🇪🇫🇷😀", face, 0, 40, color.Black)

	if f.bitmapCalls != 1 {
		t.Errorf("bitmapCalls = %d, want 1 (only the trailing 😀)", f.bitmapCalls)
	}
	// 😀 after two 16px advances.
	if !pixelIsColor(dst, 36, 36, fakeGreen) {
		t.Error("😀 not positioned after outline/COLR cluster advances")
	}
}

func TestDrawWithEmoji_MissingBitmapFallsBack(t *testing.T) {
	f := newClusterFixture()
	face := newFakeEmojiSource(f).Face(16)
	dst := image.NewRGBA(image.Rect(0, 0, 200, 50))

	// 🤖 claims a bitmap but the data is missing: fall back to outline
	// (no pixels from the fake), keep the advance, don't panic.
	DrawWithEmoji(dst, "🤖😀", face, 0, 40, color.Black)

	if !pixelIsColor(dst, 20, 36, fakeGreen) {
		t.Error("😀 not positioned after missing-bitmap emoji advance")
	}
}

func TestDrawWithEmoji_MixedTextAndEmoji(t *testing.T) {
	f := newClusterFixture()
	face := newFakeEmojiSource(f).Face(16)
	dst := image.NewRGBA(image.Rect(0, 0, 200, 50))

	// Text runs adjacent to emoji clusters: "A" (8px), 😀 (16px),
	// "B" (8px), then the family cluster (16px).
	DrawWithEmoji(dst, "A😀B👨‍👩‍👧", face, 0, 40, color.Black)

	// 😀 starts after the "A" advance (8px).
	if !pixelIsColor(dst, 12, 36, fakeGreen) {
		t.Error("😀 not positioned after text run")
	}
	// Family cluster starts at 8 + 16 + 8 = 32.
	if !pixelIsColor(dst, 36, 36, fakeRed) {
		t.Error("cluster not positioned after text/emoji runs")
	}
}

func TestDrawWithEmoji_MultipleSizes(t *testing.T) {
	f := newClusterFixture()
	src := newFakeEmojiSource(f)

	// Size 16: bitmap is 1:1 (8x8 at PPEM 16).
	dst16 := image.NewRGBA(image.Rect(0, 0, 200, 60))
	DrawWithEmoji(dst16, "👨‍👩‍👧😀", src.Face(16), 0, 40, color.Black)
	if !pixelIsColor(dst16, 4, 36, fakeRed) {
		t.Error("size 16: cluster bitmap missing")
	}
	if !pixelIsEmpty(dst16, 12, 36) {
		t.Error("size 16: cluster bitmap wider than 8px at 1:1 scale")
	}
	if !pixelIsColor(dst16, 20, 36, fakeGreen) {
		t.Error("size 16: advance not applied")
	}

	// Size 32: bitmap scales 2x (16x16), advance doubles.
	dst32 := image.NewRGBA(image.Rect(0, 0, 200, 60))
	DrawWithEmoji(dst32, "👨‍👩‍👧😀", src.Face(32), 0, 50, color.Black)
	if !pixelIsColor(dst32, 12, 46, fakeRed) {
		t.Error("size 32: scaled cluster bitmap missing")
	}
	if !pixelIsColor(dst32, 36, 46, fakeGreen) {
		t.Error("size 32: advance did not scale with size")
	}

	// The bitmap strike request must use each face's ppem.
	var saw16, saw32 bool
	for _, ppem := range f.bitmapPPEMs {
		switch ppem {
		case 16:
			saw16 = true
		case 32:
			saw32 = true
		}
	}
	if !saw16 || !saw32 {
		t.Errorf("bitmap ppems = %v, want both 16 and 32", f.bitmapPPEMs)
	}
}

// --- Robustness tests ----------------------------------------------------

func TestDrawWithEmoji_Robustness(t *testing.T) {
	f := newClusterFixture()
	face := newFakeEmojiSource(f).Face(16)

	t.Run("nil face", func(t *testing.T) {
		dst := image.NewRGBA(image.Rect(0, 0, 50, 50))
		DrawWithEmoji(dst, "😀", nil, 0, 40, color.Black) // must not panic
	})

	t.Run("nil destination", func(t *testing.T) {
		DrawWithEmoji(nil, "😀", face, 0, 40, color.Black) // must not panic
	})

	t.Run("empty string", func(t *testing.T) {
		dst := image.NewRGBA(image.Rect(0, 0, 50, 50))
		DrawWithEmoji(dst, "", face, 0, 40, color.Black) // must not panic
	})

	t.Run("invalid UTF-8", func(t *testing.T) {
		dst := image.NewRGBA(image.Rect(0, 0, 200, 50))
		// Lone continuation byte between text and emoji.
		DrawWithEmoji(dst, "A\xff😀", face, 0, 40, color.Black)
		// 😀 follows "A" (8px) and U+FFFD (.notdef, 8px).
		if !pixelIsColor(dst, 20, 36, fakeGreen) {
			t.Error("emoji after invalid UTF-8 not drawn at expected position")
		}
	})

	t.Run("lone modifier", func(t *testing.T) {
		dst := image.NewRGBA(image.Rect(0, 0, 200, 50))
		DrawWithEmoji(dst, "\U0001F3FB😀", face, 0, 40, color.Black) // must not panic
		// The lone modifier has no glyph in the fake font: nothing drawn,
		// .notdef advance consumed.
		if !pixelIsColor(dst, 12, 36, fakeGreen) {
			t.Error("emoji after lone modifier not at expected position")
		}
	})

	t.Run("lone ZWJ and variation selector", func(t *testing.T) {
		dst := image.NewRGBA(image.Rect(0, 0, 200, 50))
		DrawWithEmoji(dst, "😀‍️", face, 0, 40, color.Black) // must not panic
	})

	t.Run("overlong sequence", func(t *testing.T) {
		dst := image.NewRGBA(image.Rect(0, 0, 600, 50))
		long := strings.Repeat("👨‍", 20) + "👩" // 41 codepoints
		seqBefore := f.sequenceCalls
		DrawWithEmoji(dst, long, face, 0, 40, color.Black) // must not panic
		// Overlong clusters skip whole-cluster resolution entirely.
		if f.sequenceCalls != seqBefore {
			t.Error("overlong cluster attempted whole-cluster resolution")
		}
		// Members are still rendered (per-member fallback).
		if f.bitmapCalls == 0 {
			t.Error("overlong cluster members not rendered")
		}
	})

	t.Run("unterminated ZWJ", func(t *testing.T) {
		dst := image.NewRGBA(image.Rect(0, 0, 200, 50))
		DrawWithEmoji(dst, "👨‍", face, 0, 40, color.Black) // must not panic
		if !pixelIsColor(dst, 4, 36, fakeBlue) {
			t.Error("base emoji before dangling ZWJ not drawn")
		}
	})
}

func TestDrawWithEmoji_NoColorTablesDelegates(t *testing.T) {
	// A font without color tables must behave identically to Draw.
	f := newFakeEmojiFont()
	f.glyphs['A'] = 40
	f.advUnits[40] = 500

	// Use a non-color parsed font: wrap the fake but report no tables.
	nc := &fakeNoColorFont{fakeEmojiFont: f}
	face := newFakeEmojiSource(nc).Face(16)
	dst := image.NewRGBA(image.Rect(0, 0, 200, 50))

	// Must not panic and must not touch color paths.
	DrawWithEmoji(dst, "AB😀", face, 0, 40, color.Black)
	if f.bitmapCalls != 0 {
		t.Error("non-color font used bitmap path")
	}
}

// fakeNoColorFont wraps fakeEmojiFont but reports no color tables.
type fakeNoColorFont struct {
	*fakeEmojiFont
}

func (f *fakeNoColorFont) HasColorTables() bool { return false }
