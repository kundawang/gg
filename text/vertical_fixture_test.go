package text

import (
	"math"
	"testing"
)

// TestVerticalFixture_MixedLatinCJK emulates a 中英混排 column. goregular
// lacks CJK glyphs, but classification and orientation are pure Unicode
// operations: Latin and ASCII punctuation rotate, CJK ideographs and CJK
// punctuation stay upright.
func TestVerticalFixture_MixedLatinCJK(t *testing.T) {
	face := verticalTestFace(t)

	// "A中B。" — Latin A/B sideways, Han 中 and ideographic full stop upright.
	opts := LayoutOptions{Direction: DirectionTTB, Vertical: &VerticalOptions{}}
	layout := LayoutText("A中B。", face, opts)
	if len(layout.Columns) != 1 {
		t.Fatalf("expected 1 column, got %d", len(layout.Columns))
	}

	wantOrients := []VerticalOrient{
		VerticalOrientSideways, // A
		VerticalOrientUpright,  // 中 (notdef in goregular but classified CJK)
		VerticalOrientSideways, // B
		VerticalOrientUpright,  // 。
	}
	got := layout.Columns[0].Glyphs
	if len(got) != len(wantOrients) {
		t.Fatalf("expected %d glyphs, got %d", len(wantOrients), len(got))
	}
	for i, want := range wantOrients {
		if got[i].VerticalOrient != want {
			t.Errorf("glyph %d (cluster %d) orient = %s, want %s",
				i, got[i].Cluster, got[i].VerticalOrient, want)
		}
	}
}

// TestVerticalFixture_BracketPunctuation covers bracket rotation rules:
// fullwidth CJK brackets stay upright, ASCII brackets rotate with Latin.
func TestVerticalFixture_BracketPunctuation(t *testing.T) {
	face := verticalTestFace(t)
	opts := LayoutOptions{Direction: DirectionTTB, Vertical: &VerticalOptions{}}

	// ASCII brackets classify as punctuation and default sideways.
	ascii := LayoutText("(A)", face, opts)
	for _, g := range ascii.Columns[0].Glyphs {
		if g.VerticalOrient != VerticalOrientSideways {
			t.Errorf("ASCII bracket cluster %d should be sideways, got %s",
				g.Cluster, g.VerticalOrient)
		}
	}

	// Fullwidth brackets classify as CJK and stay upright in auto mode.
	fw := LayoutText("（A）", face, opts)
	got := fw.Columns[0].Glyphs
	want := []VerticalOrient{
		VerticalOrientUpright,  // （
		VerticalOrientSideways, // A
		VerticalOrientUpright,  // ）
	}
	if len(got) != len(want) {
		t.Fatalf("fullwidth: expected %d glyphs, got %d", len(want), len(got))
	}
	for i, w := range want {
		if got[i].VerticalOrient != w {
			t.Errorf("fullwidth glyph %d = %s, want %s", i, got[i].VerticalOrient, w)
		}
	}
}

// TestVerticalFixture_CombiningMarks verifies marks inherit the base
// orientation in both policy modes.
func TestVerticalFixture_CombiningMarks(t *testing.T) {
	face := verticalTestFace(t)

	// The shaper composes "é" into one precomposed glyph via ccmp, so use a
	// sequence the test font cannot compose: "a" + combining ring above.
	const input = "å"

	t.Run("sideways base", func(t *testing.T) {
		opts := LayoutOptions{Direction: DirectionTTB, Vertical: &VerticalOptions{}}
		layout := LayoutText(input, face, opts)
		for _, g := range layout.Columns[0].Glyphs {
			if g.VerticalOrient != VerticalOrientSideways {
				t.Errorf("cluster %d should inherit sideways, got %s",
					g.Cluster, g.VerticalOrient)
			}
		}
	})

	t.Run("upright base", func(t *testing.T) {
		opts := ttbUprightOptions()
		layout := LayoutText(input, face, opts)
		for _, g := range layout.Columns[0].Glyphs {
			if g.VerticalOrient != VerticalOrientUpright {
				t.Errorf("cluster %d should inherit upright, got %s",
					g.Cluster, g.VerticalOrient)
			}
		}
	})
}

// TestVerticalFixture_ReferenceCoordinatesSideways checks the exact
// geometric contract for a sideways column against hand-computed values:
// glyph Y equals the cumulative horizontal advance, X stays on the center
// line, and the column height equals the line's horizontal width.
func TestVerticalFixture_ReferenceCoordinatesSideways(t *testing.T) {
	face := verticalTestFace(t)

	horizontal := LayoutText("Ab", face, DefaultLayoutOptions())
	hGlyphs := horizontal.Lines[0].Glyphs

	opts := ttbSidewaysOptions()
	vertical := LayoutText("Ab", face, opts)
	col := &vertical.Columns[0]

	// Cumulative-Y reference: pen starts at 0, each step adds the prior
	// glyph's horizontal advance. X is absolute after column positioning:
	// the local cross-axis offset (shaped Y, 0 for plain text) plus the
	// column center line.
	cumY := 0.0
	for i, g := range col.Glyphs {
		if math.Abs(g.Y-cumY) > 1e-9 {
			t.Errorf("glyph %d Y = %f, reference %f", i, g.Y, cumY)
		}
		wantX := col.X + hGlyphs[i].Y
		if math.Abs(g.X-wantX) > 1e-9 {
			t.Errorf("glyph %d X = %f, reference %f", i, g.X, wantX)
		}
		cumY += hGlyphs[i].XAdvance
	}

	wantHeight := hGlyphs[len(hGlyphs)-1].X + hGlyphs[len(hGlyphs)-1].XAdvance
	if math.Abs(col.Height-wantHeight) > 1e-9 {
		t.Errorf("column height = %f, reference line width %f", col.Height, wantHeight)
	}

	// Absolute center line: single RTL column's right edge equals layout
	// width, so center = width - half column width.
	wantCenter := vertical.Width - col.Width/2
	if math.Abs(col.X-wantCenter) > 1e-9 {
		t.Errorf("column center X = %f, reference %f", col.X, wantCenter)
	}
}

// TestVerticalFixture_ReferenceCoordinatesUpright checks the em-square
// geometric contract for an all-upright column.
func TestVerticalFixture_ReferenceCoordinatesUpright(t *testing.T) {
	face := verticalTestFace(t)
	size := face.Size()

	layout := LayoutText("Ab", face, ttbUprightOptions())
	col := &layout.Columns[0]

	// Every glyph advances one em; glyph n starts at pen n*em with its
	// synthesized origin offset at (-em/2) in Y-down coordinates.
	for i, g := range col.Glyphs {
		wantY := float64(i)*size - size/2
		if math.Abs(g.Y-wantY) > 1e-9 {
			t.Errorf("upright glyph %d Y = %f, reference %f", i, g.Y, wantY)
		}
		if math.Abs(g.YAdvance-size) > 1e-9 {
			t.Errorf("upright glyph %d YAdvance = %f, reference %f", i, g.YAdvance, size)
		}
	}
}

// TestVerticalFixture_TTBBTTMirror verifies TTB and BTT columns have
// mirrored Y coordinates about the column midpoint and identical extents.
func TestVerticalFixture_TTBBTTMirror(t *testing.T) {
	face := verticalTestFace(t)

	ttb := LayoutText("Ab", face, ttbSidewaysOptions())
	bttOpts := LayoutOptions{
		Direction: DirectionBTT,
		Vertical:  &VerticalOptions{Orientation: VerticalOrientSideways},
	}
	btt := LayoutText("Ab", face, bttOpts)

	if len(ttb.Columns) != 1 || len(btt.Columns) != 1 {
		t.Fatalf("expected one column each: %d vs %d", len(ttb.Columns), len(btt.Columns))
	}
	if math.Abs(ttb.Columns[0].Height-btt.Columns[0].Height) > 1e-9 {
		t.Errorf("TTB/BTT heights differ: %f vs %f",
			ttb.Columns[0].Height, btt.Columns[0].Height)
	}

	h := ttb.Columns[0].Height
	for i := range ttb.Columns[0].Glyphs {
		ttbY := ttb.Columns[0].Glyphs[i].Y
		bttY := btt.Columns[0].Glyphs[i].Y
		if math.Abs((ttbY+bttY)-h) > 1e-9 {
			t.Errorf("glyph %d not mirrored: ttb=%f btt=%f sum=%f height=%f",
				i, ttbY, bttY, ttbY+bttY, h)
		}
	}
}

// TestVerticalFixture_BoundsAndColumnGap verifies layout bounds cover every
// glyph and that ColumnSpacing widens the block predictably.
func TestVerticalFixture_BoundsAndColumnGap(t *testing.T) {
	face := verticalTestFace(t)

	base := LayoutText("a\nb\nc", face, ttbSidewaysOptions())

	for _, col := range base.Columns {
		for _, g := range col.Glyphs {
			if g.X < 0 || g.X > base.Width {
				t.Errorf("glyph X=%f outside layout width %f", g.X, base.Width)
			}
			if g.Y < 0 || g.Y > col.Height {
				t.Errorf("glyph Y=%f outside column height %f", g.Y, col.Height)
			}
		}
	}

	spaced := ttbSidewaysOptions()
	spaced.Vertical.ColumnSpacing = 25
	spacedLayout := LayoutText("a\nb\nc", face, spaced)

	// Three columns: two gaps added to the block width.
	wantExtra := 2 * 25.0
	if math.Abs(spacedLayout.Width-base.Width-wantExtra) > 1e-9 {
		t.Errorf("spacing delta = %f, want %f (base=%f spaced=%f)",
			spacedLayout.Width-base.Width, wantExtra, base.Width, spacedLayout.Width)
	}
}
