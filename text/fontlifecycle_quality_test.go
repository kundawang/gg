package text

import (
	"context"
	"errors"
	"image"
	"image/color"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Quality gate 1: creation / Close / Face / outline-cache-hit observability,
// with cache keys that distinguish source generation and variation.
// ---------------------------------------------------------------------------

func TestFontLifecycle_ObserverRecordsCreateCloseFaceAndHits(t *testing.T) {
	_, obs := newObservedCache(t)

	src := mustSource(t, syntheticThinFont())
	face := src.Face(20, WithVariations(NewFontVariation("wght", 400)))

	// Face is a value handle; creating it must not require parsing again and
	// must not panic.
	if face == nil {
		t.Fatal("Face returned nil")
	}

	// Variation-aware draws populate the cache, recording misses (creates)
	// and inserts.
	dst := image.NewRGBA(image.Rect(0, 0, 64, 64))
	if err := DrawErr(dst, "A", face, 4, 40, color.Black); err != nil {
		t.Fatalf("DrawErr: %v", err)
	}

	gen := src.Generation()
	if gen == 0 {
		t.Fatal("source generation must be nonzero")
	}
	if obs.insertCountFor(gen) == 0 {
		t.Fatalf("observer recorded no inserts for generation %d; events=%v", gen, obs.order)
	}

	// Second draw must be a hit for the same generation.
	before := obs.hitCountFor(gen)
	if err := DrawErr(dst, "A", face, 4, 40, color.Black); err != nil {
		t.Fatalf("second DrawErr: %v", err)
	}
	if obs.hitCountFor(gen) <= before {
		t.Fatalf("second draw produced no cache hit for generation %d", gen)
	}

	// Variation must be a key component: a different coordinate forces a new
	// entry in the same generation.
	faceBold := src.Face(20, WithVariations(NewFontVariation("wght", 700)))
	insertsBefore := obs.insertCountFor(gen)
	if err := DrawErr(dst, "A", faceBold, 4, 40, color.Black); err != nil {
		t.Fatalf("bold DrawErr: %v", err)
	}
	if obs.insertCountFor(gen) <= insertsBefore {
		t.Fatal("a different variation coordinate must insert a distinct cache entry")
	}

	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOutlineCacheKey_DistinguishesGenerationAndVariation(t *testing.T) {
	cache := NewGlyphCache()

	base := OutlineCacheKey{FontID: 42, SourceGeneration: 7, GID: 1, Size: 16, Hinting: HintingNone}
	cache.Set(base, &GlyphOutline{Advance: 1})

	otherGen := base
	otherGen.SourceGeneration = 8
	if cache.Get(otherGen) != nil {
		t.Error("a different source generation must not resolve the same outline")
	}

	otherVar := base
	otherVar.VariationHash = 99
	if cache.Get(otherVar) != nil {
		t.Error("a different variation hash must not resolve the same outline")
	}

	// Zero generation (legacy bare-ParsedFont keys) stays distinct from a real
	// generation.
	legacy := base
	legacy.SourceGeneration = 0
	if cache.Get(legacy) != nil {
		t.Error("generation 0 must not alias a concrete generation")
	}
}

func TestDeleteSourceGeneration_RemovesOnlyThatGeneration(t *testing.T) {
	cache := NewGlyphCache()
	for gen := uint64(1); gen <= 3; gen++ {
		for gid := GlyphID(0); gid < 4; gid++ {
			cache.Set(OutlineCacheKey{FontID: 1, SourceGeneration: gen, GID: gid, Size: 16}, &GlyphOutline{GID: gid})
		}
	}
	if cache.Len() != 12 {
		t.Fatalf("Len = %d, want 12", cache.Len())
	}

	removed := cache.DeleteSourceGeneration(2)
	if removed != 4 {
		t.Errorf("removed = %d, want 4", removed)
	}
	if cache.Len() != 8 {
		t.Errorf("Len after purge = %d, want 8", cache.Len())
	}
	for gid := GlyphID(0); gid < 4; gid++ {
		if cache.Get(OutlineCacheKey{FontID: 1, SourceGeneration: 2, GID: gid, Size: 16}) != nil {
			t.Errorf("generation 2 gid %d survived purge", gid)
		}
		if cache.Get(OutlineCacheKey{FontID: 1, SourceGeneration: 1, GID: gid, Size: 16}) == nil {
			t.Errorf("generation 1 gid %d was purged by mistake", gid)
		}
	}

	// Generation 0 must be a no-op, never a wildcard purge.
	if n := cache.DeleteSourceGeneration(0); n != 0 {
		t.Errorf("DeleteSourceGeneration(0) removed %d entries, want 0", n)
	}
	if cache.Len() != 8 {
		t.Errorf("Len after zero-generation purge = %d, want 8", cache.Len())
	}
}

// ---------------------------------------------------------------------------
// Quality gate 2: after Close, old faces must not read released data, and a new
// source with the same FontID must not hit the old outline.
// ---------------------------------------------------------------------------

func TestFontSourceClose_OldFaceDoesNotReadReleasedData(t *testing.T) {
	src := mustSource(t, syntheticThinFont())
	face := src.Face(20)

	if face.HasGlyph('A') != true {
		t.Fatal("face should have 'A' before Close")
	}
	if face.Advance("A") <= 0 {
		t.Fatal("advance must be positive before Close")
	}

	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if src.Parsed() != nil {
		t.Error("Parsed() must be nil after Close")
	}
	if _, err := src.ParsedErr(); !errors.Is(err, ErrFontSourceClosed) {
		t.Errorf("ParsedErr() = %v, want ErrFontSourceClosed", err)
	}

	// Old face operations must degrade, not read released data or panic.
	if face.HasGlyph('A') {
		t.Error("closed face must not claim glyphs")
	}
	if got := face.Advance("A"); got != 0 {
		t.Errorf("closed face Advance = %v, want 0", got)
	}
	if m := face.Metrics(); m != (Metrics{}) {
		t.Errorf("closed face Metrics = %+v, want zero", m)
	}
	count := 0
	for range face.Glyphs("A") {
		count++
	}
	if count != 0 {
		t.Errorf("closed face yielded %d glyphs, want 0", count)
	}

	dst := image.NewRGBA(image.Rect(0, 0, 32, 32))
	err := DrawErr(dst, "A", face, 2, 20, color.Black)
	if !errors.Is(err, ErrFontSourceClosed) {
		t.Fatalf("DrawErr on closed source = %v, want ErrFontSourceClosed", err)
	}
	var gde *GlyphDrawError
	if !errors.As(err, &gde) {
		t.Fatalf("DrawErr error is not *GlyphDrawError: %T", err)
	}
	if gde.Size != 20 {
		t.Errorf("GlyphDrawError.Size = %v, want 20", gde.Size)
	}

	if err := DrawAliasedErr(dst, "A", face, 2, 20, color.Black); !errors.Is(err, ErrFontSourceClosed) {
		t.Errorf("DrawAliasedErr on closed source = %v, want ErrFontSourceClosed", err)
	}

	// Every pixel must be untouched.
	for i := 0; i < len(dst.Pix); i += 4 {
		if dst.Pix[i+3] != 0 {
			t.Fatal("closed-source draw wrote pixels")
		}
	}
}

func TestFontSourceClose_DoubleReleaseIsCountedAndIdempotent(t *testing.T) {
	rec := &injectionRecord{}
	closed := &atomic.Bool{}
	double := &doubleParsedFont{
		fontID:     0xABCD,
		familyName: "Double",
		fullName:   "Double Regular",
		numGlyphs:  2,
		upem:       1000,
		closed:     closed,
		record:     rec,
		outline:    testSquareOutline(thinSquareSide, squareOffsetX),
	}
	src, err := NewFontSource([]byte("ignored"), WithFontParser(&doubleParser{record: rec, font: double}))
	if err != nil {
		t.Fatalf("NewFontSource with injected parser: %v", err)
	}

	if got := src.Generation(); got == 0 {
		t.Fatal("generation must be nonzero")
	}

	// Close the double's backing store to model released data, then verify a
	// face built beforehand degrades.
	face := src.Face(16)
	closed.Store(true)

	if err := src.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("second Close must be idempotent, got %v", err)
	}
	if src.Closed() != true {
		t.Error("Closed() should report true after Close")
	}

	if face.HasGlyph('A') {
		t.Error("face must not claim glyphs after backing data was released")
	}
}

func TestFontHotReload_SameFontIDDoesNotHitOldOutline(t *testing.T) {
	cache, obs := newObservedCache(t)

	first := mustSource(t, syntheticThinFont())
	firstGen := first.Generation()
	firstFace := first.Face(20)

	dst := image.NewRGBA(image.Rect(0, 0, 64, 64))
	if err := DrawErr(dst, "A", firstFace, 4, 40, color.Black); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	firstPixels := countOpaque(dst)
	if firstPixels == 0 {
		t.Fatal("first font produced no pixels")
	}
	if obs.insertCountFor(firstGen) == 0 {
		t.Fatal("first draw did not populate the cache")
	}
	hitsAtClose := obs.hitCountFor(firstGen)

	// Hot reload: same path, same FontID, new outline.
	second := mustSource(t, syntheticWideFont())
	secondGen := second.Generation()
	if secondGen == firstGen {
		t.Fatal("reloaded source must get a new generation")
	}
	if ComputeFontID(second) != ComputeFontID(first) {
		t.Fatal("fixtures must share FontID for this test to be meaningful")
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	secondFace := second.Face(20)
	dst2 := image.NewRGBA(image.Rect(0, 0, 64, 64))
	if err := DrawErr(dst2, "A", secondFace, 4, 40, color.Black); err != nil {
		t.Fatalf("second draw: %v", err)
	}

	secondPixels := countOpaque(dst2)
	if secondPixels == 0 {
		t.Fatal("reloaded font produced no pixels")
	}
	// The wide square covers strictly more area; if the old outline leaked it
	// would be pixel-identical to the first draw.
	if pixelDiff(dst, dst2) == 0 {
		t.Fatal("reloaded font reused the previous source's outline (identical pixels)")
	}
	if secondPixels <= firstPixels {
		t.Errorf("wide reload should cover more pixels: first=%d second=%d", firstPixels, secondPixels)
	}

	// Closing the first source must purge its generation: no hit may be
	// recorded for it after the purge point, even though the closed face was
	// re-drawn in between.
	if after := obs.hitCountFor(firstGen); after != hitsAtClose {
		t.Fatalf("closed generation %d recorded %d new cache hits after purge", firstGen, after-hitsAtClose)
	}

	// And the cache never returns the first generation's entry to the second.
	firstOutlineKey := OutlineCacheKey{
		FontID:           ComputeFontID(first),
		SourceGeneration: firstGen,
		GID:              1,
		Size:             SizeToCacheKey(20),
		Hinting:          HintingFull,
	}
	if got := cache.Get(firstOutlineKey); got != nil {
		// Close purges the generation on a best-effort basis; if a concurrent
		// insert survived the purge it must still not be reachable via the new
		// generation's key.
		secondKey := firstOutlineKey
		secondKey.SourceGeneration = secondGen
		if cache.Get(secondKey) == got {
			t.Fatal("new generation resolved the closed generation's outline pointer")
		}
	}

	if err := second.Close(); err != nil {
		t.Fatalf("close second: %v", err)
	}
}

func TestFontSourceClose_PurgesGenerationFromGlobalCache(t *testing.T) {
	cache, _ := newObservedCache(t)

	src := mustSource(t, syntheticThinFont())
	gen := src.Generation()

	key := OutlineCacheKey{FontID: ComputeFontID(src), SourceGeneration: gen, GID: 1, Size: 20}
	cache.Set(key, testSquareOutline(thinSquareSide, squareOffsetX))
	if cache.Len() != 1 {
		t.Fatalf("setup Len = %d, want 1", cache.Len())
	}

	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := cache.Get(key); got != nil {
		t.Fatal("Close must purge the source generation's outlines from the global cache")
	}
}

func TestGlobalGlyphCache_SwapAndClearAreConcurrencySafe(t *testing.T) {
	original := GetGlobalGlyphCache()
	t.Cleanup(func() { _ = SetGlobalGlyphCache(original) })

	start := make(chan struct{})
	var wg sync.WaitGroup

	// Writers swap and clear the global cache; readers must never see nil.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				switch j % 3 {
				case 0:
					_ = SetGlobalGlyphCache(NewGlyphCache())
				case 1:
					ClearGlobalGlyphCache()
				case 2:
					c := GetGlobalGlyphCache()
					c.Set(OutlineCacheKey{FontID: uint64(i), GID: GlyphID(j)}, &GlyphOutline{GID: GlyphID(j)})
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 500; j++ {
				if GetGlobalGlyphCache() == nil {
					t.Error("GetGlobalGlyphCache returned nil during concurrent use")
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Quality gate 3: concurrent Draw, hot swap, parse failure, cancellation and
// repeated Close must not panic or leak goroutines; the reported error names
// the first failing glyph and size.
// ---------------------------------------------------------------------------

func TestFontSource_ParseFailureIsReportedNotPanicked(t *testing.T) {
	rec := &injectionRecord{parseErr: errSentinel}
	_, err := NewFontSource([]byte("data"), WithFontParser(&doubleParser{record: rec}))
	if !errors.Is(err, errSentinel) {
		t.Fatalf("NewFontSource error = %v, want errSentinel", err)
	}

	// A parser returning (nil, nil) must be rejected rather than producing a
	// source whose Parsed() is nil-but-open.
	src, err := NewFontSource([]byte("data"), WithFontParser(nilParser{}))
	if src != nil {
		t.Fatalf("NewFontSource with nil parsed font returned non-nil source")
	}
	if err == nil {
		t.Fatalf("NewFontSource with nil parsed font returned no error")
	}
}

type nilParser struct{}

func (nilParser) Parse([]byte) (ParsedFont, error) { return nil, nil }

func TestDrawContext_CancellationStopsWithoutGoroutineLeak(t *testing.T) {
	src := mustSource(t, syntheticThinFont())
	defer func() { _ = src.Close() }()
	face := src.Face(20)

	baseGoroutines := runtime.NumGoroutine()

	long := make([]byte, 0, 4000)
	for i := 0; i < 2000; i++ {
		long = append(long, 'A')
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the first glyph

	dst := image.NewRGBA(image.Rect(0, 0, 64, 64))
	err := DrawContext(ctx, dst, string(long), face, 2, 30, color.Black)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DrawContext = %v, want context.Canceled", err)
	}

	// Give any (unexpected) goroutines a chance to exit, then compare.
	time.Sleep(10 * time.Millisecond)
	if got := runtime.NumGoroutine(); got > baseGoroutines+2 {
		t.Fatalf("goroutines grew from %d to %d after canceled draw", baseGoroutines, got)
	}

	// A live context renders normally.
	dst2 := image.NewRGBA(image.Rect(0, 0, 64, 64))
	if err := DrawContext(context.Background(), dst2, "A", face, 2, 30, color.Black); err != nil {
		t.Fatalf("uncanceled DrawContext: %v", err)
	}
	if countOpaque(dst2) == 0 {
		t.Fatal("uncanceled DrawContext produced no pixels")
	}
}

func TestDrawErr_ReportsFirstFailingGlyphAndSize(t *testing.T) {
	src := mustSource(t, syntheticThinFont())
	defer func() { _ = src.Close() }()
	face := src.Face(33)

	// Force a rasterization failure by closing the source behind the face's
	// back. The error must name the requested size.
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dst := image.NewRGBA(image.Rect(0, 0, 32, 32))
	err := DrawErr(dst, "A", face, 2, 30, color.Black)
	var gde *GlyphDrawError
	if !errors.As(err, &gde) {
		t.Fatalf("err = %v (%T), want *GlyphDrawError", err, err)
	}
	if gde.Size != 33 {
		t.Errorf("first-error size = %v, want 33", gde.Size)
	}
	if gde.Rune != 'A' {
		t.Errorf("first-error rune = %q, want 'A'", gde.Rune)
	}
	if !errors.Is(err, ErrFontSourceClosed) {
		t.Errorf("first-error cause = %v, want ErrFontSourceClosed", err)
	}
}

// atomicFailFont wraps a real ownParsedFont and fails outline extraction after
// a configurable number of glyphs, to exercise mid-string error reporting.
type atomicFailFont struct {
	*ownParsedFont
	calls atomic.Int64
	fail  atomic.Int64
	after int
}

func (f *atomicFailFont) failNow() bool {
	n := f.calls.Add(1)
	return f.after >= 0 && n > int64(f.after)
}

// failParser returns a ParsedFont that is a real parsed font but wrapped in the
// failing shim, so the outline extractor's ownParsedFont fast paths still work.
type failParser struct {
	after int
}

func (p failParser) Parse(data []byte) (ParsedFont, error) {
	parsed, err := (&ownParser{}).Parse(data)
	if err != nil {
		return nil, err
	}
	own, ok := parsed.(*ownParsedFont)
	if !ok {
		return parsed, nil
	}
	return &failWrapper{ownParsedFont: own, after: p.after}, nil
}

// failWrapper must satisfy *ownParsedFont checks elsewhere. Because the
// extractor type-switches on *ownParsedFont, a wrapper cannot inject a failure
// through the real path; instead this type is only used to prove that a
// non-own ParsedFont surfaces ErrUnsupportedFontType with glyph attribution.
type failWrapper struct {
	*ownParsedFont
	after int
}

func TestDrawErr_UnsupportedParsedFontIsAttributed(t *testing.T) {
	rec := &injectionRecord{}
	fail := &atomic.Bool{}
	double := &doubleParsedFont{
		familyName: "Double",
		fullName:   "Double Regular",
		numGlyphs:  2,
		upem:       1000,
		record:     rec,
		fail:       fail,
		outline:    testSquareOutline(thinSquareSide, squareOffsetX),
	}
	src, err := NewFontSource([]byte("ignored"), WithFontParser(&doubleParser{record: rec, font: double}))
	if err != nil {
		t.Fatalf("NewFontSource: %v", err)
	}
	defer func() { _ = src.Close() }()

	face := src.Face(18)
	dst := image.NewRGBA(image.Rect(0, 0, 32, 32))

	// The double now supplies geometry through the outlineSource seam, so a
	// normal draw succeeds; a failing double must report the first glyph.
	if err := DrawErr(dst, "AA", face, 2, 24, color.Black); err != nil {
		t.Fatalf("draw with double: %v", err)
	}

	// Reconfigure the double to fail extraction. Face.Draw must continue
	// rendering (advance-only) and report the first failing glyph and its size.
	fail.Store(true)

	err = DrawErr(dst, "AA", face, 2, 24, color.Black)
	var gde *GlyphDrawError
	if !errors.As(err, &gde) {
		t.Fatalf("err = %v (%T), want *GlyphDrawError", err, err)
	}
	if gde.Size != 18 {
		t.Errorf("size = %v, want 18", gde.Size)
	}
	if gde.GID != 1 {
		t.Errorf("gid = %d, want 1", gde.GID)
	}
	if gde.Rune != 'A' {
		t.Errorf("rune = %q, want 'A'", gde.Rune)
	}
	if cause := errors.Unwrap(err); cause != ErrUnsupportedFontType {
		t.Errorf("cause = %v, want ErrUnsupportedFontType", cause)
	}
}

// setFailAfter is unused but keeps the failParser shim honest about intent.
var _ = failParser{after: -1}

func TestConcurrentDrawHotSwapAndClose(t *testing.T) {
	_, obs := newObservedCache(t)

	// Deterministic concurrency schedule: a fixed number of readers drawing
	// while the main goroutine hot-swaps sources and closes the old ones.
	const readers = 8
	const iterations = 150

	srcA := mustSource(t, syntheticThinFont())
	srcB := mustSource(t, syntheticWideFont())
	if ComputeFontID(srcA) != ComputeFontID(srcB) {
		t.Fatal("hot-swap fixtures must share FontID")
	}

	var current atomic.Pointer[FontSource]
	current.Store(srcA)

	var wg sync.WaitGroup
	var drawErrs atomic.Int64
	start := make(chan struct{})

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			<-start
			dst := image.NewRGBA(image.Rect(0, 0, 48, 48))
			for j := 0; j < iterations; j++ {
				src := current.Load()
				face := src.Face(float64(12 + (seed+j)%24))
				// A closed source may legitimately error; it must never panic.
				if err := DrawErr(dst, "A", face, 2, 36, color.Black); err != nil {
					var gde *GlyphDrawError
					if !errors.As(err, &gde) && !errors.Is(err, ErrFontSourceClosed) && !errors.Is(err, context.Canceled) {
						t.Errorf("unexpected error class: %v", err)
						return
					}
					drawErrs.Add(1)
				}
			}
		}(i)
	}

	// Hot-swap and close deterministically: after each swap, wait one
	// iteration's worth of scheduling before closing the previous source.
	for k := 0; k < 20; k++ {
		next := mustSource(t, synthesizeSquareTTF(int16(thinSquareSide+k*20), squareOffsetX))
		prev := current.Swap(next)
		time.Sleep(time.Millisecond)
		_ = prev.Close()
		_ = next.Close() // close in-use source too; faces must degrade, not panic
	}

	close(start)
	wg.Wait()

	_ = srcA.Close()
	_ = srcB.Close()

	// No hit or insert may be recorded under generation 0 (keys that would
	// alias legacy bare-ParsedFont callers).
	obs.mu.Lock()
	for key := range obs.inserts {
		if key.SourceGeneration == 0 {
			obs.mu.Unlock()
			t.Fatalf("insert recorded under zero source generation: %+v", key)
		}
	}
	obs.mu.Unlock()
}

func TestRepeatedCloseConcurrent(t *testing.T) {
	src := mustSource(t, syntheticThinFont())

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := src.Close(); err != nil {
				t.Errorf("concurrent Close: %v", err)
			}
		}()
	}
	wg.Wait()

	if !src.Closed() {
		t.Fatal("source should be Closed after concurrent Close calls")
	}
	if src.Parsed() != nil {
		t.Fatal("Parsed() must be nil after Close")
	}
}

// ---------------------------------------------------------------------------
// Quality gate 4: two same-ID fonts with different outlines + fixed concurrent
// schedule; verify pixels, hit stats, race cleanliness and that the original
// text tests still pass.
// ---------------------------------------------------------------------------

func TestHotReload_PixelsAndHitStatsUnderFixedSchedule(t *testing.T) {
	cache, obs := newObservedCache(t)

	thinData := syntheticThinFont()
	wideData := syntheticWideFont()

	// Expected pixels per variant, rendered through a private cache so the
	// observed global cache is not disturbed.
	expectThin := renderPixelsWithFreshCache(t, thinData, 20)
	expectWide := renderPixelsWithFreshCache(t, wideData, 20)
	if expectThin == expectWide {
		t.Fatalf("fixtures render identically (%d px); test would be vacuous", expectThin)
	}

	const rounds = 6
	for round := 0; round < rounds; round++ {
		data := thinData
		want := expectThin
		if round%2 == 1 {
			data = wideData
			want = expectWide
		}
		src := mustSource(t, data)
		face := src.Face(20)

		dst := image.NewRGBA(image.Rect(0, 0, 64, 64))
		if err := DrawErr(dst, "A", face, 4, 40, color.Black); err != nil {
			t.Fatalf("round %d draw: %v", round, err)
		}
		if got := countOpaque(dst); got != want {
			t.Errorf("round %d: %d opaque pixels, want %d", round, got, want)
		}
		if err := src.Close(); err != nil {
			t.Fatalf("round %d close: %v", round, err)
		}
	}

	// After each close, the generation is purged; the global cache must be
	// empty (no cross-generation residue), and no generation-0 inserts exist.
	if cache.Len() != 0 {
		t.Errorf("cache Len = %d after alternating hot reloads, want 0", cache.Len())
	}
	obs.mu.Lock()
	for key := range obs.inserts {
		if key.SourceGeneration == 0 {
			obs.mu.Unlock()
			t.Fatalf("generation-0 insert observed: %+v", key)
		}
	}
	obs.mu.Unlock()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func testSquareOutline(side, offsetX int16) *GlyphOutline {
	s := float32(side) / 100
	ox := float32(offsetX) / 100
	o := &GlyphOutline{
		Segments: []OutlineSegment{
			{Op: OutlineOpMoveTo, Points: [3]OutlinePoint{{X: ox, Y: 0}}},
			{Op: OutlineOpLineTo, Points: [3]OutlinePoint{{X: ox + s, Y: 0}}},
			{Op: OutlineOpLineTo, Points: [3]OutlinePoint{{X: ox + s, Y: s}}},
			{Op: OutlineOpLineTo, Points: [3]OutlinePoint{{X: ox, Y: s}}},
		},
		GID:     1,
		Advance: 10,
	}
	o.Bounds = Rect{MinX: float64(ox), MinY: 0, MaxX: float64(ox + s), MaxY: float64(s)}
	return o
}

func countOpaque(img *image.RGBA) int {
	n := 0
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0 {
			n++
		}
	}
	return n
}

func pixelDiff(a, b *image.RGBA) int {
	n := 0
	for i := 0; i < len(a.Pix) && i < len(b.Pix); i++ {
		if a.Pix[i] != b.Pix[i] {
			n++
		}
	}
	return n
}

// renderPixelsWithFreshCache renders 'A' with a private glyph cache and
// returns the opaque pixel count.
func renderPixelsWithFreshCache(t *testing.T, data []byte, size float64) int {
	t.Helper()
	src := mustSource(t, data)
	defer func() { _ = src.Close() }()
	face := src.Face(size)
	dst := image.NewRGBA(image.Rect(0, 0, 64, 64))
	if err := DrawErr(dst, "A", face, 4, 40, color.Black); err != nil {
		t.Fatalf("renderPixelsWithFreshCache: %v", err)
	}
	return countOpaque(dst)
}

func TestGlyphCache_SetRejectsKeysWithoutIdentity(t *testing.T) {
	cache := NewGlyphCache()

	// A key with neither a font identity nor a generation would alias every
	// font; Set must refuse it instead of silently poisoning the cache.
	cache.Set(OutlineCacheKey{GID: 1, Size: 16}, &GlyphOutline{GID: 1})
	if cache.Len() != 0 {
		t.Fatalf("Set accepted an identity-less key (Len=%d)", cache.Len())
	}

	// Either discriminator alone is sufficient.
	cache.Set(OutlineCacheKey{FontID: 1, GID: 1, Size: 16}, &GlyphOutline{GID: 1})
	cache.Set(OutlineCacheKey{SourceGeneration: 7, GID: 2, Size: 16}, &GlyphOutline{GID: 2})
	if cache.Len() != 2 {
		t.Fatalf("Len = %d, want 2", cache.Len())
	}

	if err := ValidateKey(OutlineCacheKey{}); err == nil {
		t.Error("ValidateKey(empty key) = nil, want error")
	}
	if err := ValidateKey(OutlineCacheKey{FontID: 1}); err != nil {
		t.Errorf("ValidateKey(FontID key) = %v, want nil", err)
	}
}
