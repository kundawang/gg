// GDEF table parsing — minimal support needed by GSUB lookup flag handling.
//
// The full GDEF table also contains glyph attachment, ligature caret and
// mark attachment structures; those are consumed by complex-script shaping
// and are not required for contextual GSUB. What we need here is:
//
//   - GlyphClassDef (offset 10 in versions 1.0/1.2/1.3): marks are class 3,
//     bases class 1 and ligatures class 2, used by the Ignore* lookup flags.
//   - MarkGlyphSetsDef (version 1.2+, offset 14): coverage-based mark
//     filtering sets referenced by lookups with the UseMarkFilteringSet flag.
//
// Reference: https://learn.microsoft.com/en-us/typography/opentype/spec/gdef
package text

import "encoding/binary"

// gdefTable holds the subset of GDEF information used during GSUB application.
// A nil *gdefTable is a valid value meaning "no GDEF data available": every
// glyph is then treated as a base and every mark filter set as empty.
type gdefTable struct {
	glyphClassDef otClassDef   // nil when the font has no GlyphClassDef
	markSets      []otCoverage // mark filtering sets, index = set index
}

// parseGDEF parses the raw GDEF table data. Malformed tables degrade to nil so
// shaping can continue without GDEF-based classification rather than failing.
func parseGDEF(data []byte) *gdefTable {
	// Version 1.0 header layout (all offsets uint16 from table start):
	//   [0:4]  Version
	//   [4:6]  GlyphClassDefOffset
	//   [6:8]  AttachListOffset
	//   [8:10] LigCaretListOffset
	//   [10:12] MarkAttachClassDefOffset
	// Version 1.2+ appends:
	//   [12:14] MarkGlyphSetsDefOffset
	if len(data) < 12 {
		return nil
	}
	major := binary.BigEndian.Uint16(data[0:2])
	if major != 1 {
		return nil
	}

	g := &gdefTable{}

	if off := int(binary.BigEndian.Uint16(data[4:6])); off > 0 && off < len(data) {
		g.glyphClassDef = parseClassDef(data[off:])
	}

	// MarkGlyphSetsDef only exists in version 1.2 (0x00010002) and later.
	minor := binary.BigEndian.Uint16(data[2:4])
	if minor >= 2 && len(data) >= 14 {
		if off := int(binary.BigEndian.Uint16(data[12:14])); off > 0 && off < len(data) {
			g.markSets = parseMarkGlyphSets(data[off:])
		}
	}
	return g
}

// parseMarkGlyphSets parses a MarkGlyphSetsDef table:
//
//	MarkSetTableFormat uint16 (must be 1)
//	MarkSetCount       uint16
//	Coverage[count]    Offset32 from the start of the MarkGlyphSetsDef table
func parseMarkGlyphSets(data []byte) []otCoverage {
	if len(data) < 4 {
		return nil
	}
	format := binary.BigEndian.Uint16(data[0:2])
	if format != 1 {
		return nil
	}
	count := int(binary.BigEndian.Uint16(data[2:4]))
	// 32-bit offsets: 4 bytes header + count*4.
	if len(data) < 4+count*4 {
		return nil
	}
	sets := make([]otCoverage, 0, count)
	for i := range count {
		off := int(binary.BigEndian.Uint32(data[4+i*4 : 8+i*4]))
		if off <= 0 || off >= len(data) {
			// A bad coverage offset makes the whole set unmatchable; keep a
			// nil slot so index-based lookups still line up.
			sets = append(sets, nil)
			continue
		}
		sets = append(sets, parseCoverage(data[off:]))
	}
	return sets
}

// isMark reports whether the glyph is classified as a mark (GDEF class 3).
// Without a GDEF table or GlyphClassDef, no glyph is considered a mark.
func (g *gdefTable) isMark(gid uint16) bool {
	if g == nil || g.glyphClassDef == nil {
		return false
	}
	return g.glyphClassDef.classOf(gid) == 3
}

// glyphClass returns the GDEF glyph class (0 = unassigned, 1 = base,
// 2 = ligature, 3 = mark).
func (g *gdefTable) glyphClass(gid uint16) uint16 {
	if g == nil || g.glyphClassDef == nil {
		return 0
	}
	return g.glyphClassDef.classOf(gid)
}

// markSetCovers reports whether markSetIndex contains gid. Out-of-range set
// indices and sets whose coverage failed to parse never match.
func (g *gdefTable) markSetCovers(markSetIndex int, gid uint16) bool {
	if g == nil || markSetIndex < 0 || markSetIndex >= len(g.markSets) {
		return false
	}
	cov := g.markSets[markSetIndex]
	if cov == nil {
		return false
	}
	_, ok := cov.contains(gid)
	return ok
}
