// Vertical font metrics — vhea and vmtx table parsing.
//
// Provides vertical advance heights and top side bearings for fonts that
// ship vertical-metrics tables (most CJK fonts). Fonts without these tables
// fall back to synthesized metrics (em-square advance, centered origin),
// matching the OpenType vertical-text recommendations.
//
// Coordinate convention (font units, Y-up, matching the rest of the
// parser stack):
//   - VerticalOriginY is measured from the Y=0 baseline upward. The
//     vertical origin sits on the vertical baseline of the glyph.
//   - TopSideBearing is the distance from the vertical origin downward to
//     the top of the glyph's bounding box (stored positive for the common
//     case where the origin is above the box).
//
// Reference: https://learn.microsoft.com/en-us/typography/opentype/spec/vhea
// Reference: https://learn.microsoft.com/en-us/typography/opentype/spec/vmtx
package text

import "encoding/binary"

// VerticalMetric holds the vertical metrics of a single glyph in font units.
type VerticalMetric struct {
	// AdvanceHeight is the vertical advance (distance between consecutive
	// vertical origins along the block-progression axis), in font units.
	AdvanceHeight int16

	// TopSideBearing is the offset from the vertical origin to the top of
	// the glyph bounding box, in font units (Y-up coordinates).
	TopSideBearing int16
}

// vheaMetrics holds relevant fields from the vhea table.
type vheaMetrics struct {
	// vertTypoAscender/Descender define the vertical typographic extent.
	vertTypoAscender  int16
	vertTypoDescender int16
	vertTypoLineGap   int16
	// advanceHeightMax is the maximum vmtx advance height.
	advanceHeightMax int16
	numberOfVMetrics int
}

// parseVheaTable parses the vhea (Vertical Header) table.
//
// Layout (36 bytes, versions 1.0 and 1.1 share the fixed fields used here):
//
//	uint32 version
//	int16  vertTypoAscender         [offset 4]
//	int16  vertTypoDescender        [offset 6]
//	int16  vertTypoLineGap          [offset 8]
//	int16  advanceHeightMax         [offset 10]
//	int16  minTopSideBearing        [offset 12]
//	int16  minBottomSideBearing     [offset 14]
//	int16  yMaxExtent               [offset 16]
//	int16  caretSlopeRise           [offset 18]
//	int16  caretSlopeRun            [offset 20]
//	int16  caretOffset              [offset 22]
//	int16  reserved[4]              [offset 24-30]
//	int16  metricDataFormat         [offset 32]
//	uint16 numberOfLongVerMetrics   [offset 34]
func parseVheaTable(data []byte) (vheaMetrics, bool) {
	if len(data) < 36 {
		return vheaMetrics{}, false
	}
	return vheaMetrics{
		vertTypoAscender:  int16(binary.BigEndian.Uint16(data[4:6])),
		vertTypoDescender: int16(binary.BigEndian.Uint16(data[6:8])),
		vertTypoLineGap:   int16(binary.BigEndian.Uint16(data[8:10])),
		advanceHeightMax:  int16(binary.BigEndian.Uint16(data[10:12])),
		numberOfVMetrics:  int(binary.BigEndian.Uint16(data[34:36])),
	}, true
}

// parseVmtx parses the vmtx table into per-glyph long metrics plus the
// top-side-bearing tail, mirroring parseHmtx for hmtx.
//
// Long metrics (numberOfLongVerMetrics entries, 4 bytes each):
//
//	uint16 advanceHeight
//	int16  topSideBearing
//
// Remaining glyphs inherit the last advance height and carry only an
// int16 top side bearing.
//
// Truncated tables are tolerated: missing tail bearings stay zero.
func parseVmtx(data []byte, numLongMetrics, numGlyphs int) (advances []int16, tsbs []int16, ok bool) {
	longSize := numLongMetrics * 4
	if numLongMetrics <= 0 || len(data) < longSize {
		return nil, nil, false
	}

	advances = make([]int16, numLongMetrics)
	tsbs = make([]int16, numGlyphs)

	for i := range numLongMetrics {
		advances[i] = int16(binary.BigEndian.Uint16(data[i*4 : i*4+2]))
		tsbs[i] = int16(binary.BigEndian.Uint16(data[i*4+2 : i*4+4]))
	}

	remaining := numGlyphs - numLongMetrics
	if remaining > 0 {
		need := longSize + remaining*2
		if len(data) < need {
			// Tolerate truncated vmtx — remaining TSBs default to 0.
			return advances, tsbs, true
		}
		for i := range remaining {
			tsbs[numLongMetrics+i] = int16(binary.BigEndian.Uint16(data[longSize+i*2 : longSize+i*2+2]))
		}
	}

	return advances, tsbs, true
}

// vmtxAdvance returns the vertical advance in font units for the given
// glyph ID. Glyphs beyond the long-metric count use the last advance
// (monospace tail), mirroring hmtxAdvance.
func vmtxAdvance(advances []int16, numLongMetrics int, glyphID uint16) int16 {
	gid := int(glyphID)
	if gid < numLongMetrics && gid < len(advances) {
		return advances[gid]
	}
	if numLongMetrics > 0 && len(advances) > 0 {
		return advances[numLongMetrics-1]
	}
	return 0
}
