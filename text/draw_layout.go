// Layout rasterization — draws a positioned Layout onto a draw.Image.
//
// Horizontal layouts reuse the per-glyph mask pipeline from draw.go.
// Vertical layouts additionally rotate sideways glyph runs 90° clockwise
// around the column center line; upright glyphs are drawn unchanged at
// their vertical-origin positions.
package text

import (
	"image"
	"image/color"
	"image/draw"
	"math"
)

// DrawLayout renders a laid-out text block onto dst at the given origin.
// The origin corresponds to the layout's top-left corner: horizontal lines
// keep their baseline offsets internally, vertical columns start at x.
//
// Only *sourceFace faces carry rasterizable outlines; glyphs from other
// face implementations (or zero-GID space glyphs) are skipped while their
// advances remain reflected in the layout coordinates.
func DrawLayout(dst draw.Image, layout *Layout, face Face, x, y float64, col color.Color) {
	if layout == nil || face == nil {
		return
	}
	sf, ok := face.(*sourceFace)
	if !ok {
		return
	}
	if layout.Vertical {
		drawVerticalLayout(dst, layout, sf, x, y, col)
		return
	}
	drawHorizontalLayout(dst, layout, sf, x, y, col)
}

// drawHorizontalLayout renders horizontal lines. Line.Y is the baseline
// offset from the layout top; glyph X/Y are relative to that baseline.
func drawHorizontalLayout(dst draw.Image, layout *Layout, sf *sourceFace, ox, oy float64, col color.Color) {
	rast := NewGlyphMaskRasterizer()
	src := image.NewUniform(col)
	parsed := sf.source.Parsed()

	for i := range layout.Lines {
		line := &layout.Lines[i]
		for j := range line.Glyphs {
			g := &line.Glyphs[j]
			if g.GID == 0 {
				continue
			}
			drawMaskAt(dst, rast, parsed, sf, g, ox+g.X, oy+line.Y+g.Y, src)
		}
	}
}

// drawVerticalLayout renders columns. Upright glyphs draw normally;
// sideways glyphs are rotated 90° clockwise around the glyph origin
// (which lies on the column center line).
func drawVerticalLayout(dst draw.Image, layout *Layout, sf *sourceFace, ox, oy float64, col color.Color) {
	rast := NewGlyphMaskRasterizer()
	src := image.NewUniform(col)
	parsed := sf.source.Parsed()

	for i := range layout.Columns {
		column := &layout.Columns[i]
		for j := range column.Glyphs {
			g := &column.Glyphs[j]
			if g.GID == 0 {
				continue
			}
			if g.VerticalOrient == VerticalOrientSideways {
				drawRotatedMask(dst, rast, parsed, sf, g, ox, oy, src)
				continue
			}
			drawMaskAt(dst, rast, parsed, sf, g, ox+g.X, oy+g.Y, src)
		}
	}
}

// drawMaskAt rasterizes one glyph mask and composites it at a baseline
// position (px, py), mirroring drawGlyphs' bearing math.
func drawMaskAt(dst draw.Image, rast *GlyphMaskRasterizer, parsed ParsedFont, sf *sourceFace,
	g *ShapedGlyph, px, py float64, src *image.Uniform) {
	intX := math.Floor(px)
	intY := math.Floor(py)
	subX := px - intX
	subY := py - intY

	result, err := rast.RasterizeHinted(parsed, g.GID, sf.size, subX, subY, sf.config.hinting)
	if err != nil || result == nil {
		return
	}

	maskImg := &image.Alpha{
		Pix:    result.Mask,
		Stride: result.Width,
		Rect:   image.Rect(0, 0, result.Width, result.Height),
	}
	dstX := int(intX) + int(math.Round(float64(result.BearingX)))
	dstY := int(intY) - int(math.Round(float64(result.BearingY)))
	destRect := image.Rect(dstX, dstY, dstX+result.Width, dstY+result.Height)
	draw.DrawMask(dst, destRect, src, image.Point{}, maskImg, image.Point{}, draw.Over)
}

// drawRotatedMask rasterizes a sideways glyph and composites the mask
// rotated 90° clockwise about the glyph origin (px, py).
//
// Mask-local pixel (mx, my) maps as follows: the unrotated mask pixel sits
// at (px + BearingX + mx, py - BearingY + my); after a clockwise rotation
// about (px, py), (dx, dy) -> (-dy, dx).
func drawRotatedMask(dst draw.Image, rast *GlyphMaskRasterizer, parsed ParsedFont, sf *sourceFace,
	g *ShapedGlyph, ox, oy float64, src *image.Uniform) {
	px := ox + g.X
	py := oy + g.Y

	result, err := rast.RasterizeHinted(parsed, g.GID, sf.size, 0, 0, sf.config.hinting)
	if err != nil || result == nil {
		return
	}

	bearingX := math.Round(float64(result.BearingX))
	bearingY := math.Round(float64(result.BearingY))
	originIX := int(math.Round(px))
	originIY := int(math.Round(py))

	bounds := dst.Bounds()
	srcColor := src.At(0, 0)

	for my := 0; my < result.Height; my++ {
		for mx := 0; mx < result.Width; mx++ {
			alpha := result.Mask[my*result.Width+mx]
			if alpha == 0 {
				continue
			}
			// Unrotated offset from the glyph origin.
			dx := bearingX + float64(mx)
			dy := -bearingY + float64(my)
			// Clockwise 90°: (dx, dy) -> (-dy, dx).
			rx := originIX + int(math.Round(-dy))
			ry := originIY + int(math.Round(dx))
			if !image.Pt(rx, ry).In(bounds) {
				continue
			}
			blendAlpha(dst, rx, ry, srcColor, alpha)
		}
	}
}

// blendAlpha composites srcColor with the given mask coverage over the
// destination pixel using standard source-over blending.
func blendAlpha(dst draw.Image, x, y int, srcColor color.Color, coverage byte) {
	sr, sg, sb, sa := srcColor.RGBA()
	// srcColor is a color.Color (16-bit channels); fold in mask coverage.
	alpha := float64(coverage) / 255.0
	sa16 := float64(sa) / 65535.0 * alpha
	if sa16 <= 0 {
		return
	}

	dr, dg, db, _ := dst.At(x, y).RGBA()
	outR := lerpChannel(float64(dr)/65535.0, float64(sr)/65535.0, sa16)
	outG := lerpChannel(float64(dg)/65535.0, float64(sg)/65535.0, sa16)
	outB := lerpChannel(float64(db)/65535.0, float64(sb)/65535.0, sa16)

	dst.Set(x, y, color.NRGBA{
		R: uint8(math.Round(outR * 255)),
		G: uint8(math.Round(outG * 255)),
		B: uint8(math.Round(outB * 255)),
		A: 255,
	})
}

// lerpChannel blends dst and src channels (0..1) by source alpha.
func lerpChannel(dst, src, alpha float64) float64 {
	return src*alpha + dst*(1-alpha)
}
