package text

import (
	"fmt"
	"hash/fnv"
)

// ComputeFontID generates a stable hash identifier for a font source.
// Uses the full font name (includes subfamily like "Regular"/"Bold") and
// number of glyphs as a lightweight fingerprint. The full name prevents
// glyph-cache collisions between faces in the same family (e.g., "Go Regular"
// vs "Go Bold") that share the same family name and glyph count.
//
// FontID is deliberately identity-stable across reloads: the same font file
// loaded twice returns the same FontID. It therefore must not be used alone as
// a cache discriminator for hot-reloaded fonts — pair it with
// [FontSource.Generation] (already embedded in
// [OutlineCacheKey.SourceGeneration]).
//
// ComputeFontID returns 0 for a nil source or a source whose data has been
// released by Close.
func ComputeFontID(source *FontSource) uint64 {
	if source == nil {
		return 0
	}
	parsed := source.Parsed()
	if parsed == nil {
		return 0
	}
	h := fnv.New64a()
	fullName := parsed.FullName()
	if fullName == "" {
		fullName = source.Name()
	}
	_, _ = fmt.Fprintf(h, "%s:%d", fullName, parsed.NumGlyphs())
	return h.Sum64()
}

// cacheFontID returns the FontID to use in cache keys for source, and its
// generation. Unlike ComputeFontID it continues to work after Close by using
// the name captured at construction, so a closed source's stale outlines can
// still be identified and purged.
func cacheFontID(source *FontSource) (fontID, generation uint64) {
	if source == nil {
		return 0, 0
	}
	generation = source.Generation()
	if parsed := source.Parsed(); parsed != nil {
		return ComputeFontID(source), generation
	}
	// Closed source: fall back to the stable name captured at construction.
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s", source.Name())
	return h.Sum64(), generation
}

// outlineKeyFor builds the canonical outline cache key for a glyph produced by
// the given source. It is the single place where FontID + generation +
// variation + size + hinting are combined, so call sites cannot accidentally
// omit the generation.
func outlineKeyFor(source *FontSource, gid GlyphID, size float64, hinting Hinting, variations []FontVariation) OutlineCacheKey {
	fontID, generation := cacheFontID(source)
	return OutlineCacheKey{
		FontID:           fontID,
		SourceGeneration: generation,
		GID:              gid,
		Size:             sizeToInt16(size),
		Hinting:          hinting,
		VariationHash:    VariationHash(variations),
	}
}

// outlineKeyForFont builds an outline cache key from a bare ParsedFont when no
// FontSource is available (legacy GPU/scene call sites). SourceGeneration is 0
// because the identity of the owning source is unknown; callers that have a
// FontSource must use [outlineKeyFor] instead.
func outlineKeyForFont(font ParsedFont, gid GlyphID, size float64, hinting Hinting, variations []FontVariation) OutlineCacheKey {
	return OutlineCacheKey{
		FontID:        computeFontID(font),
		GID:           gid,
		Size:          sizeToInt16(size),
		Hinting:       hinting,
		VariationHash: VariationHash(variations),
	}
}

// SizeToCacheKey converts a float64 ppem size to the int16 representation
// used by outline cache keys. Sizes are clamped to [0, 32767].
//
// Call sites outside the text package use this so cache keys can never
// disagree with the package's own key construction.
func SizeToCacheKey(size float64) int16 {
	return sizeToInt16(size)
}
