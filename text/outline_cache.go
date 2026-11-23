package text

// Outline cache integration shared by the drawing front ends.
//
// Every outline derived from a FontSource must be cached under a key that
// includes the source's generation. Keeping the lookup in one place makes that
// invariant structural: callers cannot forget the generation, and a font
// hot-reload (new source, same path, same FontID) cannot resolve outlines that
// were built from the closed predecessor.

// CachedSourceOutline returns the glyph outline for gid at size ppem, using
// cache when non-nil and extracting through extractor on a miss. The cache key
// embeds source's FontID, generation, hinting mode and variation coordinates.
//
// It returns [ErrFontSourceClosed] when source has been closed; nothing is
// cached in that case, so a closed source cannot repopulate the cache it was
// purged from. When the glyph has no outline (space, .notdef) the result is
// (nil, nil) and no entry is stored.
//
// CachedSourceOutline is safe for concurrent use as long as cache and
// extractor are (GlyphCache is; OutlineExtractor is stateless).
func CachedSourceOutline(
	cache *GlyphCache,
	extractor *OutlineExtractor,
	source *FontSource,
	gid GlyphID,
	size float64,
	hinting Hinting,
	variations []FontVariation,
) (*GlyphOutline, error) {
	parsed, err := source.ParsedErr()
	if err != nil {
		return nil, err
	}
	if extractor == nil {
		extractor = NewOutlineExtractor()
	}
	if cache == nil {
		return extractor.ExtractOutlineHintedVar(parsed, gid, size, hinting, variations)
	}

	key := OutlineKeyFor(source, gid, size, hinting, variations)
	return cache.GetOrCreate(key, func() *GlyphOutline {
		o, xerr := extractor.ExtractOutlineHintedVar(parsed, gid, size, hinting, variations)
		if xerr != nil || o == nil || o.IsEmpty() {
			return nil
		}
		return o
	}), nil
}

// OutlineKeyFor returns the canonical outline cache key for a glyph produced
// by source. It is exported for front ends that manage their own cache lookup
// (GPU pipelines, retained scenes) but must keep keys consistent with
// [CachedSourceOutline].
func OutlineKeyFor(source *FontSource, gid GlyphID, size float64, hinting Hinting, variations []FontVariation) OutlineCacheKey {
	return outlineKeyFor(source, gid, size, hinting, variations)
}

// extractOutlineForDraw returns the outline used by the text draw loop.
//
// Production sources go through CachedSourceOutline. Parser doubles may
// implement [outlineSource] to supply geometry directly while still exercising
// the real GlyphCache (their drawOutline implementation decides), which lets
// tests assert hit/miss accounting for the exact bytes that were rendered.
func extractOutlineForDraw(source *FontSource, parsed ParsedFont, gid GlyphID, ppem float64, hinting Hinting, variations []FontVariation) (*GlyphOutline, error) {
	if ds, ok := parsed.(outlineSource); ok {
		return ds.drawOutline(gid, ppem, hinting, variations)
	}
	return CachedSourceOutline(GetGlobalGlyphCache(), nil, source, gid, ppem, hinting, variations)
}

// outlineSource is implemented by parser doubles to provide drawable geometry.
type outlineSource interface {
	drawOutline(gid GlyphID, ppem float64, hinting Hinting, variations []FontVariation) (*GlyphOutline, error)
}
