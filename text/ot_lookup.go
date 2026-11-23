// OpenType LookupList parsing layer — cursor-based, context-rich errors.
//
// This file replaces the previous bare-slice lookup parsing with a
// bounds-checked cursor (otCursor) and a structured result (otLookupList).
// Every malformed structure is reported as an OTLookupError carrying the
// table type, lookup index, subtable index, and byte range, so shaping
// failures in corrupt fonts can be diagnosed instead of silently skipped.
//
// Two parse modes are supported (otParseMode):
//
//   - Lenient: malformed lookups/subtables are skipped (replaced by empty
//     placeholders so feature→lookup indices stay stable) and parsing
//     continues; all errors are collected in otLookupList.errors.
//   - Strict: the first malformed structure aborts the parse and is
//     returned as the error.
//
// The parser never mutates its input, is fully iterative (no recursion, so
// deeply corrupt tables cannot overflow the stack), and produces identical
// results on repeated calls over the same data.
//
// Reference: https://learn.microsoft.com/en-us/typography/opentype/spec/chapter2
package text

import (
	"encoding/binary"
	"fmt"
)

// otLookupFlagUseMarkFilteringSet is the lookupFlag bit (bit 4) indicating
// that a markFilteringSet uint16 follows the subtable offset array.
const otLookupFlagUseMarkFilteringSet = 0x0010

// OTLookupError describes a malformed structure found while parsing an
// OpenType Layout (GSUB/GPOS) LookupList.
type OTLookupError struct {
	Table         string // table type, e.g. "GSUB" or "GPOS"
	LookupIndex   int    // index into the LookupList, -1 if at list level
	SubtableIndex int    // index into the lookup's subtable array, -1 if at lookup level
	Offset        int    // byte offset of the offending region, relative to the LookupList data
	Length        int    // length of the offending region in bytes (0 if not applicable)
	Reason        string // human-readable description of the violation
}

// Error implements the error interface.
func (e *OTLookupError) Error() string {
	s := "text: " + e.Table
	if e.LookupIndex >= 0 {
		s += fmt.Sprintf(" lookup %d", e.LookupIndex)
	}
	if e.SubtableIndex >= 0 {
		s += fmt.Sprintf(" subtable %d", e.SubtableIndex)
	}
	return fmt.Sprintf("%s: %s (byte range [%d, %d))", s, e.Reason, e.Offset, e.Offset+e.Length)
}

// otParseMode controls how the lookup parser reacts to malformed data.
type otParseMode int

const (
	// otParseLenient skips malformed lookups/subtables, keeps the rest of
	// the table usable, and collects all errors in the result.
	otParseLenient otParseMode = iota
	// otParseStrict aborts the whole table parse on the first malformed
	// structure and returns it as the error.
	otParseStrict
)

// otCursor is a bounds-checked reader over OpenType table data.
// It records the first violation as an OTLookupError with the current
// context (table, lookup index, subtable index); once failed, all
// subsequent reads are no-ops so callers can chain reads without
// checking every one.
type otCursor struct {
	data      []byte
	pos       int
	table     string // "GSUB" / "GPOS" for error context
	lookupIdx int    // current lookup index, -1 at list level
	subIdx    int    // current subtable index, -1 at lookup level
	err       *OTLookupError
}

// fail records the first parse error. offset and length describe the byte
// range (relative to the cursor's data) that could not be satisfied.
func (c *otCursor) fail(offset, length int, reason string) {
	if c.err != nil {
		return
	}
	if offset < 0 {
		offset = 0
	}
	if length < 0 {
		length = 0
	}
	c.err = &OTLookupError{
		Table:         c.table,
		LookupIndex:   c.lookupIdx,
		SubtableIndex: c.subIdx,
		Offset:        offset,
		Length:        length,
		Reason:        reason,
	}
}

// ok reports whether no error has been recorded.
func (c *otCursor) ok() bool { return c.err == nil }

// u16 reads a big-endian uint16 at the cursor position.
func (c *otCursor) u16() uint16 {
	if !c.ok() {
		return 0
	}
	if c.pos+2 > len(c.data) {
		c.fail(c.pos, len(c.data)-c.pos,
			fmt.Sprintf("truncated uint16 at offset %d (need 2 bytes, have %d)", c.pos, len(c.data)-c.pos))
		return 0
	}
	v := binary.BigEndian.Uint16(c.data[c.pos:])
	c.pos += 2
	return v
}

// count reads a uint16 count and validates that count records of
// recordSize bytes each fit in the remaining data. This unifies the
// offset/length/count-overflow validation for all count-prefixed arrays.
func (c *otCursor) count(recordSize int, what string) int {
	n := int(c.u16())
	if !c.ok() {
		return 0
	}
	remaining := len(c.data) - c.pos
	if n > remaining/recordSize {
		c.fail(c.pos-2, 2,
			fmt.Sprintf("%s count %d overflows remaining data (%d bytes, record size %d)",
				what, n, remaining, recordSize))
		return 0
	}
	return n
}

// otSubtable is a parsed subtable reference: a bounded view into the
// LookupList data together with its location for diagnostics.
type otSubtable struct {
	data   []byte // subtable bytes (from the subtable offset to the end of the lookup data)
	offset int    // byte offset of the subtable relative to the LookupList data
}

// otLookup represents a parsed Lookup table header.
type otLookup struct {
	lookupType uint16
	lookupFlag uint16
	// markFilteringSet is the extension field present when lookupFlag has
	// the UseMarkFilteringSet bit set. It is parsed and retained for future
	// mark filtering support; hasMarkFilteringSet reports its validity.
	markFilteringSet    uint16
	hasMarkFilteringSet bool
	subtables           []otSubtable
}

// otLookupList is the result of parsing a LookupList: the parsed lookups
// plus one error per malformed structure encountered (lenient mode).
// In lenient mode a lookup that could not be parsed is kept as an empty
// placeholder so that lookup indices referenced by the FeatureList remain
// stable; only its (empty) subtable set differs from a valid lookup.
type otLookupList struct {
	lookups []otLookup
	errors  []OTLookupError
}

// parseLookupListEx parses the LookupList at the start of data.
// table is the table type name ("GSUB"/"GPOS") used in error contexts.
//
// In lenient mode it always returns a non-nil result and a nil error;
// malformed structures are recorded in result.errors. In strict mode the
// first malformed structure aborts the parse: the result is nil and the
// error is the offending *OTLookupError.
func parseLookupListEx(data []byte, table string, mode otParseMode) (*otLookupList, error) {
	res := &otLookupList{}
	cur := &otCursor{data: data, table: table, lookupIdx: -1, subIdx: -1}

	lookupCount := cur.count(2, "lookup")
	if !cur.ok() {
		return finishLookupList(res, *cur.err, mode)
	}

	// Read the lookup offset array first so a truncated array rejects the
	// whole list (matching the historical behavior) before any lookup is
	// parsed.
	offsets := make([]int, lookupCount)
	for i := range offsets {
		cur.lookupIdx = i
		offsets[i] = int(cur.u16())
	}
	if !cur.ok() {
		return finishLookupList(res, *cur.err, mode)
	}

	for i, off := range offsets {
		if off >= len(data) {
			otErr := OTLookupError{
				Table:         table,
				LookupIndex:   i,
				SubtableIndex: -1,
				Offset:        off,
				Length:        0,
				Reason:        fmt.Sprintf("lookup offset %d out of range (LookupList size %d)", off, len(data)),
			}
			if mode == otParseStrict {
				return nil, &otErr
			}
			res.errors = append(res.errors, otErr)
			// Placeholder keeps lookup indices stable for the FeatureList.
			res.lookups = append(res.lookups, otLookup{})
			continue
		}
		lu, errs := parseLookupTableEx(data[off:], off, table, i)
		if len(errs) > 0 && mode == otParseStrict {
			return nil, &errs[0]
		}
		res.errors = append(res.errors, errs...)
		res.lookups = append(res.lookups, lu)
	}
	return res, nil
}

// finishLookupList converts a list-level cursor failure into the return
// values for the requested mode.
func finishLookupList(res *otLookupList, otErr OTLookupError, mode otParseMode) (*otLookupList, error) {
	if mode == otParseStrict {
		return nil, &otErr
	}
	res.errors = append(res.errors, otErr)
	return res, nil
}

// parseLookupTableEx parses a single Lookup table. data starts at the
// lookup; base is the lookup's offset relative to the LookupList data
// (used only for error byte ranges). Parsing is iterative and bounded by
// the declared subtable count, so corrupt tables cannot cause unbounded
// work or recursion.
func parseLookupTableEx(data []byte, base int, table string, lookupIdx int) (otLookup, []OTLookupError) {
	var lu otLookup
	cur := &otCursor{data: data, table: table, lookupIdx: lookupIdx, subIdx: -1}

	lu.lookupType = cur.u16()
	lu.lookupFlag = cur.u16()
	subtableCount := cur.count(2, "subtable")
	if !cur.ok() {
		return lu, []OTLookupError{shiftError(*cur.err, base)}
	}

	offsets := make([]int, subtableCount)
	for i := range offsets {
		offsets[i] = int(cur.u16())
	}
	if !cur.ok() {
		return lu, []OTLookupError{shiftError(*cur.err, base)}
	}

	// Extension field: retained in the structure even though mark
	// filtering is not yet applied during shaping.
	if lu.lookupFlag&otLookupFlagUseMarkFilteringSet != 0 {
		lu.markFilteringSet = cur.u16()
		if !cur.ok() {
			return lu, []OTLookupError{shiftError(*cur.err, base)}
		}
		lu.hasMarkFilteringSet = true
	}

	var errs []OTLookupError
	for i, off := range offsets {
		if off >= len(data) {
			errs = append(errs, OTLookupError{
				Table:         table,
				LookupIndex:   lookupIdx,
				SubtableIndex: i,
				Offset:        base + off,
				Length:        0,
				Reason:        fmt.Sprintf("subtable offset %d out of range (lookup data size %d)", off, len(data)),
			})
			continue
		}
		lu.subtables = append(lu.subtables, otSubtable{data: data[off:], offset: base + off})
	}
	return lu, errs
}

// shiftError rebases a cursor error's byte range (relative to a sub-slice)
// to the LookupList data by adding base.
func shiftError(e OTLookupError, base int) OTLookupError {
	e.Offset += base
	return e
}
