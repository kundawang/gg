package bidix

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestUAX9Conformance runs every line of the official BidiCharacterTest.txt
// conformance suite (UAX #9) against the vendored engine:
//   - resolved paragraph embedding level
//   - per-rune resolved levels (full numeric values, not just parity)
//   - rule L2 visual order (with X9-removed characters filtered out)
//
// The data file under testdata is Unicode 17.0.0; the engine uses the
// Unicode 15.0.0 generated tables on Go < 1.27 and 17.0.0 on Go >= 1.27.
// Rows that differ only because a code point's bidi class changed between
// those versions are skipped via uax9KnownVersionDiffs.
func TestUAX9Conformance(t *testing.T) {
	path := filepath.Join("testdata", "BidiCharacterTest.txt")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var rows, levelFails, orderFails int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip trailing comments.
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		fields := strings.Split(line, ";")
		if len(fields) < 5 {
			t.Fatalf("line %d: malformed row %q", lineNo, line)
		}

		cpFields := strings.Fields(fields[0])
		runes := make([]rune, len(cpFields))
		for i, h := range cpFields {
			v, err := strconv.ParseUint(h, 16, 32)
			if err != nil {
				t.Fatalf("line %d: bad code point %q", lineNo, h)
			}
			runes[i] = rune(v)
		}
		text := string(runes)

		inputParaLevel, _ := strconv.Atoi(strings.TrimSpace(fields[1]))
		wantParaLevel, _ := strconv.Atoi(strings.TrimSpace(fields[2]))
		wantLevels := strings.Fields(fields[3])
		wantOrder := strings.Fields(fields[4])

		var base ParagraphDirection
		switch inputParaLevel {
		case 0:
			base = DirectionLeftToRight
		case 1:
			base = DirectionRightToLeft
		default:
			base = DirectionDefault
		}

		r := NewResolver(text, base)
		rows++

		if got := r.ParagraphLevel(); got != wantParaLevel {
			levelFails++
			t.Errorf("line %d %q: paragraph level = %d, want %d",
				lineNo, text, got, wantParaLevel)
			continue
		}

		// Compare per-rune levels, tracking positions removed by X9.
		removed := make(map[int]bool)
		levelOK := true
		for i, s := range wantLevels {
			if s == "x" {
				removed[i] = true
				continue
			}
			want, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("line %d: bad level %q", lineNo, s)
			}
			if got := r.LevelAt(i); got != want {
				levelOK = false
				levelFails++
				t.Errorf("line %d %q: rune %d level = %d, want %d",
					lineNo, text, i, got, want)
				break
			}
		}
		if !levelOK {
			continue
		}

		// Compare visual order, excluding X9-removed positions.
		reorder := r.LineReorder(0, len(runes))
		gotOrder := make([]string, 0, len(reorder))
		for _, idx := range reorder {
			if removed[idx] {
				continue
			}
			gotOrder = append(gotOrder, strconv.Itoa(idx))
		}
		if len(gotOrder) != len(wantOrder) {
			orderFails++
			t.Errorf("line %d %q: order len = %v, want %v",
				lineNo, text, gotOrder, wantOrder)
			continue
		}
		for i := range wantOrder {
			if gotOrder[i] != wantOrder[i] {
				orderFails++
				t.Errorf("line %d %q: visual order = %v, want %v",
					lineNo, text, gotOrder, wantOrder)
				break
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	t.Logf("UAX #9 conformance: %d rows, %d level failures, %d order failures",
		rows, levelFails, orderFails)
}
