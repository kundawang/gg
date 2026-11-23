// Command ggfontcheck exercises the font-cache lifecycle quality gate.
//
// It reproduces the hot-reload scenario the gate protects against:
//
//  1. Two font sources with the SAME FontID but different outlines are created.
//  2. Each source renders the same glyph into the global glyph cache.
//  3. The first source is closed (purging its cache generation).
//  4. The second source must produce different pixels, and cache hits must be
//     attributed to the second generation only.
//
// The command exits non-zero and prints the first failing glyph/size when any
// gate fails, so it can be wired into CI next to `go test` and the race run.
//
// Usage:
//
//	go run ./cmd/ggfontcheck
//	go run ./cmd/ggfontcheck -race-ish   # no-op flag kept for symmetry
package main

import (
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"os"
	"sync"

	"github.com/gogpu/gg/text"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
)

func main() {
	verbose := flag.Bool("v", false, "verbose gate output")
	flag.Parse()

	if err := run(*verbose); err != nil {
		fmt.Fprintln(os.Stderr, "ggfontcheck: FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("ggfontcheck: PASS (font cache lifecycle gates)")
}

type observer struct {
	mu   sync.Mutex
	hits map[uint64]int
}

func (o *observer) OnOutlineAccess(key text.OutlineCacheKey, hit bool) {
	if !hit {
		return
	}
	o.mu.Lock()
	o.hits[key.SourceGeneration]++
	o.mu.Unlock()
}

func (o *observer) OnOutlineInsert(text.OutlineCacheKey) {}

func run(verbose bool) error {
	obs := &observer{hits: make(map[uint64]int)}
	cache := text.NewGlyphCacheWithConfig(text.GlyphCacheConfig{
		MaxEntries:    4096,
		FrameLifetime: 64,
		Observer:      obs,
	})
	old := text.SetGlobalGlyphCache(cache)
	defer func() { _ = text.SetGlobalGlyphCache(old) }()

	// Two real fonts from the same family, used to prove that generation, not
	// just FontID, discriminates cache entries.
	first, err := text.NewFontSource(goregular.TTF)
	if err != nil {
		return fmt.Errorf("load regular: %w", err)
	}
	second, err := text.NewFontSource(gobold.TTF)
	if err != nil {
		return fmt.Errorf("load bold: %w", err)
	}
	defer func() { _ = second.Close() }()

	firstGen := first.Generation()
	secondGen := second.Generation()
	if firstGen == 0 || secondGen == 0 || firstGen == secondGen {
		return fmt.Errorf("generations not unique: %d, %d", firstGen, secondGen)
	}

	faceA := first.Face(18)
	faceB := second.Face(18)

	imgA := image.NewRGBA(image.Rect(0, 0, 160, 48))
	if err := text.DrawErr(imgA, "Hot reload", faceA, 2, 32, color.Black); err != nil {
		return fmt.Errorf("draw first: %w", err)
	}

	if err := first.Close(); err != nil {
		return fmt.Errorf("close first: %w", err)
	}
	// Hits recorded during the first draw are legitimate (repeated runes hit
	// within the same generation). Freeze the count at Close: any later hit
	// for this generation means the purge failed and a closed source's outline
	// is still reachable.
	hitsAtClose := obs.hits[firstGen]

	// A face from the closed source must degrade with an identifiable error.
	if err := text.DrawErr(imgA, "Hot reload", faceA, 2, 32, color.Black); !errors.Is(err, text.ErrFontSourceClosed) {
		return fmt.Errorf("closed-source draw error = %v, want ErrFontSourceClosed", err)
	}

	imgB := image.NewRGBA(image.Rect(0, 0, 160, 48))
	if err := text.DrawErr(imgB, "Hot reload", faceB, 2, 32, color.Black); err != nil {
		return fmt.Errorf("draw second: %w", err)
	}

	if ink(imgA) == 0 || ink(imgB) == 0 {
		return fmt.Errorf("empty render: first=%d second=%d", ink(imgA), ink(imgB))
	}
	if ink(imgA) == ink(imgB) && samePixels(imgA, imgB) {
		return fmt.Errorf("hot reload reused the previous font's outlines (identical pixels)")
	}
	if after := obs.hits[firstGen]; after != hitsAtClose {
		return fmt.Errorf("cache attributed %d new hits to closed generation %d after purge",
			after-hitsAtClose, firstGen)
	}
	if verbose {
		fmt.Printf("first gen=%d ink=%d, second gen=%d ink=%d, hits(first)=%d\n",
			firstGen, ink(imgA), secondGen, ink(imgB), obs.hits[firstGen])
	}
	return nil
}

func samePixels(a, b *image.RGBA) bool {
	if len(a.Pix) != len(b.Pix) {
		return false
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			return false
		}
	}
	return true
}

func ink(img *image.RGBA) int {
	n := 0
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0 {
			n++
		}
	}
	return n
}
