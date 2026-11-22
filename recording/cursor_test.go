package recording

import (
	"errors"
	"image"
	"reflect"
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
)

// countBackend records every backend call so tests can compare full playback
// against stitched range playback at the command level. It tracks transform
// and Save/Restore depth the way a real backend would.
type countBackend struct {
	beginErr error
	endErr   error

	begins, ends    int
	saves, restores int
	depth           int
	transform       Matrix
	transformStack  []Matrix

	clips       int
	clearClips  int
	fills       int
	strokes     int
	fillRects   int
	images      int
	texts       int
	fillXforms  []Matrix // transform observed at each FillRect
	setXformCnt int
}

func (b *countBackend) Begin(_, _ int) error {
	b.begins++
	if b.beginErr != nil {
		return b.beginErr
	}
	b.transform = Identity()
	b.transformStack = b.transformStack[:0]
	b.depth = 0
	return nil
}

func (b *countBackend) End() error {
	b.ends++
	return b.endErr
}

func (b *countBackend) Save() {
	b.saves++
	b.depth++
	b.transformStack = append(b.transformStack, b.transform)
}

func (b *countBackend) Restore() {
	b.restores++
	if b.depth > 0 {
		b.depth--
	}
	if len(b.transformStack) > 0 {
		b.transform = b.transformStack[len(b.transformStack)-1]
		b.transformStack = b.transformStack[:len(b.transformStack)-1]
	}
}

func (b *countBackend) SetTransform(m Matrix) {
	b.setXformCnt++
	b.transform = m
}

func (b *countBackend) SetClip(*gg.Path, FillRule) { b.clips++ }
func (b *countBackend) ClearClip()                 { b.clearClips++ }
func (b *countBackend) FillPath(*gg.Path, Brush, FillRule) {
	b.fills++
}
func (b *countBackend) StrokePath(*gg.Path, Brush, Stroke) {
	b.strokes++
}
func (b *countBackend) FillRect(Rect, Brush) {
	b.fillRects++
	b.fillXforms = append(b.fillXforms, b.transform)
}
func (b *countBackend) DrawImage(image.Image, Rect, Rect, ImageOptions) {
	b.images++
}
func (b *countBackend) DrawText(string, float64, float64, text.Face, Brush) {
	b.texts++
}

// buildCursorTestRecording records a multi-layer scene with nested
// transforms, clipping, an image, and text, segmented by labels.
func buildCursorTestRecording() *Recording {
	rec := NewRecorder(120, 120)

	rec.SetFillRGB(1, 1, 1)
	rec.FillRectangle(0, 0, 120, 120)

	rec.Mark("shapes")
	rec.Save()
	rec.Translate(20, 20)
	rec.Rotate(0.25)
	rec.SetFillRGB(1, 0, 0)
	rec.DrawCircle(30, 30, 15)
	rec.Fill()
	rec.Save()
	rec.Scale(2, 0.5)
	rec.SetStrokeRGB(0, 0, 1)
	rec.SetLineWidth(2)
	rec.DrawRectangle(5, 40, 30, 20)
	rec.Stroke()
	rec.Restore()

	rec.Mark("clipped")
	rec.DrawRectangle(0, 0, 60, 60)
	rec.Clip()
	rec.SetFillRGB(0, 0.7, 0)
	rec.FillRectangle(10, 10, 100, 100)
	rec.ClipRoundRect(50, 50, 40, 30, 6)
	rec.SetFillRGB(0, 0, 0.8)
	rec.FillRectangle(40, 40, 60, 50)
	rec.ResetClip()
	rec.Restore()

	rec.Mark("media")
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	rec.DrawImage(img, 90, 90)
	rec.SetFillRGB(0, 0, 0)
	rec.DrawString("hi", 10, 110)

	return rec.FinishRecording()
}

func labelIndices(t *testing.T, r *Recording) (shapes, clipped, media int) {
	t.Helper()
	var ok bool
	if shapes, ok = r.LabelIndex("shapes"); !ok {
		t.Fatal("missing label shapes")
	}
	if clipped, ok = r.LabelIndex("clipped"); !ok {
		t.Fatal("missing label clipped")
	}
	if media, ok = r.LabelIndex("media"); !ok {
		t.Fatal("missing label media")
	}
	return shapes, clipped, media
}

func TestCursorStitchedPlaybackMatchesFullPlayback(t *testing.T) {
	r := buildCursorTestRecording()

	full := &countBackend{}
	if err := r.Playback(full); err != nil {
		t.Fatalf("Playback: %v", err)
	}

	i1, i2, i3 := labelIndices(t, r)
	n := len(r.Commands())

	stitched := &countBackend{}
	sess := r.NewSession(stitched)
	c0, err := r.Cursor(0, i1)
	if err != nil {
		t.Fatalf("Cursor(0, %d): %v", i1, err)
	}
	c1, err := r.CursorForLabel("shapes")
	if err != nil {
		t.Fatalf("CursorForLabel: %v", err)
	}
	c2, err := r.CursorBetweenLabels("clipped", "media")
	if err != nil {
		t.Fatalf("CursorBetweenLabels: %v", err)
	}
	c3, err := r.Cursor(i3, n)
	if err != nil {
		t.Fatalf("Cursor(%d, %d): %v", i3, n, err)
	}

	// The four ranges tile [0, n) with no overlap or gap.
	bounds := [][2]int{{c0.Start(), c0.End()}, {c1.Start(), c1.End()},
		{c2.Start(), c2.End()}, {c3.Start(), c3.End()}}
	wantBounds := [][2]int{{0, i1}, {i1, i2}, {i2, i3}, {i3, n}}
	if !reflect.DeepEqual(bounds, wantBounds) {
		t.Fatalf("cursor bounds = %v, want %v", bounds, wantBounds)
	}

	for i, c := range []*Cursor{c0, c1, c2, c3} {
		if err := sess.Resume(c); err != nil {
			t.Fatalf("Resume cursor %d: %v", i, err)
		}
		if stitched.depth != 0 {
			t.Fatalf("backend state depth = %d after cursor %d, want 0 (state restored)", stitched.depth, i)
		}
	}
	if err := sess.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if stitched.begins != 1 || stitched.ends != 1 {
		t.Errorf("Begin/End calls = %d/%d, want 1/1", stitched.begins, stitched.ends)
	}

	// Every drawing command executes exactly once across the stitched ranges.
	if stitched.fills != full.fills || stitched.strokes != full.strokes ||
		stitched.fillRects != full.fillRects || stitched.images != full.images ||
		stitched.texts != full.texts {
		t.Errorf("drawing counts stitched (fill %d stroke %d rect %d img %d text %d) "+
			"!= full (fill %d stroke %d rect %d img %d text %d)",
			stitched.fills, stitched.strokes, stitched.fillRects, stitched.images, stitched.texts,
			full.fills, full.strokes, full.fillRects, full.images, full.texts)
	}

	// Save/Restore stays balanced: full playback issues the stream's 2 pairs;
	// stitching adds one isolation pair per cursor (4) plus the pairs replayed
	// from state prefixes (c1: 0, c2: 2, c3: 2), giving 2+4+4 = 10.
	if stitched.saves != 10 || stitched.restores != 10 {
		t.Errorf("Save/Restore = %d/%d, want 10/10 (stream + isolation + prefix replay)",
			stitched.saves, stitched.restores)
	}

	// The transform observed by each FillRect must match full playback exactly:
	// range playback rebuilt the same transform state from the prefix.
	if !reflect.DeepEqual(stitched.fillXforms, full.fillXforms) {
		t.Errorf("FillRect transforms = %v, want %v", stitched.fillXforms, full.fillXforms)
	}
}

func TestCursorRebuildsStatePrefix(t *testing.T) {
	r := buildCursorTestRecording()
	_, _, i3 := labelIndices(t, r)

	// A cursor starting at "media" must rebuild the clip history from the
	// "clipped" segment: SetClip + ClipRoundRect + ClearClip.
	c, err := r.Cursor(i3, len(r.Commands()))
	if err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	b := &countBackend{}
	if err := c.Resume(b); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if b.begins != 1 || b.ends != 1 {
		t.Errorf("Begin/End = %d/%d, want 1/1 (self-contained frame)", b.begins, b.ends)
	}
	if b.clips != 2 || b.clearClips != 1 {
		t.Errorf("prefix clip replay = %d clips, %d clears; want 2 clips, 1 clear",
			b.clips, b.clearClips)
	}
	if b.images != 1 || b.texts != 1 {
		t.Errorf("range drawing = %d images, %d texts; want 1 each", b.images, b.texts)
	}
	if b.fills != 0 || b.fillRects != 0 || b.strokes != 0 {
		t.Errorf("range drew %d fills, %d rects, %d strokes outside its interval",
			b.fills, b.fillRects, b.strokes)
	}
	if b.depth != 0 {
		t.Errorf("backend state depth = %d after Resume, want 0", b.depth)
	}
}

func TestCursorNestedSaveRestore(t *testing.T) {
	rec := NewRecorder(50, 50)
	rec.Save()                        // 0
	rec.Translate(5, 5)               // 1
	rec.Save()                        // 2
	rec.SetFillRGB(1, 0, 0)           // 3
	rec.FillRectangle(0, 0, 10, 10)   // 4
	rec.Restore()                     // 5
	rec.SetFillRGB(0, 1, 0)           // 6
	rec.FillRectangle(20, 20, 10, 10) // 7
	rec.Restore()                     // 8
	r := rec.FinishRecording()

	// Range opens with a Restore whose matching Save lives in the prefix.
	c, err := r.Cursor(4, 6)
	if err != nil {
		t.Fatalf("Cursor(4, 6): %v", err)
	}
	b := &countBackend{}
	if err := c.Resume(b); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if b.fillRects != 1 {
		t.Errorf("FillRect calls = %d, want 1", b.fillRects)
	}
	if b.depth != 0 {
		t.Errorf("backend state depth = %d, want 0", b.depth)
	}
	if b.saves != b.restores {
		t.Errorf("Save/Restore = %d/%d, want balanced", b.saves, b.restores)
	}
	// The prefix transform (Translate(5,5)) is visible to the range's FillRect.
	if want := (Matrix{A: 1, C: 5, E: 1, F: 5}); len(b.fillXforms) != 1 || b.fillXforms[0] != want {
		t.Errorf("FillRect transform = %v, want [%v]", b.fillXforms, want)
	}

	// Range leaves a Save open; the cursor must unwind it afterwards.
	c2, err := r.Cursor(2, 5)
	if err != nil {
		t.Fatalf("Cursor(2, 5): %v", err)
	}
	b2 := &countBackend{}
	if err := c2.Resume(b2); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if b2.depth != 0 {
		t.Errorf("backend state depth = %d after unbalanced range, want 0", b2.depth)
	}
	if b2.saves != b2.restores {
		t.Errorf("Save/Restore = %d/%d, want balanced after unwind", b2.saves, b2.restores)
	}
}

func TestCursorRangeValidation(t *testing.T) {
	r := buildCursorTestRecording()
	n := len(r.Commands())

	tests := []struct {
		name       string
		start, end int
		want       error
	}{
		{"empty range", 2, 2, ErrEmptyRange},
		{"reversed range", 3, 2, ErrInvalidRange},
		{"negative start", -1, 2, ErrRangeOutOfBounds},
		{"end beyond stream", 0, n + 1, ErrRangeOutOfBounds},
		{"whole stream", 0, n, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := r.Cursor(tt.start, tt.end)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Cursor(%d, %d) error = %v, want %v", tt.start, tt.end, err, tt.want)
			}
		})
	}

	if _, err := r.Snapshot(-1); !errors.Is(err, ErrRangeOutOfBounds) {
		t.Errorf("Snapshot(-1) error = %v, want %v", err, ErrRangeOutOfBounds)
	}
	if _, err := r.Snapshot(n + 1); !errors.Is(err, ErrRangeOutOfBounds) {
		t.Errorf("Snapshot(n+1) error = %v, want %v", err, ErrRangeOutOfBounds)
	}
	snap, err := r.Snapshot(n)
	if err != nil {
		t.Fatalf("Snapshot(%d): %v", n, err)
	}
	if snap.Index() != n {
		t.Errorf("snapshot index = %d, want %d", snap.Index(), n)
	}
	if _, err := snap.Cursor(n); !errors.Is(err, ErrEmptyRange) {
		t.Errorf("snapshot cursor at end error = %v, want %v", err, ErrEmptyRange)
	}
}

func TestCursorLabels(t *testing.T) {
	r := buildCursorTestRecording()
	i1, i2, i3 := labelIndices(t, r)
	n := len(r.Commands())

	c, err := r.CursorForLabel("clipped")
	if err != nil {
		t.Fatalf("CursorForLabel: %v", err)
	}
	if c.Start() != i2 || c.End() != i3 {
		t.Errorf("clipped segment = [%d, %d), want [%d, %d)", c.Start(), c.End(), i2, i3)
	}

	last, err := r.CursorForLabel("media")
	if err != nil {
		t.Fatalf("CursorForLabel(media): %v", err)
	}
	if last.Start() != i3 || last.End() != n {
		t.Errorf("last segment = [%d, %d), want [%d, %d)", last.Start(), last.End(), i3, n)
	}

	between, err := r.CursorBetweenLabels("shapes", "media")
	if err != nil {
		t.Fatalf("CursorBetweenLabels: %v", err)
	}
	if between.Start() != i1 || between.End() != i3 {
		t.Errorf("between = [%d, %d), want [%d, %d)", between.Start(), between.End(), i1, i3)
	}

	if _, err := r.CursorForLabel("nope"); !errors.Is(err, ErrUnknownLabel) {
		t.Errorf("unknown label error = %v, want %v", err, ErrUnknownLabel)
	}
	if _, err := r.CursorBetweenLabels("shapes", "nope"); !errors.Is(err, ErrUnknownLabel) {
		t.Errorf("unknown end label error = %v, want %v", err, ErrUnknownLabel)
	}
	if _, err := r.CursorBetweenLabels("media", "shapes"); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("reversed labels error = %v, want %v", err, ErrInvalidRange)
	}

	// A label with no commands after it yields a marker-only range that
	// plays nothing — a defined, valid result.
	rec := NewRecorder(10, 10)
	rec.Mark("tail")
	rt := rec.FinishRecording()
	tail, err := rt.CursorForLabel("tail")
	if err != nil {
		t.Fatalf("CursorForLabel(tail): %v", err)
	}
	b := &countBackend{}
	if err := tail.Resume(b); err != nil {
		t.Fatalf("Resume tail: %v", err)
	}
	if b.fills != 0 || b.fillRects != 0 || b.strokes != 0 || b.texts != 0 || b.images != 0 {
		t.Errorf("marker-only range drew commands: %+v", b)
	}
}

func TestCursorDuplicateResume(t *testing.T) {
	r := buildCursorTestRecording()
	c, err := r.Cursor(0, 2)
	if err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	if err := c.Resume(&countBackend{}); err != nil {
		t.Fatalf("first Resume: %v", err)
	}
	if err := c.Resume(&countBackend{}); !errors.Is(err, ErrAlreadyResumed) {
		t.Errorf("second Resume error = %v, want %v", err, ErrAlreadyResumed)
	}
	// Also rejected when resumed into a session.
	sess := r.NewSession(&countBackend{})
	if err := sess.Resume(c); !errors.Is(err, ErrAlreadyResumed) {
		t.Errorf("session Resume error = %v, want %v", err, ErrAlreadyResumed)
	}
}

func TestCursorBackendErrors(t *testing.T) {
	r := buildCursorTestRecording()
	boom := errors.New("boom")

	// Begin failure: error is wrapped, the cursor is not consumed, and no
	// half frame is reported as success.
	c, err := r.Cursor(0, 2)
	if err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	if err := c.Resume(&countBackend{beginErr: boom}); !errors.Is(err, boom) {
		t.Fatalf("Begin error = %v, want wrapped %v", err, boom)
	}
	if err := c.Resume(&countBackend{}); err != nil {
		t.Errorf("Resume after failed Begin = %v, want nil (cursor not consumed)", err)
	}

	// End failure: error is wrapped and the cursor is consumed — the frame
	// played but finalization failed, so it must not look like success.
	c2, err := r.Cursor(0, 2)
	if err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	if err := c2.Resume(&countBackend{endErr: boom}); !errors.Is(err, boom) {
		t.Fatalf("End error = %v, want wrapped %v", err, boom)
	}
	if err := c2.Resume(&countBackend{}); !errors.Is(err, ErrAlreadyResumed) {
		t.Errorf("Resume after failed End error = %v, want %v", err, ErrAlreadyResumed)
	}
}

func TestCursorDanglingResources(t *testing.T) {
	brushPool := NewResourcePool()
	brushPool.AddBrush(NewSolidBrush(gg.Black)) // BrushRef(0) valid

	tests := []struct {
		name      string
		commands  []Command
		resources *ResourcePool
		start     int
		end       int
	}{
		{
			name: "brush ref out of pool",
			commands: []Command{
				FillRectCommand{Rect: NewRect(0, 0, 5, 5), Brush: BrushRef(7)},
			},
			resources: brushPool,
			end:       1,
		},
		{
			name: "invalid sentinel ref",
			commands: []Command{
				FillPathCommand{Path: PathRef(InvalidRef), Brush: BrushRef(0)},
			},
			resources: brushPool,
			end:       1,
		},
		{
			name: "nil path in pool",
			commands: []Command{
				FillPathCommand{Path: PathRef(0), Brush: BrushRef(0)},
			},
			resources: func() *ResourcePool {
				p := NewResourcePool()
				p.AddPath(nil)
				p.AddBrush(NewSolidBrush(gg.Black))
				return p
			}(),
			end: 1,
		},
		{
			name: "dangling ref in state prefix",
			commands: []Command{
				SetClipCommand{Path: PathRef(3)}, // no paths in pool
				FillRectCommand{Rect: NewRect(0, 0, 5, 5), Brush: BrushRef(0)},
			},
			resources: brushPool,
			start:     1,
			end:       2,
		},
		{
			name: "dangling image ref",
			commands: []Command{
				DrawImageCommand{Image: ImageRef(0)},
			},
			resources: brushPool,
			end:       1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Recording{
				width:     10,
				height:    10,
				commands:  tt.commands,
				resources: tt.resources,
			}
			if _, err := r.Cursor(tt.start, tt.end); !errors.Is(err, ErrDanglingResource) {
				t.Fatalf("Cursor(%d, %d) error = %v, want %v", tt.start, tt.end, err, ErrDanglingResource)
			}
		})
	}
}

func TestSessionLifecycle(t *testing.T) {
	r := buildCursorTestRecording()

	// Cursor from a different recording is rejected.
	otherRec := NewRecorder(10, 10)
	otherRec.FillRectangle(0, 0, 5, 5)
	other := otherRec.FinishRecording()
	foreign, err := other.Cursor(0, 1)
	if err != nil {
		t.Fatalf("foreign Cursor: %v", err)
	}
	sess := r.NewSession(&countBackend{})
	if err := sess.Resume(foreign); !errors.Is(err, ErrRecordingMismatch) {
		t.Errorf("foreign cursor error = %v, want %v", err, ErrRecordingMismatch)
	}

	// Resume after Finish is rejected; Finish itself is idempotent and a
	// session that never started finishes cleanly.
	c1, err := r.Cursor(0, 2)
	if err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	c2, err := r.Cursor(2, 4)
	if err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	sess2 := r.NewSession(&countBackend{})
	if err := sess2.Resume(c1); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := sess2.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := sess2.Resume(c2); !errors.Is(err, ErrSessionFinished) {
		t.Errorf("Resume after Finish error = %v, want %v", err, ErrSessionFinished)
	}
	if err := sess2.Finish(); err != nil {
		t.Errorf("second Finish = %v, want nil", err)
	}
	if err := r.NewSession(&countBackend{}).Finish(); err != nil {
		t.Errorf("Finish of unstarted session = %v, want nil", err)
	}
}

func TestSnapshotReuse(t *testing.T) {
	r := buildCursorTestRecording()
	_, i2, i3 := labelIndices(t, r)
	n := len(r.Commands())

	snap, err := r.Snapshot(i2)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// One snapshot backs multiple cursors.
	c1, err := snap.Cursor(i3)
	if err != nil {
		t.Fatalf("snapshot Cursor(%d): %v", i3, err)
	}
	c2, err := snap.Cursor(n)
	if err != nil {
		t.Fatalf("snapshot Cursor(%d): %v", n, err)
	}
	if c1.Start() != i2 || c2.Start() != i2 {
		t.Fatalf("cursor starts = %d, %d; want %d", c1.Start(), c2.Start(), i2)
	}

	got := &countBackend{}
	sess := r.NewSession(got)
	if err := sess.Resume(c1); err != nil {
		t.Fatalf("Resume c1: %v", err)
	}
	if err := sess.Resume(c2); err != nil {
		t.Fatalf("Resume c2: %v", err)
	}
	if err := sess.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// Drawing commands in [i2,i3) + [i2,n) == [i2,n) played once plus the
	// shared [i2,i3) prefix played twice — compare against direct cursors.
	want := &countBackend{}
	full, err := r.Cursor(i2, n)
	if err != nil {
		t.Fatalf("Cursor(%d, %d): %v", i2, n, err)
	}
	head, err := r.Cursor(i2, i3)
	if err != nil {
		t.Fatalf("Cursor(%d, %d): %v", i2, i3, err)
	}
	if err := full.Resume(want); err != nil {
		t.Fatalf("Resume full: %v", err)
	}
	if err := head.Resume(want); err != nil {
		t.Fatalf("Resume head: %v", err)
	}

	if got.fillRects != want.fillRects || got.fills != want.fills ||
		got.strokes != want.strokes || got.images != want.images || got.texts != want.texts {
		t.Errorf("snapshot-reused cursors drew (rect %d fill %d stroke %d img %d text %d), "+
			"want (rect %d fill %d stroke %d img %d text %d)",
			got.fillRects, got.fills, got.strokes, got.images, got.texts,
			want.fillRects, want.fills, want.strokes, want.images, want.texts)
	}
}
