// Contextual GSUB engine — lookup types 5 (Context), 6 (ChainContext) and
// 8 (ReverseChainSingle), plus the shared machinery that drives the existing
// simple substitution types (1-4, 7) with lookup-flag awareness, nested
// lookup recursion and bounded execution.
//
// Semantics follow the OpenType Layout specification and match observed
// HarfBuzz 15 behavior:
//
//   - Lookup flags (IgnoreMarks/IgnoreLigatures/IgnoreBases and mark
//     filtering sets) govern the flag-aware cursor when scanning the buffer
//     and when matching input/backtrack/lookahead sequences.
//   - Nested lookups invoked through SubstLookupRecord run with their OWN
//     flags against a single anchor position. HarfBuzz's recursive path
//     (dispatch_recurse_func -> accelerator.apply) bypasses the top-level
//     check_glyph_property gate, so mark filtering sets are NOT re-evaluated
//     on the anchor for nested single/ligature lookups; coverage alone
//     decides. This engine reproduces that behavior.
//   - RightToLeft lookup flags do not reorder the buffer: HarfBuzz applies
//     GSUB on the logical-order glyph slice and only reorders for rendering
//     afterwards. Backtrack sequences therefore always walk toward lower
//     indices and lookahead sequences toward higher indices; the flag mainly
//     governs the direction of reverse-type scans.
//   - ReverseChainSingle applies in place, scanning from the end.
//
// All malformed binary structures (bad offsets, truncated arrays, unknown
// formats, empty coverage) are treated as non-matching, never as a reason to
// panic. Recursion depth and an operation budget bound the work per shaping
// call; exceeding either stops processing safely.
package text

import "encoding/binary"

// LookupFlag bit masks, per the OpenType Layout common table spec.
const (
	lookupFlagIgnoreBaseGlyphs    uint16 = 0x0002
	lookupFlagIgnoreLigatures     uint16 = 0x0004
	lookupFlagIgnoreMarks         uint16 = 0x0008
	lookupFlagUseMarkFilteringSet uint16 = 0x0010
)

// Execution limits. These mirror the intent of HarfBuzz's HB_MAX_NESTING_LEVEL
// and buffer max_ops: they are generous for real-world text but make cycles
// and hostile tables terminate deterministically.
const (
	gsubMaxNesting    = 64     // maximum nested SubstLookupRecord depth
	gsubMaxOps        = 200000 // substitution/match operations per Shape call
	gsubMaxContextLen = 64     // maximum rule input/backtrack/lookahead length
)

// gsubEngine carries the mutable state of one GSUB application. It is never
// shared across goroutines; each Shape call creates a fresh engine.
type gsubEngine struct {
	table *gsubTable
	gdef  *gdefTable

	glyphs  []shapingGlyph
	ops     int
	lastErr error

	// flagCarrier is the lookup currently executing; used by subtable-level
	// helpers (e.g. ligature component walking) to honor IgnoreMarks flags.
	flagCarrier *otLookup
}

// gsubError marks failures that should stop GSUB processing while still
// returning the glyphs produced so far.
type gsubError struct{ msg string }

func (e *gsubError) Error() string { return "text/gsub: " + e.msg }

var (
	errGSUBMaxNesting = &gsubError{"nested lookup depth exceeded (possible lookup cycle)"}
	errGSUBMaxOps     = &gsubError{"operation budget exceeded while applying GSUB"}
)

// charge accounts one operation. It returns false once the budget is spent.
func (e *gsubEngine) charge() bool {
	e.ops++
	if e.ops > gsubMaxOps {
		if e.lastErr == nil {
			e.lastErr = errGSUBMaxOps
		}
		return false
	}
	return true
}

// fail records the first error encountered.
func (e *gsubEngine) fail(err error) {
	if e.lastErr == nil {
		e.lastErr = err
	}
}

// --- Flag-aware cursor ---

// glyphSkippable reports whether the lookup's Ignore* flags require
// skipping gid while scanning the buffer or walking rule sequences. Mark
// filtering sets are handled separately by shouldSkip, which also has access
// to the lookup's MarkFilteringSet index.
func glyphSkippable(flags uint16, gdef *gdefTable, gid uint16) bool {
	switch gdef.glyphClass(gid) {
	case 1: // base
		return flags&lookupFlagIgnoreBaseGlyphs != 0
	case 2: // ligature
		return flags&lookupFlagIgnoreLigatures != 0
	case 3: // mark
		return flags&lookupFlagIgnoreMarks != 0
	default:
		return false
	}
}

// shouldSkip is the full per-lookup skip predicate used by both the
// top-level anchor gate and the flag-aware sequence cursors.
func shouldSkip(lu *otLookup, gdef *gdefTable, gid uint16) bool {
	if glyphSkippable(lu.lookupFlag, gdef, gid) {
		return true
	}
	if gdef.isMark(gid) && lu.lookupFlag&lookupFlagUseMarkFilteringSet != 0 {
		return !gdef.markSetCovers(int(lu.markFilteringSet), gid)
	}
	return false
}

// eligibleAtGate reports whether the glyph at an anchor position may enter
// lookup application at all.
func eligibleAtGate(lu *otLookup, gdef *gdefTable, gid uint16) bool {
	return !shouldSkip(lu, gdef, gid)
}

// nextGlyph returns the index of the next non-skipped glyph strictly after
// from, or -1 if the buffer ends first.
func (e *gsubEngine) nextGlyph(lu *otLookup, from int) int {
	for i := from + 1; i < len(e.glyphs); i++ {
		if shouldSkip(lu, e.gdef, e.glyphs[i].gid) {
			continue
		}
		return i
	}
	return -1
}

// prevGlyph returns the index of the nearest non-skipped glyph strictly
// before from, or -1 if the buffer starts first.
func (e *gsubEngine) prevGlyph(lu *otLookup, from int) int {
	for i := from - 1; i >= 0; i-- {
		if shouldSkip(lu, e.gdef, e.glyphs[i].gid) {
			continue
		}
		return i
	}
	return -1
}

// mergeClusters returns the smallest (earliest) cluster index in [lo, hi).
func mergeClusters(glyphs []shapingGlyph, lo, hi int) int {
	cluster := -1
	for i := lo; i < hi && i < len(glyphs); i++ {
		if cluster < 0 || glyphs[i].cluster < cluster {
			cluster = glyphs[i].cluster
		}
	}
	if cluster < 0 {
		return 0
	}
	return cluster
}

// --- Lookup entry points ---

// applyLookupIndex runs a lookup from the owning GSUB table by index,
// guarding against out-of-range references and recursion cycles.
func (e *gsubEngine) applyLookupIndex(index int) {
	e.applyLookupIndexDepth(index, 0)
}

// applyLookupIndexDepth is the recursive entry point carrying the current
// SubstLookupRecord nesting depth.
func (e *gsubEngine) applyLookupIndexDepth(index, depth int) bool {
	if e.lastErr != nil {
		return false
	}
	if index < 0 || index >= len(e.table.lookups) {
		// Bad lookup reference: stop this branch safely.
		return false
	}
	if depth >= gsubMaxNesting {
		e.fail(errGSUBMaxNesting)
		return false
	}
	return e.applyLookup(&e.table.lookups[index], depth)
}

// applyLookup runs one lookup over the whole engine buffer.
//
// The buffer always stays in logical character order. HarfBuzz likewise
// applies GSUB on the logical-order buffer (the visual reordering happens
// only after shaping), so the RightToLeft lookup flag does not reverse the
// buffer here: backtrack sequences walk toward lower indices and lookahead
// sequences toward higher indices regardless of the flag. Type 8 reverse
// lookups are the exception and handle their own backward scan.
func (e *gsubEngine) applyLookup(lu *otLookup, depth int) bool {
	if e.lastErr != nil || len(lu.subtables) == 0 {
		return false
	}

	// Extension lookups resolve to another lookup type per subtable.
	if lu.lookupType == 7 {
		return e.applyExtensionLookup(lu, depth)
	}

	e.flagCarrier = lu
	applied := false
	switch lu.lookupType {
	case 1, 2, 3, 4:
		applied = e.runSimpleLookup(lu)
	case 5:
		applied = e.runContextLookup(lu, depth)
	case 6:
		applied = e.runChainContextLookup(lu, depth)
	case 8:
		applied = e.runReverseChainLookup(lu)
	default:
		// Unknown lookup type: safe no-op (spec-reserved types 9+).
	}
	return applied
}

// applyExtensionLookup handles Type 7: each subtable points at the real
// subtable data and declares its lookup type.
func (e *gsubEngine) applyExtensionLookup(lu *otLookup, depth int) bool {
	applied := false
	for _, st := range lu.subtables {
		if len(st) < 8 {
			continue
		}
		if binary.BigEndian.Uint16(st[0:2]) != 1 {
			continue // unknown extension format
		}
		extType := binary.BigEndian.Uint16(st[2:4])
		extOffset := int(binary.BigEndian.Uint32(st[4:8]))
		if extOffset <= 0 || extOffset >= len(st) {
			continue
		}
		// Extension subtables never nest extension lookups; run as an
		// in-place pseudo-lookup carrying the parent flags.
		pseudo := &otLookup{
			lookupType:       extType,
			lookupFlag:       lu.lookupFlag,
			markFilteringSet: lu.markFilteringSet,
			subtables:        [][]byte{st[extOffset:]},
		}
		if e.applyLookup(pseudo, depth) {
			applied = true
		}
	}
	return applied
}
