package text

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"testing"
)

// verticalTestFace builds the standard goregular face at 16px for vertical
// layout tests. goregular has no vhea/vmtx tables, exercising the
// synthesized em-square fallback path.
func verticalTestFace(t *testing.T) Face {
	t.Helper()
	return layoutTestFace(t)
}

// ttbSidewaysOptions returns conventional vertical options where the
// Latin test font renders every glyph sideways.
func ttbSidewaysOptions() LayoutOptions {
	return LayoutOptions{
		Direction:   DirectionTTB,
		LineSpacing: 1.0,
		Vertical:    &VerticalOptions{Orientation: VerticalOrientSideways},
	}
}

// ttbUprightOptions forces every class upright to emulate a CJK font with
// upright ideographs (goregular lacks CJK coverage).
func ttbUprightOptions() LayoutOptions {
	return LayoutOptions{
		Direction: DirectionTTB,
		Vertical: &VerticalOptions{
			ClassModes: map[VerticalClass]VerticalOrient{
				VerticalClassLatin:       VerticalOrientUpright,
				VerticalClassPunctuation: VerticalOrientUpright,
			},
		},
	}
}

// TestVerticalLayout_BasicStructure verifies that vertical mode produces
// columns rather than lines and reports the Vertical flag.
func TestVerticalLayout_BasicStructure(t *testing.T) {
	face := verticalTestFace(t)

	layout := LayoutText("Ab", face, ttbSidewaysOptions())

	if !layout.Vertical {
		t.Fatal("layout should be marked vertical")
	}
	if len(layout.Lines) != 0 {
		t.Errorf("vertical layout must not contain lines, got %d", len(layout.Lines))
	}
	if len(layout.Columns) != 1 {
		t.Fatalf("expected 1 column, got %d", len(layout.Columns))
	}
	col := &layout.Columns[0]
	if len(col.Glyphs) != 2 {
		t.Fatalf("expected 2 glyphs, got %d", len(col.Glyphs))
	}
	if col.Height <= 0 || col.Width <= 0 {
		t.Errorf("column extent must be positive, got W=%f H=%f", col.Width, col.Height)
	}
	if layout.Width < col.Width || layout.Height < col.Height {
		t.Errorf("layout bounds %fx%f smaller than column %fx%f",
			layout.Width, layout.Height, col.Width, col.Height)
	}
}

// TestVerticalLayout_GlyphsAdvanceDownward verifies the block-axis pen:
// sideways glyph Y positions strictly increase by their shaped advances.
func TestVerticalLayout_GlyphsAdvanceDownward(t *testing.T) {
	face := verticalTestFace(t)

	horizontal := LayoutText("Ab", face, DefaultLayoutOptions())
	hGlyphs := horizontal.Lines[0].Glyphs

	layout := LayoutText("Ab", face, ttbSidewaysOptions())
	glyphs := layout.Columns[0].Glyphs

	// First glyph sits at the column top.
	if glyphs[0].Y != 0 {
		t.Errorf("first glyph Y = %f, want 0", glyphs[0].Y)
	}
	// Each subsequent glyph is offset downward by the previous advance.
	wantY := hGlyphs[0].XAdvance
	if math.Abs(glyphs[1].Y-wantY) > 1e-9 {
		t.Errorf("second glyph Y = %f, want %f", glyphs[1].Y, wantY)
	}
	// Column height equals the total horizontal advance.
	wantHeight := hGlyphs[1].X + hGlyphs[1].XAdvance
	if math.Abs(layout.Columns[0].Height-wantHeight) > 1e-9 {
		t.Errorf("column height = %f, want %f", layout.Columns[0].Height, wantHeight)
	}
}

// TestVerticalLayout_ClustersPreserved verifies logical cluster indices
// survive the vertical pipeline unchanged from horizontal shaping.
func TestVerticalLayout_ClustersPreserved(t *testing.T) {
	face := verticalTestFace(t)

	horizontal := LayoutText("Hello", face, DefaultLayoutOptions())
	layout := LayoutText("Hello", face, ttbSidewaysOptions())

	want := horizontal.Lines[0].Glyphs
	got := layout.Columns[0].Glyphs
	if len(want) != len(got) {
		t.Fatalf("glyph count mismatch: horizontal %d, vertical %d", len(want), len(got))
	}
	for i := range want {
		if got[i].Cluster != want[i].Cluster {
			t.Errorf("glyph %d cluster: vertical %d, horizontal %d",
				i, got[i].Cluster, want[i].Cluster)
		}
		if got[i].GID != want[i].GID {
			t.Errorf("glyph %d GID: vertical %d, horizontal %d",
				i, got[i].GID, want[i].GID)
		}
	}
}

// TestVerticalLayout_HardBreaksMakeColumns verifies each newline starts a
// new column and TTB columns progress right-to-left by default.
func TestVerticalLayout_HardBreaksMakeColumns(t *testing.T) {
	face := verticalTestFace(t)

	layout := LayoutText("a\nb\nc", face, ttbSidewaysOptions())
	if len(layout.Columns) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(layout.Columns))
	}

	// RTL flow: each subsequent center line is further left.
	for i := 1; i < len(layout.Columns); i++ {
		if layout.Columns[i].X >= layout.Columns[i-1].X {
			t.Errorf("column %d X=%f should be left of column %d X=%f (RTL flow)",
				i, layout.Columns[i].X, i-1, layout.Columns[i-1].X)
		}
	}
}

// TestVerticalLayout_BTT verifies bottom-to-top ordering: glyphs grow
// upward within the column and columns progress left-to-right.
func TestVerticalLayout_BTT(t *testing.T) {
	face := verticalTestFace(t)
	opts := LayoutOptions{
		Direction: DirectionBTT,
		Vertical:  &VerticalOptions{Orientation: VerticalOrientSideways},
	}

	layout := LayoutText("Ab\nCd", face, opts)
	if len(layout.Columns) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(layout.Columns))
	}

	// LTR column flow for BTT.
	if layout.Columns[1].X <= layout.Columns[0].X {
		t.Errorf("BTT columns should progress LTR: %f then %f",
			layout.Columns[0].X, layout.Columns[1].X)
	}

	// Within a BTT column, the first logical glyph has the largest Y.
	glyphs := layout.Columns[0].Glyphs
	if glyphs[0].Y <= glyphs[1].Y {
		t.Errorf("BTT first glyph Y=%f should be below second Y=%f",
			glyphs[0].Y, glyphs[1].Y)
	}
}

// TestVerticalLayout_SynthesizedMetrics verifies the em-square fallback
// when vhea/vmtx are absent: upright advance equals the font size and the
// vertical origin sits at half an em.
func TestVerticalLayout_SynthesizedMetrics(t *testing.T) {
	face := verticalTestFace(t)
	size := face.Size()

	layout := LayoutText("Ab", face, ttbUprightOptions())
	col := &layout.Columns[0]

	for _, g := range col.Glyphs {
		if g.VerticalOrient != VerticalOrientUpright {
			t.Fatalf("expected upright glyph, got %s", g.VerticalOrient)
		}
		if math.Abs(g.YAdvance-size) > 1e-9 {
			t.Errorf("upright YAdvance = %f, want font size %f", g.YAdvance, size)
		}
	}
	// Two em-square advances.
	if math.Abs(col.Height-2*size) > 1e-9 {
		t.Errorf("column height = %f, want %f", col.Height, 2*size)
	}
}

// TestVerticalLayout_OrientationRules verifies auto classification and
// configurable per-class overrides.
func TestVerticalLayout_OrientationRules(t *testing.T) {
	face := verticalTestFace(t)

	// Auto (mixed): Latin sideways; an explicit upright override on Latin
	// flips it. Mark inherits its base's orientation.
	t.Run("auto Latin sideways", func(t *testing.T) {
		opts := LayoutOptions{Direction: DirectionTTB, Vertical: &VerticalOptions{}}
		layout := LayoutText("Ab", face, opts)
		for _, g := range layout.Columns[0].Glyphs {
			if g.VerticalOrient != VerticalOrientSideways {
				t.Errorf("Latin glyph should be sideways, got %s", g.VerticalOrient)
			}
		}
	})

	t.Run("global sideways policy", func(t *testing.T) {
		opts := ttbSidewaysOptions()
		layout := LayoutText("Ab", face, opts)
		for _, g := range layout.Columns[0].Glyphs {
			if g.VerticalOrient != VerticalOrientSideways {
				t.Errorf("global sideways policy violated: %s", g.VerticalOrient)
			}
		}
	})

	t.Run("class override upright", func(t *testing.T) {
		layout := LayoutText("Ab", face, ttbUprightOptions())
		for _, g := range layout.Columns[0].Glyphs {
			if g.VerticalOrient != VerticalOrientUpright {
				t.Errorf("class override should force upright, got %s", g.VerticalOrient)
			}
		}
	})
}

// TestClassifyVerticalRune covers the five orientation classes.
func TestClassifyVerticalRune(t *testing.T) {
	tests := []struct {
		name string
		r    rune
		want VerticalClass
	}{
		{"latin letter", 'A', VerticalClassLatin},
		{"latin digit", '9', VerticalClassLatin},
		{"han ideograph", '中', VerticalClassCJK},
		{"hiragana", 'あ', VerticalClassCJK},
		{"fullwidth bracket", '（', VerticalClassCJK},
		{"ideographic full stop", '。', VerticalClassCJK},
		{"ascii bracket", '(', VerticalClassPunctuation},
		{"combining acute", '́', VerticalClassMark},
		{"emoji", '😀', VerticalClassEmoji},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyVerticalRune(tt.r); got != tt.want {
				t.Errorf("ClassifyVerticalRune(%q) = %s, want %s", tt.r, got, tt.want)
			}
		})
	}
}

// TestVerticalLayout_EmptyParagraph verifies empty paragraphs reserve a
// column with natural line height but no glyphs.
func TestVerticalLayout_EmptyParagraph(t *testing.T) {
	face := verticalTestFace(t)

	layout := LayoutText("a\n\nb", face, ttbSidewaysOptions())
	if len(layout.Columns) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(layout.Columns))
	}
	middle := &layout.Columns[1]
	if len(middle.Glyphs) != 0 {
		t.Errorf("empty column should have no glyphs, got %d", len(middle.Glyphs))
	}
	if middle.Height <= 0 {
		t.Error("empty column should retain line-height height")
	}
}

// TestVerticalLayout_NonFiniteColumnHeight verifies NaN, Inf and negative
// MaxWidth disable column wrapping without crashing.
func TestVerticalLayout_NonFiniteColumnHeight(t *testing.T) {
	face := verticalTestFace(t)
	text := "AbCdEfGh"

	for _, w := range []float64{0, -5, math.NaN(), math.Inf(1), math.Inf(-1)} {
		opts := ttbSidewaysOptions()
		opts.MaxWidth = w
		layout := LayoutText(text, face, opts)
		if len(layout.Columns) != 1 {
			t.Errorf("MaxWidth=%v: expected single unbroken column, got %d", w, len(layout.Columns))
		}
	}
}

// TestVerticalLayout_ColumnWrapping verifies height-constrained wrapping
// produces multiple columns whose heights respect the constraint except
// for unavoidable single-glyph overflow.
func TestVerticalLayout_ColumnWrapping(t *testing.T) {
	face := verticalTestFace(t)

	opts := ttbSidewaysOptions()
	opts.MaxWidth = 20 // tight column height
	opts.WrapMode = WrapWordChar

	layout := LayoutText("AbCdEfGhIj", face, opts)
	if len(layout.Columns) < 2 {
		t.Fatalf("expected wrapped columns, got %d", len(layout.Columns))
	}
	total := 0
	for _, col := range layout.Columns {
		total += len(col.Glyphs)
	}
	if total != 10 {
		t.Errorf("wrapping lost glyphs: %d of 10", total)
	}
}

// TestVerticalLayout_WrapModes verifies WrapNone disables wrapping even
// with a finite constraint.
func TestVerticalLayout_WrapModes(t *testing.T) {
	face := verticalTestFace(t)

	opts := ttbSidewaysOptions()
	opts.MaxWidth = 10
	opts.WrapMode = WrapNone

	layout := LayoutText("AbCdEfGh", face, opts)
	if len(layout.Columns) != 1 {
		t.Errorf("WrapNone should keep one column, got %d", len(layout.Columns))
	}
}

// TestVerticalLayout_Cancellation verifies context cancellation propagates.
func TestVerticalLayout_Cancellation(t *testing.T) {
	face := verticalTestFace(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	layout, err := LayoutTextWithContext(ctx, "Ab", face, ttbSidewaysOptions())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if layout != nil {
		t.Errorf("canceled layout should be nil, got %+v", layout)
	}
}

// TestVerticalLayout_LongText verifies long input stays stable and every
// glyph survives into a column.
func TestVerticalLayout_LongText(t *testing.T) {
	face := verticalTestFace(t)

	runes := make([]rune, 2000)
	for i := range runes {
		runes[i] = 'a'
	}

	layout := LayoutText(string(runes), face, ttbSidewaysOptions())
	total := 0
	for _, col := range layout.Columns {
		for _, g := range col.Glyphs {
			if math.IsNaN(g.X) || math.IsNaN(g.Y) || math.IsInf(g.X, 0) || math.IsInf(g.Y, 0) {
				t.Fatalf("non-finite glyph coordinate: %+v", g)
			}
			total++
		}
	}
	if total != 2000 {
		t.Errorf("expected 2000 glyphs, got %d", total)
	}
}

// TestVerticalLayout_RunsRebuilt verifies per-column runs keep face, size
// and direction and cover every glyph.
func TestVerticalLayout_RunsRebuilt(t *testing.T) {
	face := verticalTestFace(t)

	layout := LayoutText("Ab", face, ttbSidewaysOptions())
	col := &layout.Columns[0]
	if len(col.Runs) == 0 {
		t.Fatal("expected at least one run")
	}
	total := 0
	for i := range col.Runs {
		run := &col.Runs[i]
		if run.Face != face {
			t.Error("run lost its face")
		}
		if run.Size != face.Size() {
			t.Errorf("run size = %f, want %f", run.Size, face.Size())
		}
		total += len(run.Glyphs)
	}
	if total != len(col.Glyphs) {
		t.Errorf("run glyphs %d != column glyphs %d", total, len(col.Glyphs))
	}
}

// TestVerticalLayout_ColumnFlow verifies explicit LTR/RTL flow overrides.
func TestVerticalLayout_ColumnFlow(t *testing.T) {
	face := verticalTestFace(t)

	ltr := ttbSidewaysOptions()
	ltr.Vertical.Flow = ColumnFlowLeftToRight
	layoutLTR := LayoutText("a\nb", face, ltr)
	if layoutLTR.Columns[1].X <= layoutLTR.Columns[0].X {
		t.Error("explicit LTR flow should move columns rightward")
	}

	rtl := ttbSidewaysOptions()
	rtl.Vertical.Flow = ColumnFlowRightToLeft
	layoutRTL := LayoutText("a\nb", face, rtl)
	if layoutRTL.Columns[1].X >= layoutRTL.Columns[0].X {
		t.Error("explicit RTL flow should move columns leftward")
	}
}

// TestVerticalLayout_ColumnSpacing verifies extra spacing widens the pitch.
func TestVerticalLayout_ColumnSpacing(t *testing.T) {
	face := verticalTestFace(t)

	base := LayoutText("a\nb", face, ttbSidewaysOptions())

	spaced := ttbSidewaysOptions()
	spaced.Vertical.ColumnSpacing = 30
	layoutSpaced := LayoutText("a\nb", face, spaced)

	if layoutSpaced.Width <= base.Width {
		t.Errorf("spaced width %f should exceed base %f", layoutSpaced.Width, base.Width)
	}
}

// TestVerticalLayout_NilOptionsSafe verifies a vertical config with the
// zero value behaves like DefaultVerticalOptions.
func TestVerticalLayout_NilOptionsSafe(t *testing.T) {
	face := verticalTestFace(t)

	opts := LayoutOptions{Direction: DirectionTTB, Vertical: &VerticalOptions{}}
	layout := LayoutText("Ab", face, opts)
	if len(layout.Columns) != 1 {
		t.Fatalf("expected 1 column, got %d", len(layout.Columns))
	}
}

// TestVerticalLayout_EmptyAndNilFace mirrors the horizontal edge-case
// guarantees for the vertical pipeline.
func TestVerticalLayout_EmptyAndNilFace(t *testing.T) {
	face := verticalTestFace(t)

	empty := LayoutText("", face, ttbSidewaysOptions())
	if empty == nil || !empty.Vertical || len(empty.Columns) != 0 {
		t.Errorf("empty vertical input should return an empty vertical layout")
	}
	if empty.Width != 0 || empty.Height != 0 {
		t.Errorf("empty layout bounds should be zero, got %fx%f", empty.Width, empty.Height)
	}

	nilFace := LayoutText("Ab", nil, ttbSidewaysOptions())
	if nilFace == nil || len(nilFace.Columns) != 0 {
		t.Errorf("nil face should return an empty layout, got %+v", nilFace)
	}
}

// TestVerticalLayout_HorizontalUnchanged proves the vertical option is
// opt-in: without it the pipeline still produces horizontal lines.
func TestVerticalLayout_HorizontalUnchanged(t *testing.T) {
	face := verticalTestFace(t)

	// Vertical pointer present but horizontal direction: treated as
	// horizontal because IsVerticalLayout requires both.
	ignored := LayoutOptions{
		Direction: DirectionLTR,
		Vertical:  &VerticalOptions{},
	}
	layout := LayoutText("Ab", face, ignored)
	if layout.Vertical {
		t.Error("LTR direction with Vertical set must not trigger vertical layout")
	}
	if len(layout.Lines) != 1 || len(layout.Columns) != 0 {
		t.Errorf("expected one horizontal line, got lines=%d cols=%d",
			len(layout.Lines), len(layout.Columns))
	}
}

// TestVerticalLayout_DrawPixels renders a vertical column and verifies ink
// lands in the expected vertical band (top-to-bottom), contrasting with a
// horizontal line whose ink spans horizontally.
func TestVerticalLayout_DrawPixels(t *testing.T) {
	face := verticalTestFace(t)

	const size = 100
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for i := range img.Pix {
		img.Pix[i] = 255
	}

	layout := LayoutText("Ab", face, ttbSidewaysOptions())
	DrawLayout(img, layout, face, 10, 5, color.Black)

	// Collect ink bounds.
	minX, minY, maxX, maxY := size, size, 0, 0
	ink := 0
	for y := range size {
		for x := range size {
			if img.RGBAAt(x, y).R >= 128 {
				continue
			}
			ink++
			minX = min(minX, x)
			maxX = max(maxX, x)
			minY = min(minY, y)
			maxY = max(maxY, y)
		}
	}
	if ink == 0 {
		t.Fatal("vertical render produced no ink")
	}
	// Two rotated glyphs stacked downward: vertical extent exceeds width.
	if maxY-minY <= maxX-minX {
		t.Errorf("vertical ink should be taller than wide: bbox %dx%d",
			maxX-minX, maxY-minY)
	}
}
