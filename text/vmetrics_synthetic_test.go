package text

import (
	"encoding/binary"
	"math"
	"testing"
)

// syntheticVerticalFont builds a minimal valid sfnt containing exactly the
// tables ownParser requires (head, maxp) plus hhea/hmtx and the vertical
// vhea/vmtx pair. It has no cmap and no outlines; vertical metric lookups
// are exercised directly by GID.
//
// Layout:
//
//	sfnt header (12 bytes) + table records (16 bytes each)
//	table data appended in tag-sorted order: head, hhea, hmtx, maxp, vhea, vmtx
func syntheticVerticalFont(t *testing.T, numGlyphs int, vAdvances []int16, vTSBs []int16) []byte {
	t.Helper()

	const upem = 1000
	head := buildSyntheticHead(upem)
	hhea := buildVheaLikeHhea(uint16(numGlyphs))
	hmtx := buildSyntheticHmtx(numGlyphs)
	maxp := buildSyntheticMaxp(numGlyphs)
	vhea := buildVhea(880, -120, 40, 1000, uint16(len(vAdvances)))
	vmtx := buildVmtx(zipVmtxLong(vAdvances, vTSBs), vTSBtail(vTSBs, len(vAdvances), numGlyphs))

	tables := map[string][]byte{
		"head": head, "hhea": hhea, "hmtx": hmtx, "maxp": maxp,
		"vhea": vhea, "vmtx": vmtx,
	}
	return assembleSFNT(tables)
}

// buildVheaLikeHhea produces a 36-byte hhea with numberOfHMetrics set.
func buildVheaLikeHhea(numHMetrics uint16) []byte {
	data := make([]byte, 36)
	binary.BigEndian.PutUint32(data[0:4], 0x00010000)
	binary.BigEndian.PutUint16(data[4:6], 800) // ascent
	binary.BigEndian.PutUint16(data[6:8], 200) // descent (stored negative-aware)
	binary.BigEndian.PutUint16(data[34:36], numHMetrics)
	return data
}

// buildSyntheticHmtx gives every glyph a 500-unit horizontal advance with
// zero LSB.
func buildSyntheticHmtx(numGlyphs int) []byte {
	data := make([]byte, numGlyphs*4)
	for i := range numGlyphs {
		binary.BigEndian.PutUint16(data[i*4:i*4+2], 500)
	}
	return data
}

// buildSyntheticMaxp builds the 6-byte maxp prefix ownParser reads.
func buildSyntheticMaxp(numGlyphs int) []byte {
	data := make([]byte, 6)
	binary.BigEndian.PutUint32(data[0:4], 0x00005000) // version 0.5
	binary.BigEndian.PutUint16(data[4:6], uint16(numGlyphs))
	return data
}

// buildSyntheticHead builds a 54-byte head table with the given unitsPerEm.
func buildSyntheticHead(upem int) []byte {
	data := make([]byte, 54)
	binary.BigEndian.PutUint32(data[0:4], 0x00010000)
	binary.BigEndian.PutUint16(data[18:20], uint16(upem))
	return data
}

// zipVmtxLong pairs advances and TSBs into long-metric records.
func zipVmtxLong(advances, tsbs []int16) [][2]int16 {
	long := make([][2]int16, len(advances))
	for i := range advances {
		tsb := int16(0)
		if i < len(tsbs) {
			tsb = tsbs[i]
		}
		long[i] = [2]int16{advances[i], tsb}
	}
	return long
}

// vTSBtail returns the trailing TSB-only entries beyond the long metrics.
func vTSBtail(tsbs []int16, numLong, numGlyphs int) []int16 {
	if numGlyphs <= numLong || len(tsbs) <= numLong {
		return nil
	}
	return tsbs[numLong:min(numGlyphs, len(tsbs))]
}

// assembleSFNT writes an sfnt container with checksum-padded table data.
func assembleSFNT(tables map[string][]byte) []byte {
	tags := make([]string, 0, len(tables))
	for tag := range tables {
		tags = append(tags, tag)
	}
	// Table records must be sorted by tag.
	for i := 0; i < len(tags); i++ {
		for j := i + 1; j < len(tags); j++ {
			if tags[j] < tags[i] {
				tags[i], tags[j] = tags[j], tags[i]
			}
		}
	}

	headerSize := 12 + len(tags)*16
	offset := headerSize
	type record struct {
		tag    string
		offset int
		length int
		data   []byte
	}
	records := make([]record, len(tags))
	for i, tag := range tags {
		records[i] = record{tag: tag, offset: offset, length: len(tables[tag]), data: tables[tag]}
		offset += (len(tables[tag]) + 3) &^ 3
	}

	out := make([]byte, offset)
	binary.BigEndian.PutUint32(out[0:4], 0x00010000) // TrueType sfnt
	binary.BigEndian.PutUint16(out[4:6], uint16(len(tags)))
	// searchRange/entrySelector/rangeShift left zero — parsers tolerate this.

	for i, rec := range records {
		base := 12 + i*16
		copy(out[base:base+4], rec.tag)
		binary.BigEndian.PutUint32(out[base+4:base+8], sfntChecksum(rec.data))
		binary.BigEndian.PutUint32(out[base+8:base+12], uint32(rec.offset))
		binary.BigEndian.PutUint32(out[base+12:base+16], uint32(rec.length))
		copy(out[rec.offset:], rec.data)
	}
	return out
}

// sfntChecksum computes the standard uint32 table checksum.
func sfntChecksum(data []byte) uint32 {
	padded := make([]byte, (len(data)+3)&^3)
	copy(padded, data)
	var sum uint32
	for off := 0; off < len(padded); off += 4 {
		sum += binary.BigEndian.Uint32(padded[off : off+4])
	}
	return sum
}

// TestVerticalMetrics_RealVmtx builds a font with vhea/vmtx and verifies
// scaled advances, TSB-derived origins and the vertical line gap.
func TestVerticalMetrics_RealVmtx(t *testing.T) {
	// 3 glyphs: two long metrics, one TSB-tail glyph inheriting advance 900.
	fontData := syntheticVerticalFont(t, 3,
		[]int16{1000, 900},
		[]int16{120, 110, 95},
	)

	source, err := NewFontSource(fontData)
	if err != nil {
		t.Fatalf("NewFontSource: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	parsed := source.Parsed()
	vmp, ok := parsed.(VerticalMetricProvider)
	if !ok {
		t.Fatal("parsed font must implement VerticalMetricProvider")
	}
	if !vmp.HasVerticalMetrics() {
		t.Fatal("synthetic font should report real vertical metrics")
	}

	const ppem = 20.0 // 1000 upem -> scale 0.02
	if got := vmp.VerticalAdvance(0, ppem); math.Abs(got-20.0) > 1e-9 {
		t.Errorf("gid0 advance = %f, want 20", got)
	}
	if got := vmp.VerticalAdvance(2, ppem); math.Abs(got-18.0) > 1e-9 {
		t.Errorf("tail gid advance = %f, want 18", got)
	}

	// vhea line gap 40 units -> 0.8px.
	if got := vmp.VerticalLineGap(ppem); math.Abs(got-0.8) > 1e-9 {
		t.Errorf("line gap = %f, want 0.8", got)
	}

	// Origin Y derives from glyf bounds; this font has no glyf table so
	// bounds are empty (MinY=0), leaving originYUp = TSB*scale.
	_, oy := vmp.VerticalOrigin(0, ppem)
	wantY := -(120.0 * ppem / 1000.0)
	if math.Abs(oy-wantY) > 1e-9 {
		t.Errorf("originY = %f, want %f", oy, wantY)
	}
}
