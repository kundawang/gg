package text

import (
	"context"
	"image/color"
	"image/draw"
)

// DrawAliased renders text to a destination image using binary (non-anti-aliased)
// coverage. Every pixel in the output is either fully transparent or fully opaque
// (alpha 0 or 255). This matches Skia's SkFont::Edging::kAlias behavior.
//
// The function uses GlyphMaskRasterizer.RasterizeAliased internally, which routes
// through NoAAFiller (integer scanline, binary coverage) instead of AnalyticFiller.
//
// Position (x, y) is the baseline origin (same semantics as Draw).
// Supports sourceFace only. For MultiFace and FilteredFace, this is a no-op —
// callers should fall back to Draw() for complex font stacks.
func DrawAliased(dst draw.Image, text string, face Face, x, y float64, col color.Color) {
	_ = DrawAliasedErr(dst, text, face, x, y, col)
}

// DrawAliasedErr renders aliased text like [DrawAliased] and returns the first
// per-glyph failure as a [*GlyphDrawError].
func DrawAliasedErr(dst draw.Image, text string, face Face, x, y float64, col color.Color) error {
	return drawAliasedWithContext(context.Background(), dst, text, face, x, y, col)
}

// DrawAliasedContext renders aliased text and stops when ctx is canceled,
// returning ctx.Err().
func DrawAliasedContext(ctx context.Context, dst draw.Image, text string, face Face, x, y float64, col color.Color) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return drawAliasedWithContext(ctx, dst, text, face, x, y, col)
}

func drawAliasedWithContext(ctx context.Context, dst draw.Image, text string, face Face, x, y float64, col color.Color) error {
	if text == "" || face == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	sf, ok := face.(*sourceFace)
	if !ok {
		return nil
	}

	text = expandTabs(text)

	if vars := sf.Variations(); len(vars) > 0 {
		return drawGlyphsVariable(ctx, dst, sf, text, x, y, col, vars, rasterModeAliased)
	}

	return drawGlyphs(ctx, dst, sf, text, x, y, col, rasterizeAliasedGlyph)
}
