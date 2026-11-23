// Package bidix is an internal, trimmed copy of the Unicode Bidirectional
// Algorithm implementation from golang.org/x/text/unicode/bidi (BSD
// 3-Clause license, Copyright 2015 The Go Authors).
//
// The upstream package only exposes the *parity* (odd/even) of resolved
// embedding levels through its Paragraph/Ordering API. Layout needs the
// numeric levels to apply rule L2 (reorder) exactly as specified in UAX #9,
// including nested embeddings and isolates. Because all algorithm types in
// the upstream package are unexported, the reference implementation is
// vendored here with a small exported surface added in this file.
//
// Only the pieces required by the text layout engine are exported:
// per-paragraph resolved levels (rules P1–I2, N0), line-level whitespace
// resetting (rule L1), the L2 reordering array, and bidi class lookups.
package bidix

// ParagraphDirection selects the paragraph embedding level (rule P1).
type ParagraphDirection int

const (
	// DirectionDefault determines the paragraph level from the first
	// strong character (rules P2, P3).
	DirectionDefault ParagraphDirection = iota
	// DirectionLeftToRight forces paragraph embedding level 0.
	DirectionLeftToRight
	// DirectionRightToLeft forces paragraph embedding level 1.
	DirectionRightToLeft
)

// Resolver runs the UAX #9 algorithm over a single paragraph (a string with
// no paragraph separators). It is not safe for concurrent use.
type Resolver struct {
	para    *paragraph
	runes   []rune
	levels  []level // resolved levels after P1–I2 (X9 characters reinserted)
	paraLvl level
}

// NewResolver resolves embedding levels for paragraph text. base selects
// the paragraph embedding level; DirectionDefault applies rules P2/P3.
//
// Invalid UTF-8 is handled the same way as in the upstream package:
// bytes.Runes replaces every ill-formed subsequence with RuneError, whose
// bidi class is R; it never panics and every rune gets a level.
func NewResolver(text string, base ParagraphDirection) *Resolver {
	runes := []rune(text)
	n := len(runes)
	if n == 0 {
		return &Resolver{runes: runes}
	}

	types := make([]Class, n)
	pairTypes := make([]bracketType, n)
	pairValues := make([]rune, n)
	for i, r := range runes {
		// Rule N0 preprocessing: bracket characters with canonical
		// (NFKD) decompositions are normalized before pairing, matching
		// the upstream UAX #9 conformance harness. The bidi class is
		// looked up on the original rune.
		props, _ := LookupRune(r)
		cls := props.Class()
		if cls == B {
			// A paragraph separator must terminate the paragraph
			// (see prepareInput upstream). Layout splits on line
			// breaks before calling us, but stay defensive: treat a
			// stray B as whitespace-like ON rather than aborting.
			cls = ON
		}
		types[i] = cls
		switch {
		case !props.IsBracket():
			pairTypes[i] = bpNone
			pairValues[i] = 0
		case props.IsOpeningBracket():
			pairTypes[i] = bpOpen
			pairValues[i] = canonicalBracket(r)
		default:
			pairTypes[i] = bpClose
			// pairValue identifies the matching *opening* bracket.
			pairValues[i] = canonicalBracket(props.reverseBracket(r))
		}
	}

	var embedLevel level = implicitLevel
	switch base {
	case DirectionLeftToRight:
		embedLevel = 0
	case DirectionRightToLeft:
		embedLevel = 1
	}

	p, err := newParagraph(types, pairTypes, pairValues, embedLevel)
	if err != nil {
		// Validation only rejects empty/mismatched inputs, which cannot
		// happen with the arrays built above. Fall back to an all-default
		// resolver rather than panicking.
		return &Resolver{runes: runes}
	}

	resolved := p.getLevels([]int{n})
	return &Resolver{
		para:    p,
		runes:   runes,
		levels:  resolved,
		paraLvl: p.embeddingLevel,
	}
}

// RuneCount returns the number of runes (including RuneError placeholders
// for invalid UTF-8) in the resolved paragraph.
func (r *Resolver) RuneCount() int { return len(r.runes) }

// ParagraphLevel returns the resolved paragraph embedding level (0 for LTR,
// 1 for RTL). For an empty resolver it returns 0.
func (r *Resolver) ParagraphLevel() int {
	if r.paraLvl < 0 {
		return 0
	}
	return int(r.paraLvl)
}

// LevelAt returns the resolved embedding level of rune index i.
// Rule L1 has not been applied yet at this point; use LineReorder for
// final per-line levels. An out-of-range index yields the paragraph level.
func (r *Resolver) LevelAt(i int) int {
	if i < 0 || i >= len(r.levels) {
		return r.ParagraphLevel()
	}
	l := r.levels[i]
	if l < 0 {
		return r.ParagraphLevel()
	}
	return int(l)
}

// IsRTLAt reports whether rune i resolves to an odd (right-to-left) level.
func (r *Resolver) IsRTLAt(i int) bool { return r.LevelAt(i)%2 == 1 }

// LineReorder returns the UAX #9 rule L2 visual-to-logical reordering array
// for the rune range [start, end) of this paragraph.
//
// Rule L1 (whitespace/segment-separator reset at line ends) is applied to
// the range first. The returned values are paragraph-global rune indices:
// the leftmost visual position k holds the logical rune result[k]. The range
// must lie within the paragraph; an empty or invalid range returns nil.
func (r *Resolver) LineReorder(start, end int) []int {
	n := len(r.runes)
	if start < 0 || end > n || start >= end || len(r.levels) == 0 || r.para == nil {
		return nil
	}

	// Work on a copy so repeated calls stay independent.
	lineLevels := append([]level(nil), r.levels[start:end]...)

	// Rule L1 on the sub-line: reset trailing whitespace (including the
	// classes the reference treats as whitespace, see isWhitespace) to
	// the paragraph embedding level. Segment/paragraph separators inside
	// the line are reset together with any preceding whitespace run.
	paraLvl := r.paraLvl
	if paraLvl < 0 {
		paraLvl = 0
	}
	for j := end - 1; j >= start; j-- {
		t := r.para.initialTypes[j]
		if t == B || t == S {
			lineLevels[j-start] = paraLvl
			for k := j - 1; k >= start && isWhitespace(r.para.initialTypes[k]); k-- {
				lineLevels[k-start] = paraLvl
			}
			continue
		}
	}
	for j := end - 1; j >= start; j-- {
		if isWhitespace(r.para.initialTypes[j]) {
			lineLevels[j-start] = paraLvl
		} else {
			break
		}
	}

	local := computeReordering(lineLevels)
	if local == nil {
		return nil
	}
	// Translate slice-local indices back to paragraph-global rune indices.
	global := make([]int, len(local))
	for i, idx := range local {
		global[i] = idx + start
	}
	return global
}

// RuneClass returns the resolved input bidi class (before algorithm
// processing) of rune r. This is primarily useful for tests.
func RuneClass(r rune) Class {
	props, _ := LookupRune(r)
	return props.Class()
}

// IsBidiControl reports whether r is one of the explicit embedding/override/
// isolate formatting characters removed from shaping by rule X9.
func IsBidiControl(r rune) bool {
	switch RuneClass(r) {
	case LRO, RLO, LRE, RLE, PDF, LRI, RLI, FSI, PDI:
		return true
	default:
		return false
	}
}

// canonicalBracket maps bracket characters that have an NFKD decomposition
// to their canonical form, as required by the paired-bracket preprocessing of
// rule N0 (see the upstream BidiCharacterTest harness which performs
// norm.NFKD decomposition before running the algorithm). Every entry is a
// bracket whose compatibility decomposition is itself a bracket; all other
// brackets pass through unchanged.
func canonicalBracket(r rune) rune {
	if mapped, ok := nfdkBracketMap[r]; ok {
		return mapped
	}
	return r
}

// nfdkBracketMap is the complete set of bracket characters with a non-empty
// NFKD decomposition in the Unicode BidiBrackets property (derived from
// BidiBrackets.txt via the x/text/unicode/norm package).
var nfdkBracketMap = map[rune]rune{
	0x207D: 0x0028, // ⁽ SUPERSCRIPT LEFT PARENTHESIS
	0x207E: 0x0029, // ⁾ SUPERSCRIPT RIGHT PARENTHESIS
	0x208D: 0x0028, // ₍ SUBSCRIPT LEFT PARENTHESIS
	0x208E: 0x0029, // ₎ SUBSCRIPT RIGHT PARENTHESIS
	0x2329: 0x3008, // 〈 LEFT-POINTING ANGLE BRACKET
	0x232A: 0x3009, // 〉 RIGHT-POINTING ANGLE BRACKET
	0xFE59: 0x0028, // ﹙ SMALL LEFT PARENTHESIS
	0xFE5A: 0x0029, // ﹚ SMALL RIGHT PARENTHESIS
	0xFE5B: 0x007B, // ﹛ SMALL LEFT CURLY BRACKET
	0xFE5C: 0x007D, // ﹜ SMALL RIGHT CURLY BRACKET
	0xFE5D: 0x3014, // ﹝ SMALL LEFT TORTOISE SHELL BRACKET
	0xFE5E: 0x3015, // ﹞ SMALL RIGHT TORTOISE SHELL BRACKET
	0xFF08: 0x0028, // （ FULLWIDTH LEFT PARENTHESIS
	0xFF09: 0x0029, // ） FULLWIDTH RIGHT PARENTHESIS
	0xFF3B: 0x005B, // ［ FULLWIDTH LEFT SQUARE BRACKET
	0xFF3D: 0x005D, // ］ FULLWIDTH RIGHT SQUARE BRACKET
	0xFF5B: 0x007B, // ｛ FULLWIDTH LEFT CURLY BRACKET
	0xFF5D: 0x007D, // ｝ FULLWIDTH RIGHT CURLY BRACKET
	0xFF5F: 0x2985, // ｟ FULLWIDTH LEFT WHITE PARENTHESIS
	0xFF60: 0x2986, // ｠ FULLWIDTH RIGHT WHITE PARENTHESIS
	0xFF62: 0x300C, // 「 CORNER BRACKET
	0xFF63: 0x300D, // 」 WHITE CORNER BRACKET
}
