package text

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// This file provides injectable FontSource/ParsedFont doubles and deterministic
// synthetic fonts for the font-cache lifecycle quality gate.
//
// The synthetic fonts intentionally share FullName and glyph count so that
// ComputeFontID returns the SAME FontID for both. Their 'A' glyph has a
// different outline, which means any outline leak across a hot reload is
// visible either in the recorded cache-hit counters or in rendered pixels.

// parseCounter counts how many times the injected parser ran.
var parseCounter atomic.Int64

// injectionRecord is the process-wide log shared by the doubles so tests can
// assert on creation, Close, Face and outline-cache events.
type injectionRecord struct {
	mu       sync.Mutex
	created  int
	closed   int
	parseErr error
	events   []string
}

func (r *injectionRecord) add(format string, args ...any) {
	r.mu.Lock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *injectionRecord) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

func (r *injectionRecord) reset() {
	r.mu.Lock()
	r.created, r.closed, r.parseErr, r.events = 0, 0, nil, nil
	r.mu.Unlock()
}

// doubleParsedFont is a ParsedFont whose outlines are defined by its fields.
// It implements enough of ParsedFont for Face/Draw to run through the
// real outline extractor when it is the *ownParsedFont produced by the
// synthetic font; this double is used when the test wants to simulate a
// parser that hands back a non-own implementation.
type doubleParsedFont struct {
	fontID       uint64
	generation   uint64
	fullName     string
	familyName   string
	numGlyphs    int
	upem         int
	outline      *GlyphOutline
	closed       *atomic.Bool
	record       *injectionRecord
	fail         *atomic.Bool
	variations   []FontVariation
	cache        *GlyphCache
	extractor    *OutlineExtractor
	extractCalls *atomic.Int64
	// failOutlines makes outline extraction fail for this font.
	failOutlines bool
}

func (f *doubleParsedFont) Name() string     { return f.familyName }
func (f *doubleParsedFont) FullName() string { return f.fullName }
func (f *doubleParsedFont) NumGlyphs() int   { return f.numGlyphs }
func (f *doubleParsedFont) UnitsPerEm() int  { return f.upem }

func (f *doubleParsedFont) GlyphIndex(r rune) uint16 {
	if r == 'A' {
		return 1
	}
	return 0
}

func (f *doubleParsedFont) GlyphAdvance(_ uint16, _ float64) float64 { return 10 }

func (f *doubleParsedFont) GlyphBounds(gid uint16, ppem float64) Rect {
	if o := f.outlineFor(gid, ppem); o != nil {
		return o.Bounds
	}
	return Rect{}
}

func (f *doubleParsedFont) Metrics(_ float64) FontMetrics {
	return FontMetrics{Ascent: 8, Descent: -2, LineGap: 0}
}

// outlineFor produces a scaled outline for bounds/rotation queries. It never
// consults the cache: only the drawing path does, so cache statistics reflect
// real render requests.
func (f *doubleParsedFont) outlineFor(gid uint16, ppem float64) *GlyphOutline {
	if f.closed != nil && f.closed.Load() {
		return nil
	}
	if f.failOutlines {
		return nil
	}
	if gid != 1 || f.outline == nil {
		return nil
	}
	return f.outline.Scale(float32(ppem / 10))
}

// drawOutline implements the outlineSource seam used by the real draw path.
// It runs through the configured GlyphCache so cache-hit counters reflect
// actual render requests; the key embeds the double's FontID and the owning
// FontSource generation, exactly like a production source.
func (f *doubleParsedFont) drawOutline(gid GlyphID, ppem float64, _ Hinting, v []FontVariation) (*GlyphOutline, error) {
	if f.extractCalls != nil {
		f.extractCalls.Add(1)
	}
	if f.closed != nil && f.closed.Load() {
		return nil, ErrFontSourceClosed
	}
	if f.failOutlines || (f.fail != nil && f.fail.Load()) {
		return nil, ErrUnsupportedFontType
	}
	if gid != 1 || f.outline == nil {
		return nil, nil
	}

	fresh := func() *GlyphOutline { return f.outline.Scale(float32(ppem / 10)) }

	cache := f.cache
	if cache == nil {
		cache = GetGlobalGlyphCache()
	}
	key := OutlineCacheKey{
		FontID:           f.fontID,
		SourceGeneration: f.generation,
		GID:              gid,
		Size:             sizeToInt16(ppem),
		Hinting:          HintingNone,
		VariationHash:    VariationHash(v),
	}
	return cache.GetOrCreate(key, fresh), nil
}

// doubleParser is an injectable FontParser that returns a fresh
// doubleParsedFont and records each Parse call.
type doubleParser struct {
	record *injectionRecord
	font   *doubleParsedFont
}

func (p *doubleParser) Parse(_ []byte) (ParsedFont, error) {
	parseCounter.Add(1)
	p.record.mu.Lock()
	p.record.created++
	if p.record.parseErr != nil {
		err := p.record.parseErr
		p.record.mu.Unlock()
		return nil, err
	}
	p.record.mu.Unlock()
	return p.font.clone(), nil
}

// clone returns a copy so each FontSource owns an independent double (the
// generation is stamped by the source after parsing).
func (f *doubleParsedFont) clone() *doubleParsedFont {
	c := *f
	return &c
}

// SetSourceGeneration implements the parser-double seam used by FontSource.
func (f *doubleParsedFont) SetSourceGeneration(gen uint64) {
	f.generation = gen
	if f.outline != nil {
		f.outline.GID = 1
	}
}

// Synthetic font variants used across the lifecycle tests. Both share the
// family/full name and glyph count (hence the same FontID) but render
// visibly different squares at the same ppem.
const (
	thinSquareSide = 300 // font units (upem is 1000)
	wideSquareSide = 600
	squareOffsetX  = 100
)

func syntheticThinFont() []byte { return synthesizeSquareTTF(thinSquareSide, squareOffsetX) }
func syntheticWideFont() []byte { return synthesizeSquareTTF(wideSquareSide, squareOffsetX) }

// --- synthetic sfnt fonts with identical FontID but distinct outlines ---

// synthesizeSquareTTF builds a minimal, well-formed TrueType font containing
// glyph 0 (.notdef, empty) and glyph 1 ('A', a square). All fonts produced by
// this helper share family "Synthetic", style "Regular" and 2 glyphs, so
// ComputeFontID is identical across variants. Only the square's geometry (and
// thereby its outline) differs between variants.
//
// scale scales the 100x100 square; offsetX shifts it horizontally. Two fonts
// with different (scale, offsetX) have identical names/IDs but visibly
// different pixels.
func synthesizeSquareTTF(scale int16, offsetX int16) []byte {
	tables := map[string][]byte{}

	// head: unitsPerEm at 18, indexToLocFormat (short) at 50.
	head := make([]byte, 54)
	binary.BigEndian.PutUint16(head[18:20], 1000) // unitsPerEm
	binary.BigEndian.PutUint16(head[50:52], 0)    // short loca
	tables["head"] = head

	// maxp: numGlyphs at 4.
	maxp := make([]byte, 6)
	binary.BigEndian.PutUint32(maxp[0:4], 0x00010000)
	binary.BigEndian.PutUint16(maxp[4:6], 2)
	tables["maxp"] = maxp

	// hhea: ascent/descent/lineGap and numberOfHMetrics.
	hhea := make([]byte, 36)
	binary.BigEndian.PutUint16(hhea[4:6], 800)
	binary.BigEndian.PutUint16(hhea[6:8], 0xFF38) // -200 as int16
	binary.BigEndian.PutUint16(hhea[34:36], 2)
	tables["hhea"] = hhea

	// hmtx: two long metrics.
	hmtx := make([]byte, 8)
	binary.BigEndian.PutUint16(hmtx[0:2], 500) // .notdef advance
	binary.BigEndian.PutUint16(hmtx[4:6], 600) // 'A' advance
	binary.BigEndian.PutUint16(hmtx[6:8], 50)  // 'A' lsb
	tables["hmtx"] = hmtx

	// name: family (1) and full name (4), both Windows Unicode.
	family := "Synthetic"
	fullName := "Synthetic Regular"
	var nameRecs []byte
	var nameStrings []byte
	appendName := func(nameID uint16, s string) {
		encoded := encodeUTF16BE(s)
		rec := make([]byte, 12)
		binary.BigEndian.PutUint16(rec[0:2], 3) // platform Windows
		binary.BigEndian.PutUint16(rec[2:4], 1) // Unicode BMP
		binary.BigEndian.PutUint16(rec[4:6], 0x0409)
		binary.BigEndian.PutUint16(rec[6:8], nameID)
		binary.BigEndian.PutUint16(rec[8:10], uint16(len(encoded))) //nolint:gosec // tiny fixture
		binary.BigEndian.PutUint16(rec[10:12], uint16(len(nameStrings)))
		nameRecs = append(nameRecs, rec...)
		nameStrings = append(nameStrings, encoded...)
	}
	appendName(nameIDFamily, family)
	appendName(nameIDFullName, fullName)
	name := make([]byte, 6)
	binary.BigEndian.PutUint16(name[2:4], 2)                       // count
	binary.BigEndian.PutUint16(name[4:6], uint16(6+len(nameRecs))) // storage offset
	name = append(name, nameRecs...)
	name = append(name, nameStrings...)
	tables["name"] = name

	// cmap format 4 mapping 'A' (0x41) -> glyph 1.
	tables["cmap"] = syntheticCmapFormat4('A', 1)

	// glyf + loca: empty .notdef then scaled/offset square.
	square := buildScaledSquare(scale, offsetX)
	glyf := append([]byte{}, square...)
	if len(glyf)%2 != 0 {
		glyf = append(glyf, 0)
	}
	loca := make([]byte, 0, 6)
	loca = binary.BigEndian.AppendUint16(loca, 0)                   // gid 0 start
	loca = binary.BigEndian.AppendUint16(loca, 0)                   // gid 0 end / gid 1 start
	loca = binary.BigEndian.AppendUint16(loca, uint16(len(glyf)/2)) // gid 1 end
	tables["glyf"] = glyf
	tables["loca"] = loca

	tags := []string{"cmap", "glyf", "head", "hhea", "hmtx", "loca", "maxp", "name"}
	return assembleSFNT(0x00010000, tables, tags)
}

// buildScaledSquare returns one simple glyph record: a square with the given
// side length (relative to the 100-unit fixture) and horizontal offset.
func buildScaledSquare(side, offsetX int16) []byte {
	var b []byte
	b = binary.BigEndian.AppendUint16(b, 1) // numberOfContours
	b = binary.BigEndian.AppendUint16(b, uint16(offsetX))
	b = binary.BigEndian.AppendUint16(b, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(offsetX+side))
	b = binary.BigEndian.AppendUint16(b, uint16(side))
	b = binary.BigEndian.AppendUint16(b, 3) // endPtsOfContours[0] = 3 (4 points)
	b = binary.BigEndian.AppendUint16(b, 0) // instructionLength
	// Flags: 4 on-curve points with word deltas (0x01 == on-curve, X_SHORT off,
	// Y_SHORT off, so the delta stream contains int16 pairs).
	b = append(b, 1, 1, 1, 1)
	// Points: (0,0) -> (side,0) -> (side,side) -> (0,side) relative to origin.
	dx := []int16{0, side, 0, -side}
	dy := []int16{0, 0, side, 0}
	for i := range 4 {
		b = binary.BigEndian.AppendUint16(b, uint16(dx[i]))
	}
	for i := range 4 {
		b = binary.BigEndian.AppendUint16(b, uint16(dy[i]))
	}
	return b
}

// syntheticCmapFormat4 builds a cmap table with one format-4 subtable mapping
// a single BMP rune to one glyph ID.
func syntheticCmapFormat4(r rune, gid uint16) []byte {
	segCount := 1
	sub := make([]byte, 14+8*segCount+2)
	binary.BigEndian.PutUint16(sub[0:2], 4)
	binary.BigEndian.PutUint16(sub[2:4], uint16(len(sub))) //nolint:gosec // tiny fixture
	binary.BigEndian.PutUint16(sub[6:8], uint16(segCount*2))
	// searchRange/entrySelector/rangeShift left zero: the parser recomputes.
	endCodeOff := 14
	startCodeOff := endCodeOff + segCount*2 + 2
	idDeltaOff := startCodeOff + segCount*2
	idRangeOff := idDeltaOff + segCount*2

	binary.BigEndian.PutUint16(sub[endCodeOff:], uint16(r))
	binary.BigEndian.PutUint16(sub[startCodeOff:], uint16(r))
	// glyph ID = code + idDelta (mod 65536), so idDelta = gid - r.
	binary.BigEndian.PutUint16(sub[idDeltaOff:], uint16(int16(gid)-int16(r))) //nolint:gosec // fixture
	binary.BigEndian.PutUint16(sub[idRangeOff:], 0)

	// Wrap in a cmap header with one (3,1) encoding record.
	cmap := make([]byte, 4+8)
	binary.BigEndian.PutUint16(cmap[2:4], 1) // numTables
	binary.BigEndian.PutUint16(cmap[4:6], 3) // platform
	binary.BigEndian.PutUint16(cmap[6:8], 1) // encoding
	binary.BigEndian.PutUint32(cmap[8:12], 12)
	return append(cmap, sub...)
}

func encodeUTF16BE(s string) []byte {
	out := make([]byte, 0, len(s)*2)
	for _, r := range s {
		out = binary.BigEndian.AppendUint16(out, uint16(r)) //nolint:gosec // ASCII fixture
	}
	return out
}

// assembleSFNT builds a font file from a table map. tags must be sorted.
func assembleSFNT(sfntVersion uint32, tables map[string][]byte, tags []string) []byte {
	header := 12 + 16*len(tags)
	total := header
	for _, tag := range tags {
		total = (total + 3) &^ 3
		total += len(tables[tag])
	}
	out := make([]byte, total)
	binary.BigEndian.PutUint32(out[0:4], sfntVersion)
	binary.BigEndian.PutUint16(out[4:6], uint16(len(tags))) //nolint:gosec // tiny fixture
	off := header
	for i, tag := range tags {
		off = (off + 3) &^ 3
		rec := 12 + i*16
		copy(out[rec:rec+4], tag)
		binary.BigEndian.PutUint32(out[rec+8:rec+12], uint32(off))               //nolint:gosec // tiny fixture
		binary.BigEndian.PutUint32(out[rec+12:rec+16], uint32(len(tables[tag]))) //nolint:gosec // tiny fixture
		copy(out[off:], tables[tag])
		off += len(tables[tag])
	}
	return out
}

// --- recording cache observer ---

type recordingObserver struct {
	mu      sync.Mutex
	hits    map[OutlineCacheKey]int
	misses  map[OutlineCacheKey]int
	inserts map[OutlineCacheKey]int
	// firstInsertOrder preserves generation order for assertions.
	order []OutlineCacheKey
}

func newRecordingObserver() *recordingObserver {
	return &recordingObserver{
		hits:    make(map[OutlineCacheKey]int),
		misses:  make(map[OutlineCacheKey]int),
		inserts: make(map[OutlineCacheKey]int),
	}
}

func (o *recordingObserver) OnOutlineAccess(key OutlineCacheKey, hit bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if hit {
		o.hits[key]++
	} else {
		o.misses[key]++
	}
}

func (o *recordingObserver) OnOutlineInsert(key OutlineCacheKey) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.inserts[key]++
	o.order = append(o.order, key)
}

func (o *recordingObserver) hitCountFor(generation uint64) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	total := 0
	for k, n := range o.hits {
		if k.SourceGeneration == generation {
			total += n
		}
	}
	return total
}

func (o *recordingObserver) insertCountFor(generation uint64) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	total := 0
	for k, n := range o.inserts {
		if k.SourceGeneration == generation {
			total += n
		}
	}
	return total
}

func (o *recordingObserver) hitCountForFont(fontID uint64) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	total := 0
	for k, n := range o.hits {
		if k.FontID == fontID {
			total += n
		}
	}
	return total
}

// requireObserver verifies the observer is installed on the cache.
func requireObserver(t *testing.T, cache *GlyphCache) {
	t.Helper()
	if cache.observer == nil {
		t.Fatal("cache has no observer installed")
	}
}

// newObservedCache installs a fresh observed cache as the global cache,
// restoring the previous one on cleanup. Tests that share the global cache
// must not run in parallel.
func newObservedCache(t *testing.T) (*GlyphCache, *recordingObserver) {
	t.Helper()
	obs := newRecordingObserver()
	cache := NewGlyphCacheWithConfig(GlyphCacheConfig{
		MaxEntries:    4096,
		FrameLifetime: 64,
		Observer:      obs,
	})
	old := SetGlobalGlyphCache(cache)
	t.Cleanup(func() { _ = SetGlobalGlyphCache(old) })
	return cache, obs
}

// mustSource builds a FontSource from synthesized font data.
func mustSource(t *testing.T, data []byte) *FontSource {
	t.Helper()
	src, err := NewFontSource(data)
	if err != nil {
		t.Fatalf("NewFontSource: %v", err)
	}
	return src
}

// errSentinel is used by tests that need a stable error identity.
var errSentinel = errors.New("sentinel")

// TestSyntheticFontsShareFontIDAndDifferInPixels is a guard on the fixtures
// themselves: if the two synthetic variants stop sharing FontID, every
// hot-reload test below would pass vacuously.
func TestSyntheticFontsShareFontIDAndDifferInPixels(t *testing.T) {
	thin := mustSource(t, syntheticThinFont())
	defer func() { _ = thin.Close() }()
	wide := mustSource(t, syntheticWideFont())
	defer func() { _ = wide.Close() }()

	if thin.Parsed() == nil || wide.Parsed() == nil {
		t.Fatal("synthetic font failed to parse")
	}
	if thin.Name() != wide.Name() {
		t.Fatalf("fixtures must share family name, got %q vs %q", thin.Name(), wide.Name())
	}
	if thin.Parsed().FullName() != wide.Parsed().FullName() {
		t.Fatalf("fixtures must share full name, got %q vs %q", thin.Parsed().FullName(), wide.Parsed().FullName())
	}
	if ComputeFontID(thin) != ComputeFontID(wide) {
		t.Skip("fixtures produced distinct FontIDs; hot-reload tests need same-ID fonts")
	}
	if thin.Generation() == wide.Generation() {
		t.Fatal("independent sources must have distinct generations")
	}

	ex := NewOutlineExtractor()
	a, err := ex.ExtractOutlineHinted(thin.Parsed(), 1, 20, HintingNone)
	if err != nil || a == nil || a.IsEmpty() {
		t.Fatalf("thin outline extraction failed: %v", err)
	}
	b, err := ex.ExtractOutlineHinted(wide.Parsed(), 1, 20, HintingNone)
	if err != nil || b == nil || b.IsEmpty() {
		t.Fatalf("wide outline extraction failed: %v", err)
	}
	if a.Bounds == b.Bounds {
		t.Fatalf("fixture outlines must differ, both bounds = %+v", a.Bounds)
	}
}
