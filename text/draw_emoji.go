package text

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
	"sync"

	xdraw "golang.org/x/image/draw"

	"github.com/gogpu/gg/text/emoji"
)

// maxEmojiClusterRunes bounds the number of codepoints in a single emoji
// cluster that is eligible for whole-cluster glyph resolution. Longer
// sequences (pathological ZWJ chains) are rendered per member instead.
// The longest real-world emoji sequences use ~15 codepoints.
const maxEmojiClusterRunes = 32

// maxClusterGlyphCacheEntries bounds the package-level cluster resolution
// cache. When full, the cache is cleared (same strategy as BitmapGlyphCache).
const maxClusterGlyphCacheEntries = 512

// SequenceGlyphProvider is an optional interface that ParsedFont
// implementations can satisfy to map a complete emoji sequence
// (ZWJ chain, skin-tone-modified base, flag pair, keycap, tag sequence)
// directly to a single glyph.
//
// This is the emoji equivalent of a GSUB ligature lookup, exposed for
// fonts that resolve sequences natively (e.g. a dedicated emoji table).
// When a font does not implement this interface, DrawWithEmoji falls back
// to shaping the cluster with the global Shaper and checking whether it
// collapses to a single glyph.
type SequenceGlyphProvider interface {
	// SequenceGlyph returns the glyph ID representing the whole emoji
	// sequence, or ok=false if the font has no single glyph for it.
	// codepoints contains the full cluster (including ZWJ, modifiers,
	// variation selectors, regional indicators and combining marks).
	SequenceGlyph(codepoints []rune) (glyphID uint16, ok bool)
}

// DrawWithEmoji renders text with full color emoji support.
//
// Text is segmented into emoji clusters and plain-text runs using the
// emoji package's parsing rules (ZWJ sequences, skin tone modifiers,
// regional-indicator flags, keycaps, tag sequences and variation selectors
// are kept together as one cluster). Each cluster is rendered as a unit:
//
//  1. Whole-cluster glyph: the font is asked for a single glyph for the
//     entire sequence (SequenceGlyphProvider, then Shaper-based ligature
//     resolution). A matching color bitmap is composited once; cluster
//     members are never drawn individually on this path.
//  2. Whole-cluster outline: if the cluster resolves to a single glyph
//     without color data (or its bitmap is missing), the glyph outline is
//     rendered once in the text color.
//  3. Per-member fallback: if the font has no whole-cluster resource, each
//     visible member is rendered in cluster order. Invisible format
//     characters (ZWJ, variation selectors, tag characters) are skipped
//     and contribute no advance.
//
// Plain-text runs are rendered with Draw, preserving normal shaping.
// Position (x, y) is the baseline origin.
// For fonts without color tables, this behaves identically to Draw().
func DrawWithEmoji(dst draw.Image, text string, face Face, x, y float64, col color.Color) {
	if text == "" || face == nil || dst == nil {
		return
	}

	parsed, colorFont, ok := colorFontFor(face)
	if !ok {
		// No color support (or no single font source, e.g. MultiFace):
		// fall back to standard rendering. Draw handles composite faces.
		Draw(dst, text, face, x, y, col)
		return
	}

	// Color-aware, cluster-based rendering.
	drawWithColorSupport(dst, text, face, parsed, colorFont, x, y, col)
}

// colorFontFor returns the parsed font and its ColorFont capabilities.
// ok is false if the font has no color tables or no usable source.
func colorFontFor(face Face) (ParsedFont, ColorFont, bool) {
	src := face.Source()
	if src == nil {
		return nil, nil, false
	}
	parsed := src.Parsed()
	if parsed == nil {
		return nil, nil, false
	}
	colorFont, hasColor := parsed.(ColorFont)
	if !hasColor || !colorFont.HasColorTables() {
		return parsed, nil, false
	}
	return parsed, colorFont, true
}

// drawWithColorSupport renders text by emoji cluster.
func drawWithColorSupport(dst draw.Image, s string, face Face, parsed ParsedFont, colorFont ColorFont, x, y float64, col color.Color) {
	// Sanitize invalid UTF-8 up front: segmentation works on byte offsets
	// derived from rune lengths, which diverge from the original bytes for
	// malformed input. ToValidUTF8 keeps offsets consistent (and never panics).
	s = strings.ToValidUTF8(s, "�")

	currentX := x
	for _, run := range emoji.Segment(s) {
		if !run.IsEmoji {
			// Plain text run: render with the standard path so kerning
			// and shaping of adjacent text are preserved.
			Draw(dst, run.Text, face, currentX, y, col)
			currentX += face.Advance(run.Text)
			continue
		}

		// Emoji run: split into individual clusters (adjacent emoji are
		// merged into one run by Segment, so re-parse here).
		sequences := emoji.ParseString(run.Text)
		if len(sequences) == 0 {
			// Defensive: an emoji run should always re-parse, but never
			// drop content if the rules change.
			Draw(dst, run.Text, face, currentX, y, col)
			currentX += face.Advance(run.Text)
			continue
		}
		for _, seq := range sequences {
			currentX += drawEmojiCluster(dst, face, parsed, colorFont, seq, currentX, y, col)
		}
	}
}

// drawEmojiCluster renders one emoji cluster and returns its advance.
func drawEmojiCluster(dst draw.Image, face Face, parsed ParsedFont, colorFont ColorFont, seq emoji.Sequence, x, y float64, col color.Color) float64 {
	codepoints := seq.Codepoints
	if len(codepoints) == 0 {
		return 0
	}

	// Whole-cluster rendering, unless the sequence is pathologically long.
	if len(codepoints) <= maxEmojiClusterRunes {
		if gid, ok := resolveClusterGlyph(face, parsed, seq); ok {
			drawResolvedGlyph(dst, face, parsed, colorFont, gid, x, y, col)
			return parsed.GlyphAdvance(gid, face.Size())
		}
	}

	// Per-member fallback: the font has no single resource for this cluster.
	return drawClusterMembers(dst, face, parsed, colorFont, codepoints, x, y, col)
}

// drawClusterMembers renders each visible member of a cluster individually.
// Invisible format characters (ZWJ, variation selectors, tag characters)
// are skipped entirely: they must not produce tofu or advance.
func drawClusterMembers(dst draw.Image, face Face, parsed ParsedFont, colorFont ColorFont, codepoints []rune, x, y float64, col color.Color) float64 {
	size := face.Size()
	currentX := x

	for _, r := range codepoints {
		if isInvisibleEmojiComponent(r) {
			continue
		}
		gid := parsed.GlyphIndex(r)
		if gid == 0 {
			// No glyph at all: match Draw's behavior for missing glyphs
			// (nothing painted, .notdef advance consumed).
			currentX += parsed.GlyphAdvance(0, size)
			continue
		}
		drawResolvedGlyph(dst, face, parsed, colorFont, gid, currentX, y, col)
		currentX += parsed.GlyphAdvance(gid, size)
	}

	return currentX - x
}

// isInvisibleEmojiComponent reports whether r is a zero-width format
// character that only has meaning inside an emoji sequence.
func isInvisibleEmojiComponent(r rune) bool {
	return emoji.IsZWJ(r) || emoji.IsVariationSelector(r) ||
		emoji.IsTagCharacter(r) || emoji.IsCancelTag(r)
}

// resolveClusterGlyph finds a single glyph representing the whole cluster.
// Resolution order:
//
//  1. Single codepoint: direct cmap lookup.
//  2. SequenceGlyphProvider on the parsed font (font-native lookup).
//  3. Shape the cluster with the global Shaper; a single resulting glyph
//     means the font's GSUB collapsed the sequence (e.g. ligature).
//
// Results (including misses) are cached per font source and full sequence
// string, so distinct sequences never share a cache entry.
func resolveClusterGlyph(face Face, parsed ParsedFont, seq emoji.Sequence) (uint16, bool) {
	codepoints := seq.Codepoints
	if len(codepoints) == 1 {
		gid := parsed.GlyphIndex(codepoints[0])
		return gid, gid != 0
	}

	key := clusterCacheKey{source: face.Source(), seq: seq.String()}
	if gid, ok, found := lookupClusterGlyph(key); found {
		return gid, ok
	}

	gid, ok := resolveClusterGlyphUncached(face, parsed, codepoints, key.seq)
	storeClusterGlyph(key, gid, ok)
	return gid, ok
}

// resolveClusterGlyphUncached performs the actual (uncached) lookup.
func resolveClusterGlyphUncached(face Face, parsed ParsedFont, codepoints []rune, seqText string) (uint16, bool) {
	// Font-native sequence lookup.
	if provider, ok := parsed.(SequenceGlyphProvider); ok {
		if gid, found := provider.SequenceGlyph(codepoints); found && gid != 0 {
			return gid, true
		}
	}

	// Shaper-based resolution: GSUB ligature substitution collapses
	// supported sequences (ZWJ families, flags, keycaps) to one glyph.
	shaped := Shape(seqText, face)
	if len(shaped) == 1 && shaped[0].GID != 0 {
		return uint16(shaped[0].GID), true
	}

	return 0, false
}

// drawResolvedGlyph renders a single glyph (by ID) using the appropriate
// renderer for its type. Bitmap glyphs with missing or undecodable data
// fall back to the glyph's outline.
func drawResolvedGlyph(dst draw.Image, face Face, parsed ParsedFont, colorFont ColorFont, gid uint16, x, y float64, col color.Color) {
	switch colorFont.GlyphType(gid) {
	case GlyphTypeBitmap:
		if drawBitmapGlyph(dst, colorFont, gid, ppemFor(face), x, y) {
			return
		}
		// Missing/corrupt bitmap: fall back to the outline.
		drawOutlineGlyphByID(dst, face, parsed, gid, x, y, col)

	case GlyphTypeCOLR:
		// TODO: Implement COLR rendering in Phase 2.
		// For now, fall back to outline.
		drawOutlineGlyphByID(dst, face, parsed, gid, x, y, col)

	default:
		// GlyphTypeOutline, GlyphTypeSVG (not yet supported), and unknown types.
		drawOutlineGlyphByID(dst, face, parsed, gid, x, y, col)
	}
}

// ppemFor returns the face size clamped to a valid uint16 ppem.
func ppemFor(face Face) uint16 {
	size := face.Size()
	if size < 1 {
		return 1
	}
	if size > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(size)
}

// drawBitmapGlyph renders a bitmap glyph (CBDT or sbix).
// Returns false if the bitmap is missing or cannot be decoded,
// allowing the caller to fall back to outline rendering.
func drawBitmapGlyph(dst draw.Image, colorFont ColorFont, glyphID uint16, ppem uint16, x, y float64) bool {
	bitmap, err := colorFont.BitmapGlyph(glyphID, ppem)
	if err != nil || bitmap == nil {
		return false
	}

	// Decode PNG data.
	img, err := bitmap.Decode()
	if err != nil || img == nil {
		return false
	}

	// Calculate scaling factor.
	scale := float64(ppem) / float64(bitmap.PPEM)
	if bitmap.PPEM == 0 || scale == 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
		scale = 1.0
	}

	// Calculate scaled dimensions.
	scaledW := int(float64(bitmap.Width) * scale)
	scaledH := int(float64(bitmap.Height) * scale)

	if scaledW <= 0 || scaledH <= 0 {
		return false
	}

	// Calculate destination bounds.
	// OriginX is horizontal bearing (offset from glyph origin).
	// OriginY is vertical bearing (distance from baseline to top of glyph).
	destX := int(x + float64(bitmap.OriginX)*scale)
	destY := int(y - float64(bitmap.OriginY)*scale)

	destRect := image.Rect(
		destX,
		destY,
		destX+scaledW,
		destY+scaledH,
	)

	// Draw with high-quality scaling.
	xdraw.CatmullRom.Scale(dst, destRect, img, img.Bounds(), xdraw.Over, nil)
	return true
}

// drawOutlineGlyphByID renders a single outline glyph identified by glyph ID
// at the given baseline origin, using the same rasterization path as Draw.
func drawOutlineGlyphByID(dst draw.Image, face Face, parsed ParsedFont, gid uint16, x, y float64, col color.Color) {
	sf, ok := asSourceFace(face)
	if !ok {
		// Composite faces have no single glyph ID space; use the source
		// the glyph ID was resolved against with default face options.
		src := face.Source()
		if src == nil {
			return
		}
		sf = &sourceFace{source: src, size: face.Size(), config: defaultFaceConfig()}
	}

	ppem := sf.size
	hinting := sf.config.hinting

	intX := math.Floor(x)
	intY := math.Floor(y)
	subpixelX := x - intX
	subpixelY := y - intY

	rast := NewGlyphMaskRasterizer()

	var result *GlyphMaskResult
	if vars := sf.Variations(); len(vars) > 0 {
		// Variable font: extract the gvar-adjusted outline, then rasterize.
		extractor := &OutlineExtractor{}
		outline, err := extractor.ExtractOutlineHintedVar(parsed, GlyphID(gid), ppem, hinting, vars)
		if err != nil || outline == nil || outline.IsEmpty() {
			return
		}
		result, err = rast.RasterizeOutline(outline, subpixelX, subpixelY)
		if err != nil || result == nil {
			return
		}
	} else {
		var err error
		result, err = rast.RasterizeHinted(parsed, GlyphID(gid), ppem, subpixelX, subpixelY, hinting)
		if err != nil || result == nil {
			return
		}
	}

	maskImg := &image.Alpha{
		Pix:    result.Mask,
		Stride: result.Width,
		Rect:   image.Rect(0, 0, result.Width, result.Height),
	}

	dstX := int(intX) + int(math.Round(float64(result.BearingX)))
	dstY := int(intY) - int(math.Round(float64(result.BearingY)))

	destRect := image.Rect(dstX, dstY, dstX+result.Width, dstY+result.Height)
	draw.DrawMask(dst, destRect, image.NewUniform(col), image.Point{}, maskImg, image.Point{}, draw.Over)
}

// asSourceFace unwraps face to its underlying *sourceFace, if any.
func asSourceFace(face Face) (*sourceFace, bool) {
	switch f := face.(type) {
	case *sourceFace:
		return f, true
	case *FilteredFace:
		return asSourceFace(f.face)
	default:
		return nil, false
	}
}

// clusterCacheKey uniquely identifies a resolved emoji cluster.
// The full sequence string is part of the key so distinct sequences
// (e.g. different skin tones or flag pairs) never share an entry.
type clusterCacheKey struct {
	source *FontSource
	seq    string
}

// clusterGlyphCache caches whole-cluster glyph resolutions (hits and
// misses) to avoid re-shaping the same sequence on every frame.
var clusterGlyphCache = struct {
	sync.RWMutex
	entries map[clusterCacheKey]clusterCacheEntry
}{entries: make(map[clusterCacheKey]clusterCacheEntry)}

// clusterCacheEntry holds a cached resolution result.
type clusterCacheEntry struct {
	gid uint16
	ok  bool
}

// lookupClusterGlyph returns the cached resolution for key.
func lookupClusterGlyph(key clusterCacheKey) (gid uint16, ok, found bool) {
	clusterGlyphCache.RLock()
	entry, found := clusterGlyphCache.entries[key]
	clusterGlyphCache.RUnlock()
	return entry.gid, entry.ok, found
}

// storeClusterGlyph caches a resolution result, clearing the cache when full.
func storeClusterGlyph(key clusterCacheKey, gid uint16, ok bool) {
	clusterGlyphCache.Lock()
	if len(clusterGlyphCache.entries) >= maxClusterGlyphCacheEntries {
		clusterGlyphCache.entries = make(map[clusterCacheKey]clusterCacheEntry)
	}
	clusterGlyphCache.entries[key] = clusterCacheEntry{gid: gid, ok: ok}
	clusterGlyphCache.Unlock()
}

// clearClusterGlyphCache drops all cached cluster resolutions.
// Called when the global shaper changes, since shaping results
// (and therefore cluster resolutions) may differ.
func clearClusterGlyphCache() {
	clusterGlyphCache.Lock()
	clusterGlyphCache.entries = make(map[clusterCacheKey]clusterCacheEntry)
	clusterGlyphCache.Unlock()
}

// BitmapGlyphCache caches decoded bitmap glyphs to avoid repeated PNG decoding.
// This is important for performance when rendering the same emoji multiple times.
type BitmapGlyphCache struct {
	entries map[bitmapCacheKey]*CachedBitmap
	maxSize int
}

// bitmapCacheKey uniquely identifies a cached bitmap.
type bitmapCacheKey struct {
	fontID  uintptr // Pointer to font for identity.
	glyphID uint16
	ppem    uint16
}

// CachedBitmap holds a decoded and optionally scaled bitmap.
type CachedBitmap struct {
	Img     image.Image
	OriginX float32
	OriginY float32
	Width   int
	Height  int
}

// NewBitmapGlyphCache creates a new bitmap glyph cache.
// maxSize is the maximum number of cached entries.
func NewBitmapGlyphCache(maxSize int) *BitmapGlyphCache {
	return &BitmapGlyphCache{
		entries: make(map[bitmapCacheKey]*CachedBitmap),
		maxSize: maxSize,
	}
}

// Get retrieves a cached bitmap, or nil if not cached.
func (c *BitmapGlyphCache) Get(fontID uintptr, glyphID, ppem uint16) *CachedBitmap {
	key := bitmapCacheKey{fontID: fontID, glyphID: glyphID, ppem: ppem}
	return c.entries[key]
}

// Put stores a bitmap in the cache.
func (c *BitmapGlyphCache) Put(fontID uintptr, glyphID, ppem uint16, bitmap *emoji.BitmapGlyph, img image.Image) {
	// Simple eviction: clear when full.
	if len(c.entries) >= c.maxSize {
		c.Clear()
	}

	key := bitmapCacheKey{fontID: fontID, glyphID: glyphID, ppem: ppem}
	c.entries[key] = &CachedBitmap{
		Img:     img,
		OriginX: bitmap.OriginX,
		OriginY: bitmap.OriginY,
		Width:   bitmap.Width,
		Height:  bitmap.Height,
	}
}

// Clear removes all entries from the cache.
func (c *BitmapGlyphCache) Clear() {
	c.entries = make(map[bitmapCacheKey]*CachedBitmap)
}

// Size returns the number of cached entries.
func (c *BitmapGlyphCache) Size() int {
	return len(c.entries)
}
