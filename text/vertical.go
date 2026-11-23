// Vertical text layout — top-to-bottom (TTB) and bottom-to-top (BTT) columns.
//
// Vertical layout reuses the existing segmenter/shaper pipeline and remaps
// horizontal shaped output onto a vertical baseline:
//
//   - Upright glyphs keep their normal orientation and advance along the
//     block axis using vmtx metrics (or synthesized em-square metrics when
//     the font has no vhea/vmtx tables).
//   - Sideways glyph runs are rotated 90° clockwise around the column
//     center line and keep their horizontal advance widths, so Latin words
//     remain readable when the head is tilted to the right.
//
// Hard line breaks start new columns. MaxWidth is reinterpreted as the
// maximum column height; non-finite or <= 0 values disable wrapping.
package text

import (
	"math"
	"unicode"

	"github.com/gogpu/gg/text/emoji"
)

// VerticalOrient describes how a glyph is oriented within a vertical column.
type VerticalOrient uint8

const (
	// VerticalOrientUpright keeps the glyph in its normal (horizontal-font)
	// orientation. This is the zero value and the correct orientation for
	// every horizontal-layout glyph.
	VerticalOrientUpright VerticalOrient = iota

	// VerticalOrientSideways rotates the glyph 90° clockwise so its top
	// points toward the column center line (CSS text-orientation: sideways).
	VerticalOrientSideways
)

// String returns the string representation of the orientation.
func (o VerticalOrient) String() string {
	switch o {
	case VerticalOrientUpright:
		return "Upright"
	case VerticalOrientSideways:
		return "Sideways"
	default:
		return unknownStr
	}
}

// VerticalClass classifies a rune for the purpose of vertical orientation
// resolution (CSS Writing Modes Level 4, simplified).
type VerticalClass uint8

const (
	// VerticalClassLatin covers Latin, Greek, Cyrillic and other European
	// alphabetic scripts, plus ASCII digits and European punctuation.
	VerticalClassLatin VerticalClass = iota

	// VerticalClassCJK covers Han, kana, Hangul and CJK punctuation.
	VerticalClassCJK

	// VerticalClassPunctuation covers non-CJK punctuation (brackets, dashes,
	// quotes, ...). CJK punctuation is classified as VerticalClassCJK.
	VerticalClassPunctuation

	// VerticalClassMark covers combining marks (Mn/Mc/Me). Marks inherit
	// the orientation of their base glyph.
	VerticalClassMark

	// VerticalClassEmoji covers emoji and emoji presentation sequences.
	VerticalClassEmoji
)

// String returns the string representation of the class.
func (c VerticalClass) String() string {
	switch c {
	case VerticalClassLatin:
		return "Latin"
	case VerticalClassCJK:
		return "CJK"
	case VerticalClassPunctuation:
		return "Punctuation"
	case VerticalClassMark:
		return "Mark"
	case VerticalClassEmoji:
		return "Emoji"
	default:
		return unknownStr
	}
}

// ColumnFlow selects the direction in which vertical columns progress.
type ColumnFlow uint8

const (
	// ColumnFlowDefault gives the conventional flow for each block
	// direction: TTB columns progress right-to-left (traditional East
	// Asian), BTT columns progress left-to-right.
	ColumnFlowDefault ColumnFlow = iota

	// ColumnFlowLeftToRight forces columns to progress left-to-right.
	ColumnFlowLeftToRight

	// ColumnFlowRightToLeft forces columns to progress right-to-left.
	ColumnFlowRightToLeft
)

// String returns the string representation of the column flow.
func (f ColumnFlow) String() string {
	switch f {
	case ColumnFlowDefault:
		return "Default"
	case ColumnFlowLeftToRight:
		return "LeftToRight"
	case ColumnFlowRightToLeft:
		return "RightToLeft"
	default:
		return unknownStr
	}
}

// VerticalOptions configures vertical (column-based) layout.
//
// Per-class mode fields override Orientation for that class. Their zero
// value is VerticalOrientUpright; callers that want an override to "fall
// through" to the global policy leave them unset and use ClassModes.
type VerticalOptions struct {
	// Orientation is the global orientation policy.
	// VerticalOrientSideways reproduces CSS text-orientation: sideways
	// (everything rotated). The zero value, VerticalOrientUpright, applies
	// CSS "mixed" rules: CJK upright, rotated scripts sideways.
	Orientation VerticalOrient

	// ColumnSpacing is added between adjacent columns on top of the font's
	// vertical line gap. Zero uses the font's natural column spacing only.
	ColumnSpacing float64

	// Flow overrides the default column progression direction.
	Flow ColumnFlow

	// ClassModes optionally overrides the orientation for a whole class.
	// A missing entry means the class follows Orientation/auto rules.
	ClassModes map[VerticalClass]VerticalOrient
}

// DefaultVerticalOptions returns conventional CJK vertical settings:
// mixed orientation (CJK upright, Latin sideways), natural column spacing,
// and the script-default column flow.
func DefaultVerticalOptions() VerticalOptions {
	return VerticalOptions{}
}

// ClassifyVerticalRune assigns a vertical orientation class to r.
func ClassifyVerticalRune(r rune) VerticalClass {
	switch {
	case emoji.IsEmoji(r):
		return VerticalClassEmoji
	case unicode.IsMark(r):
		return VerticalClassMark
	case IsCJKRune(r) || isCJKBracketOrPunct(r):
		// Checked before unicode.IsPunct/IsSymbol because CJK punctuation
		// (e.g. U+3002 ideographic full stop) also satisfies those
		// predicates but must stay upright in vertical text.
		return VerticalClassCJK
	case unicode.IsPunct(r) || unicode.IsSymbol(r):
		return VerticalClassPunctuation
	default:
		return VerticalClassLatin
	}
}

// isCJKBracketOrPunct reports CJK-specific punctuation that stays upright
// in vertical text: fullwidth ASCII variants, CJK brackets and the CJK
// symbols/punctuation block. The generic IsCJKRune already covers the
// fullwidth forms 0xFF00-0xFFEF; this adds the 0x3000-0x303F block and
// a handful of vertical-specific compatibility codepoints.
func isCJKBracketOrPunct(r rune) bool {
	switch {
	case r >= 0x3000 && r <= 0x303F: // CJK Symbols and Punctuation (、。「」 etc.)
		return true
	case r >= 0xFE10 && r <= 0xFE1F: // Vertical Forms
		return true
	case r >= 0xFE30 && r <= 0xFE4F: // CJK Compatibility Forms (vertical brackets)
		return true
	default:
		return false
	}
}

// autoVerticalOrient implements the CSS "mixed" default for a class:
// CJK and emoji stay upright, Latin and European punctuation are rotated,
// marks defer to their base (handled by the caller).
func autoVerticalOrient(class VerticalClass) VerticalOrient {
	switch class {
	case VerticalClassCJK, VerticalClassEmoji, VerticalClassMark:
		return VerticalOrientUpright
	default:
		return VerticalOrientSideways
	}
}

// finiteWidth reports whether w is a usable positive dimension. NaN and
// infinities disable wrapping just like a zero width.
func finiteWidth(w float64) bool {
	return w > 0 && !math.IsInf(w, 0) && !math.IsNaN(w)
}
