package text

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"reflect"
)

// GlyphDrawError reports the first glyph that failed to render.
//
// Draw, DrawAliased and their error-aware variants render as much as they can
// and report the first failure rather than aborting mid-string. GID and Size
// identify the glyph and ppem; Rune is the source rune (or -1 when the glyph
// could not be resolved, e.g. a closed FontSource). Err is the underlying
// cause and supports errors.Is/As.
type GlyphDrawError struct {
	// GID is the glyph index that failed (0 when unresolvable).
	GID GlyphID

	// Rune is the source rune for GID, or -1 when unknown.
	Rune rune

	// Size is the font size in ppem (device pixels per em) used for GID.
	Size float64

	// Err is the underlying cause.
	Err error
}

// Error implements error.
func (e *GlyphDrawError) Error() string {
	return fmt.Sprintf("text: failed to draw glyph gid=%d rune=%U size=%.2fpx: %v",
		e.GID, e.Rune, e.Size, e.Err)
}

// Unwrap exposes the underlying cause.
func (e *GlyphDrawError) Unwrap() error { return e.Err }

// Draw renders text to a destination image.
// Position (x, y) is the baseline origin.
// Supports sourceFace, MultiFace, and FilteredFace.
//
// Draw best-effort renders and discards per-glyph errors. Use [DrawErr] when
// the first failing glyph (and its size) must be reported, or [DrawContext]
// for cancellation.
func Draw(dst draw.Image, text string, face Face, x, y float64, col color.Color) {
	_ = DrawErr(dst, text, face, x, y, col)
}

// DrawErr renders text like [Draw] and returns the first per-glyph failure as
// a [*GlyphDrawError]. Rendering continues past a failing glyph so the output
// matches [Draw] exactly; only the report differs.
//
// A closed FontSource yields a *GlyphDrawError wrapping
// [ErrFontSourceClosed] instead of reading released font data.
func DrawErr(dst draw.Image, text string, face Face, x, y float64, col color.Color) error {
	return drawWithContext(context.Background(), dst, text, face, x, y, col)
}

// DrawContext renders text like [Draw] but stops when ctx is canceled.
// Cancellation is checked before the first glyph and before every glyph.
// It returns ctx.Err() on cancellation, or the first per-glyph error.
//
// DrawContext never starts a goroutine, so canceling cannot leak one.
func DrawContext(ctx context.Context, dst draw.Image, text string, face Face, x, y float64, col color.Color) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return drawWithContext(ctx, dst, text, face, x, y, col)
}

func drawWithContext(ctx context.Context, dst draw.Image, text string, face Face, x, y float64, col color.Color) error {
	if text == "" || face == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Expand tabs to spaces for bitmap rendering.
	// font.Drawer maps \t to .notdef (tofu) because fonts lack a tab glyph.
	// Tab = globalTabWidth spaces (default: 8, matching CSS/Pango/POSIX).
	text = expandTabs(text)

	switch f := face.(type) {
	case *sourceFace:
		return drawSourceFaceContext(ctx, dst, text, f, x, y, col)
	case *MultiFace:
		drawMultiFace(dst, text, f, x, y, col)
	case *FilteredFace:
		drawFilteredFace(dst, text, f, x, y, col)
	}
	return ctx.Err()
}

// glyphRasterMode selects the rasterization coverage mode.
// Outline extraction and rasterization mode are orthogonal
// concerns (Skia pattern: SkFont::Edging is independent of font variations).
type glyphRasterMode int

const (
	rasterModeAA      glyphRasterMode = iota // 256-level analytic AA coverage
	rasterModeAliased                        // binary 0/255 coverage (Skia kAlias)
)

// glyphRasterizeFunc is the per-glyph rasterization callback used by drawGlyphs.
// It abstracts the difference between RasterizeHinted (256-level AA) and
// RasterizeAliased (binary coverage), allowing drawSourceFace and DrawAliased
// to share the glyph iteration and compositing loop.
type glyphRasterizeFunc func(
	rast *GlyphMaskRasterizer,
	pf ParsedFont,
	gid GlyphID,
	ppem float64,
	subpixelX, subpixelY float64,
	hinting Hinting,
) (*GlyphMaskResult, error)

// drawGlyphs is the shared per-glyph rendering loop for drawSourceFace and
// DrawAliased. Each glyph is individually rasterized via the provided callback,
// then composited at its precise subpixel position using draw.DrawMask.
//
// When TT bytecode hinting is active, glyph positions are computed from
// hinted advances (phantom[1].x - phantom[0].x after TT interpreter runs)
// rather than unhinted hmtx advances. This prevents outline/advance mismatch
// where hinted outlines are wider or narrower than the raw advance, causing
// letters to merge or have gaps at certain ppem values (e.g., 16px Segoe UI).
//
// This matches skrifa/FreeType/Skia: when TT hinting runs on a glyph, the
// hinted advance replaces the raw hmtx advance for positioning.
func drawGlyphs(
	ctx context.Context,
	dst draw.Image,
	sf *sourceFace,
	text string,
	x, y float64,
	col color.Color,
	rasterize glyphRasterizeFunc,
) error {
	if vars := sf.Variations(); len(vars) > 0 {
		return drawGlyphsVariable(ctx, dst, sf, text, x, y, col, vars, rasterModeAA)
	}

	parsed, err := sf.source.ParsedErr()
	if err != nil {
		return firstGlyphError(text, sf.size, err)
	}
	ppem := sf.size
	hinting := sf.config.hinting

	// Check if TT bytecode hinting is available for this font.
	// When it is, we need to use hinted advances for glyph positioning
	// to match the hinted outlines. Without this, the outline shape
	// (grid-fitted by TT interpreter) disagrees with the cursor advance
	// (raw hmtx), causing letters to merge or gap at certain sizes.
	var ttCache *ttHintCache
	if hinting != HintingNone {
		if ownFont, ok := parsed.(*ownParsedFont); ok {
			ttCache = ownFont.loadTTHintCache()
		}
	}

	rast := NewGlyphMaskRasterizer()
	src := image.NewUniform(col)

	var firstErr error
	advanceX := 0.0
	for glyph := range sf.Glyphs(text) {
		if err := ctx.Err(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
		if glyph.GID == 0 {
			// Space and other no-outline glyphs: use unhinted advance.
			// These have no TT bytecode and phantom points would be trivial.
			advanceX += glyph.Advance
			continue
		}

		// Position glyph using our tracked advance (which may be hinted).
		glyphX := x + advanceX
		glyphY := y + glyph.Y

		intX := math.Floor(glyphX)
		intY := math.Floor(glyphY)
		subpixelX := glyphX - intX
		subpixelY := glyphY - intY

		result, rErr := rasterizeCachedOutline(rasterize, rast, sf.source, parsed, glyph.GID, ppem, subpixelX, subpixelY, hinting)
		if rErr != nil {
			if firstErr == nil {
				firstErr = &GlyphDrawError{GID: glyph.GID, Rune: glyph.Rune, Size: ppem, Err: rErr}
			}
			advanceX += hintedOrRawAdvance(ttCache, glyph, ppem)
			continue
		}
		if result == nil {
			advanceX += hintedOrRawAdvance(ttCache, glyph, ppem)
			continue
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

		// Advance cursor using hinted advance when TT hinting is active.
		advanceX += hintedOrRawAdvance(ttCache, glyph, ppem)
	}
	return firstErr
}

// hintedOrRawAdvance returns the TT hinted advance for a glyph if available,
// otherwise falls back to the unhinted advance from the Glyphs() iterator.
//
// When TT bytecode hinting is active, the hinted advance (from phantom
// points after TT interpreter execution) matches the hinted outline shape.
// Using unhinted advances with hinted outlines causes positioning errors
// because the outline is grid-fitted but the advance is not.
//
// Reference: skrifa ScaledOutline::advance_width — returns hinted advance
// when hinting is active, raw advance otherwise.
func hintedOrRawAdvance(ttCache *ttHintCache, glyph Glyph, ppem float64) float64 {
	if ttCache != nil {
		if adv, ok := ttCache.hintedAdvanceWidth(uint16(glyph.GID), int32(ppem)); ok {
			return adv
		}
	}
	return glyph.Advance
}

// drawGlyphsVariable renders text using the own parser's gvar path for outline
// extraction with full hinting support. This path matches skrifa's unified
// load_simple architecture: gvar deltas are applied to unscaled points BEFORE
// scaling and TT bytecode hinting.
//
// Hinting priority (same as static fonts):
//  1. TT bytecode hinting with gvar-varied unscaled points
//  2. Auto-hinter on the gvar-varied outline
//  3. Grid-fit fallback
//
// Advance priority (FreeType ttgload.c:964-977):
//
//	HVAR present → use HVAR advance (fractional, matches Face.Advance)
//	HVAR absent  → use outline.Advance (TT-hinted phantom points)
//
// TT bytecode hinting grid-fits phantom points to integer pixels, but HVAR
// provides the authoritative variable-font advance. Using TT-hinted phantom
// advances causes measurement/rendering mismatch (#405).
//
// The mode parameter selects coverage computation (Skia pattern: outline source
// and rasterization mode are orthogonal — variable fonts don't affect AA choice).
func drawGlyphsVariable(
	ctx context.Context,
	dst draw.Image,
	sf *sourceFace,
	text string,
	x, y float64,
	col color.Color,
	variations []FontVariation,
	mode glyphRasterMode,
) error {
	source := sf.source
	parsed, err := source.ParsedErr()
	if err != nil {
		return firstGlyphError(text, sf.size, err)
	}
	// Production sources use the own parser's gvar path. Parser doubles are
	// allowed too, so their geometry and cache accounting are exercised under
	// variations (ADR-054) as well.
	if _, isOwn := parsed.(*ownParsedFont); !isOwn {
		if _, isDouble := parsed.(outlineSource); !isDouble {
			return nil
		}
	}

	ppem := sf.size
	hinting := sf.config.hinting
	rast := NewGlyphMaskRasterizer()
	src := image.NewUniform(col)

	// HVAR takes priority over TT-hinted phantom advances for positioning.
	// FreeType ttgload.c:964-977: when HVAR present and hinting active,
	// discard TT-hinted phantoms and use HVAR-adjusted advance instead.
	varProv, _ := parsed.(VariableAdvanceProvider)

	var firstErr error
	advanceX := 0.0
	for _, r := range text {
		if err := ctx.Err(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
		if r < 0x20 && r != '\t' {
			continue
		}

		gid := GlyphID(parsed.GlyphIndex(r))
		if gid == 0 {
			advanceX += varOrRawAdvance(varProv, parsed, uint16(gid), ppem, variations)
			continue
		}

		glyphX := x + advanceX
		glyphY := y

		intX := math.Floor(glyphX)
		intY := math.Floor(glyphY)
		subpixelX := glyphX - intX
		subpixelY := glyphY - intY

		// Unified gvar + hinting path (skrifa load_simple parity).
		// ExtractOutlineHintedVar applies gvar deltas THEN hinting in one pass.
		outline, xErr := extractOutlineForDraw(sf.source, parsed, gid, ppem, hinting, variations)
		if xErr != nil {
			if firstErr == nil {
				firstErr = &GlyphDrawError{GID: gid, Rune: r, Size: ppem, Err: xErr}
			}
			advanceX += varOrRawAdvance(varProv, parsed, uint16(gid), ppem, variations)
			continue
		}
		if outline == nil || outline.IsEmpty() {
			advanceX += varOrRawAdvance(varProv, parsed, uint16(gid), ppem, variations)
			continue
		}

		var result *GlyphMaskResult
		var rErr error
		switch mode {
		case rasterModeAliased:
			result, rErr = rast.RasterizeOutlineAliased(outline, subpixelX, subpixelY)
		default:
			result, rErr = rast.RasterizeOutline(outline, subpixelX, subpixelY)
		}
		if rErr != nil {
			if firstErr == nil {
				firstErr = &GlyphDrawError{GID: gid, Rune: r, Size: ppem, Err: rErr}
			}
			advanceX += varOrRawAdvance(varProv, parsed, uint16(gid), ppem, variations)
			continue
		}
		if result == nil {
			advanceX += varOrRawAdvance(varProv, parsed, uint16(gid), ppem, variations)
			continue
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

		advanceX += varOrRawAdvance(varProv, parsed, uint16(gid), ppem, variations)
	}
	return firstErr
}

// firstGlyphError builds the error reported when a face cannot render at all
// (for example because its FontSource was closed). It names the first
// drawable rune and the requested size so callers can pinpoint the failure.
func firstGlyphError(text string, size float64, err error) error {
	r := rune(-1)
	for _, candidate := range text {
		if candidate < 0x20 && candidate != '\t' {
			continue
		}
		r = candidate
		break
	}
	return &GlyphDrawError{GID: 0, Rune: r, Size: size, Err: err}
}

// varOrRawAdvance returns the HVAR advance when available, raw hmtx otherwise.
func varOrRawAdvance(varProv VariableAdvanceProvider, parsed ParsedFont, gid uint16, ppem float64, variations []FontVariation) float64 {
	if varProv != nil {
		return varProv.GlyphAdvanceVar(gid, ppem, variations)
	}
	return parsed.GlyphAdvance(gid, ppem)
}

// rasterizeCachedOutline extracts the outline through the cache and rasterizes
// it, bypassing GlyphMaskRasterizer's internal (uncached) extraction.
//
// The extraction/rasterization split is passed in so aliased draws keep using
// the NoAAFiller: the glyphRasterizeFunc callbacks differ only in how the
// outline is turned into coverage.
func rasterizeCachedOutline(
	rasterize glyphRasterizeFunc,
	rast *GlyphMaskRasterizer,
	source *FontSource,
	parsed ParsedFont,
	gid GlyphID,
	ppem, subpixelX, subpixelY float64,
	hinting Hinting,
) (*GlyphMaskResult, error) {
	if rast == nil {
		rast = NewGlyphMaskRasterizer()
	}
	// Extract once through the shared outline cache (which embeds FontID,
	// source generation and variation) and rasterize the cached outline. This
	// keeps AA and aliased draws on the same cached geometry while preserving
	// their distinct coverage fillers.
	outline, err := extractOutlineForDraw(source, parsed, gid, ppem, hinting, nil)
	if err != nil {
		return nil, err
	}
	if outline == nil || outline.IsEmpty() {
		return nil, nil //nolint:nilnil // empty glyph
	}
	if isAliasedRasterizer(rasterize) {
		return rast.RasterizeOutlineAliased(outline, subpixelX, subpixelY)
	}
	return rast.RasterizeOutline(outline, subpixelX, subpixelY)
}

// isAliasedRasterizer distinguishes the aliased callback. The two callbacks
// are package-level functions; comparing them is stable and allocation-free.
func isAliasedRasterizer(rasterize glyphRasterizeFunc) bool {
	return reflect.ValueOf(rasterize).Pointer() == reflect.ValueOf(rasterizeAliasedGlyph).Pointer()
}

// rasterizeHintedGlyph rasterizes a glyph with 256-level analytic AA coverage.
func rasterizeHintedGlyph(
	rast *GlyphMaskRasterizer,
	pf ParsedFont,
	gid GlyphID,
	ppem float64,
	subpixelX, subpixelY float64,
	hinting Hinting,
) (*GlyphMaskResult, error) {
	return rast.RasterizeHinted(pf, gid, ppem, subpixelX, subpixelY, hinting)
}

// rasterizeAliasedGlyph rasterizes a glyph with binary (0 or 255) coverage.
func rasterizeAliasedGlyph(
	rast *GlyphMaskRasterizer,
	pf ParsedFont,
	gid GlyphID,
	ppem float64,
	subpixelX, subpixelY float64,
	hinting Hinting,
) (*GlyphMaskResult, error) {
	return rast.RasterizeAliased(pf, gid, ppem, subpixelX, subpixelY, hinting)
}

// drawSourceFace renders text using per-glyph rasterization with fractional
// advances. Each glyph is individually rasterized via GlyphMaskRasterizer
// (256-level analytic AA with hinting), then composited at its precise
// subpixel position.
//
// This replaces the previous font.Drawer approach which used integer-rounded
// advances internally, causing letters to merge at small sizes (e.g., "Te"
// at 12px). The Glyphs() iterator now returns fractional X positions from
// HintingNone advances (ADR-039), while outline rasterization still uses
// the face's configured hinting for crisp stems.
func drawSourceFace(dst draw.Image, text string, sf *sourceFace, x, y float64, col color.Color) {
	_ = drawGlyphs(context.Background(), dst, sf, text, x, y, col, rasterizeHintedGlyph)
}

// drawSourceFaceContext is drawSourceFace with cancellation and error
// reporting. It is used by DrawContext/DrawErr.
func drawSourceFaceContext(ctx context.Context, dst draw.Image, text string, sf *sourceFace, x, y float64, col color.Color) error {
	return drawGlyphs(ctx, dst, sf, text, x, y, col, rasterizeHintedGlyph)
}

// drawMultiFace renders text using a MultiFace, selecting the appropriate font for each rune.
func drawMultiFace(dst draw.Image, text string, mf *MultiFace, x, y float64, col color.Color) {
	currentX := x

	// Tabs already expanded to spaces by Draw() via expandTabs().
	for _, r := range text {
		runeStr := string(r)

		// Find the face that has this glyph
		var faceToUse Face
		for _, f := range mf.faces {
			if f.HasGlyph(r) {
				faceToUse = f
				break
			}
		}

		// Fallback to first face if no face has the glyph
		if faceToUse == nil {
			faceToUse = mf.faces[0]
		}

		// Get advance for this rune (prefer hinted advance if available).
		advance := faceGlyphAdvance(faceToUse, runeStr)

		// Render based on face type
		switch f := faceToUse.(type) {
		case *sourceFace:
			drawSourceFace(dst, runeStr, f, currentX, y, col)
		case *FilteredFace:
			drawFilteredFace(dst, runeStr, f, currentX, y, col)
		case *MultiFace:
			// Nested MultiFace (rare but possible)
			drawMultiFace(dst, runeStr, f, currentX, y, col)
		}

		currentX += advance
	}
}

// drawFilteredFace renders text using a FilteredFace.
func drawFilteredFace(dst draw.Image, text string, ff *FilteredFace, x, y float64, col color.Color) {
	// FilteredFace wraps another face - extract and use it
	// Only render runes that pass the filter
	currentX := x

	// Tabs already expanded to spaces by Draw() via expandTabs().
	for _, r := range text {
		if !ff.inRanges(r) {
			continue // Skip filtered runes
		}

		runeStr := string(r)

		// Get advance for this rune (prefer hinted advance if available).
		advance := faceGlyphAdvance(ff.face, runeStr)

		// Render using the underlying face
		switch f := ff.face.(type) {
		case *sourceFace:
			drawSourceFace(dst, runeStr, f, currentX, y, col)
		case *FilteredFace:
			drawFilteredFace(dst, runeStr, f, currentX, y, col)
		case *MultiFace:
			drawMultiFace(dst, runeStr, f, currentX, y, col)
		}

		currentX += advance
	}
}

// faceGlyphAdvance returns the advance width for a single-rune string.
// Uses advanceResolver (HVAR for variable, TT-hinted for static, raw otherwise).
// This is used by drawMultiFace and drawFilteredFace which render one rune
// at a time and need consistent cursor advancement matching Face.Advance().
func faceGlyphAdvance(face Face, runeStr string) float64 {
	sf, ok := face.(*sourceFace)
	if !ok {
		return unhintedGlyphAdvance(face, runeStr)
	}

	ar := newAdvanceResolver(sf)
	for glyph := range sf.Glyphs(runeStr) {
		return ar.advance(uint16(glyph.GID))
	}
	return 0
}

// unhintedGlyphAdvance returns the advance from the Glyphs iterator (unhinted).
func unhintedGlyphAdvance(face Face, runeStr string) float64 {
	for glyph := range face.Glyphs(runeStr) {
		return glyph.Advance
	}
	return 0
}

// Measure returns the dimensions of text.
// Width is the horizontal advance, height is the font's line height.
func Measure(text string, face Face) (width, height float64) {
	if text == "" || face == nil {
		return 0, 0
	}

	// Get advance width
	width = face.Advance(text)

	// Get line height from metrics
	metrics := face.Metrics()
	height = metrics.LineHeight()

	return width, height
}

// DrawOptions provides advanced options for text drawing.
// Reserved for future enhancements.
type DrawOptions struct {
	// Color for the text (default: black)
	Color color.Color
}
