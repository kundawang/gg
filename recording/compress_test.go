package recording

import (
	"math"
	"testing"
)

// assertStatsInvariants verifies the documented CompressionStats identities.
func assertStatsInvariants(t *testing.T, s CompressionStats) {
	t.Helper()
	if got := s.KeptBarrier + s.KeptLastWrite + s.KeptUnknown; got != s.CompressedCommands {
		t.Errorf("keep breakdown %d+%d+%d = %d, want CompressedCommands %d",
			s.KeptBarrier, s.KeptLastWrite, s.KeptUnknown, got, s.CompressedCommands)
	}
	if got := s.RedundantCommands + s.SupersededCommands; got != s.RemovedCommands {
		t.Errorf("remove breakdown %d+%d = %d, want RemovedCommands %d",
			s.RedundantCommands, s.SupersededCommands, got, s.RemovedCommands)
	}
	if got := s.CompressedCommands + s.RemovedCommands; got != s.OriginalCommands {
		t.Errorf("%d+%d = %d, want OriginalCommands %d",
			s.CompressedCommands, s.RemovedCommands, got, s.OriginalCommands)
	}
}

func types(cmds []Command) []CommandType {
	out := make([]CommandType, len(cmds))
	for i, c := range cmds {
		out[i] = c.Type()
	}
	return out
}

func TestCompressNilAndDisabledAreStable(t *testing.T) {
	if got, stats := Compress(nil, CompressOptions{Enabled: true}); got != nil {
		t.Errorf("Compress(nil, enabled) = %v, want nil", got)
	} else if stats != (CompressionStats{}) {
		t.Errorf("Compress(nil) stats = %+v, want zero", stats)
	}

	rec := NewRecorder(20, 20).FinishRecording()
	if got, stats := Compress(rec, CompressOptions{Enabled: false}); got != rec {
		t.Error("disabled compression must return the same recording pointer")
	} else if stats != (CompressionStats{}) {
		t.Errorf("disabled stats = %+v, want zero", stats)
	}
}

func TestCompressEmptyRecording(t *testing.T) {
	rec := NewRecorder(30, 30).FinishRecording()
	out, stats := Compress(rec, CompressOptions{Enabled: true})
	if len(out.Commands()) != 0 {
		t.Fatalf("empty recording compressed to %d commands", len(out.Commands()))
	}
	if stats.OriginalCommands != 0 || stats.CompressedCommands != 0 || stats.RemovedCommands != 0 {
		t.Errorf("unexpected stats for empty recording: %+v", stats)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressSingleCommandStaysIdentical(t *testing.T) {
	rec := NewRecorder(40, 40)
	rec.SetLineWidth(3)
	original := rec.FinishRecording()

	out, stats := Compress(original, CompressOptions{Enabled: true})
	if len(out.Commands()) != 1 {
		t.Fatalf("single command compressed to %d", len(out.Commands()))
	}
	if types(out.Commands())[0] != CmdSetLineWidth {
		t.Fatalf("survivor type = %v, want SetLineWidth", types(out.Commands())[0])
	}
	if out.Commands()[0] != original.Commands()[0] {
		t.Fatal("single command identity not preserved")
	}
	if stats.KeptLastWrite != 1 || stats.RemovedCommands != 0 {
		t.Errorf("stats = %+v", stats)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressRepeatedTransformsKeepsLastWrite(t *testing.T) {
	rec := NewRecorder(60, 60)
	rec.Translate(1, 0)
	rec.Translate(1, 0)
	rec.Translate(1, 0)
	rec.Translate(1, 0)
	original := rec.FinishRecording()
	if len(original.Commands()) != 4 {
		t.Fatalf("setup produced %d commands, want 4", len(original.Commands()))
	}

	out, stats := Compress(original, CompressOptions{Enabled: true})
	if len(out.Commands()) != 1 {
		t.Fatalf("got %d commands, want 1", len(out.Commands()))
	}
	want := original.Commands()[3].(SetTransformCommand).Matrix
	got := out.Commands()[0].(SetTransformCommand).Matrix
	if got != want {
		t.Fatalf("surviving matrix = %#v, want %#v", got, want)
	}
	if stats.SupersededCommands != 3 || stats.RedundantCommands != 0 {
		t.Errorf("stats = %+v, want 3 superseded, 0 redundant", stats)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressMixedStateRunFoldsEverySlot(t *testing.T) {
	rec := NewRecorder(80, 80)
	// Two full state bursts with no drawing between them: every first-burst
	// write must be superseded, and the second burst must survive in order.
	rec.SetRGB(1, 0, 0)
	rec.SetLineWidth(2)
	rec.SetRGB(0, 1, 0)
	rec.SetLineWidth(5)
	rec.SetDash(3, 1)
	rec.SetDash(6, 2)
	rec.Identity()
	original := rec.FinishRecording()

	out, stats := Compress(original, CompressOptions{Enabled: true})
	wantTypes := []CommandType{
		CmdSetFillStyle, CmdSetStrokeStyle, CmdSetLineWidth,
		CmdSetDash, CmdSetTransform,
	}
	if got := types(out.Commands()); !equalTypes(got, wantTypes) {
		t.Fatalf("survivor types = %v, want %v", got, wantTypes)
	}
	// Last-write semantics per slot.
	if out.Commands()[2].(SetLineWidthCommand).Width != 5 {
		t.Error("line width survivor is not the last write")
	}
	if out.Commands()[3].(SetDashCommand).Pattern[0] != 6 {
		t.Error("dash survivor is not the last write")
	}
	// SetColor emits fill+stroke with the same brush ref; both survive once.
	fillRef := out.Commands()[0].(SetFillStyleCommand).Brush
	strokeRef := out.Commands()[1].(SetStrokeStyleCommand).Brush
	if fillRef != strokeRef {
		t.Errorf("fill/stroke refs diverged: %d vs %d", fillRef, strokeRef)
	}
	// SetRGB emits two style writes per call: red burst + green burst and
	// two dash/identity writes give four superseded commands total.
	if stats.SupersededCommands != 4 || stats.RedundantCommands != 0 {
		t.Errorf("stats = %+v, want 4 superseded, 0 redundant", stats)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressDoesNotCrossSaveRestore(t *testing.T) {
	rec := NewRecorder(60, 60)
	rec.Translate(1, 0)
	rec.Save()
	rec.Translate(2, 0)
	rec.Restore()
	rec.Translate(3, 0)
	original := rec.FinishRecording()

	out, stats := Compress(original, CompressOptions{Enabled: true})
	wantTypes := []CommandType{
		CmdSetTransform, CmdSave, CmdSetTransform, CmdRestore, CmdSetTransform,
	}
	if got := types(out.Commands()); !equalTypes(got, wantTypes) {
		t.Fatalf("types = %v, want %v", got, wantTypes)
	}
	if stats.KeptBarrier != 2 || stats.RemovedCommands != 0 {
		t.Errorf("stats = %+v, want 2 barriers and no removals", stats)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressDoesNotCrossDrawingCommands(t *testing.T) {
	rec := NewRecorder(60, 60)
	rec.SetLineWidth(1)
	rec.SetLineWidth(2)
	rec.DrawCircle(10, 10, 5)
	rec.Fill()
	rec.SetLineWidth(3)
	rec.SetLineWidth(4)
	original := rec.FinishRecording()

	out, stats := Compress(original, CompressOptions{Enabled: true})
	// Two folded state runs, one drawing barrier.
	if got, want := len(out.Commands()), 3; got != want {
		t.Fatalf("len = %d, want %d; types %v", got, want, types(out.Commands()))
	}
	widths := []float64{
		out.Commands()[0].(SetLineWidthCommand).Width,
		out.Commands()[2].(SetLineWidthCommand).Width,
	}
	if widths[0] != 2 || widths[1] != 4 {
		t.Errorf("surviving widths = %v, want [2 4]", widths)
	}
	if stats.KeptBarrier != 1 || stats.SupersededCommands != 2 {
		t.Errorf("stats = %+v", stats)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressAdjacentClearClipMerges(t *testing.T) {
	rec := NewRecorder(40, 40)
	rec.ResetClip()
	rec.ResetClip()
	original := rec.FinishRecording()

	out, stats := Compress(original, CompressOptions{Enabled: true})
	if len(out.Commands()) != 1 {
		t.Fatalf("clear clip count = %d, want 1", len(out.Commands()))
	}
	if _, ok := out.Commands()[0].(ClearClipCommand); !ok {
		t.Fatalf("survivor = %T, want ClearClipCommand", out.Commands()[0])
	}
	if stats.RedundantCommands != 1 {
		t.Errorf("stats = %+v", stats)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressClipSetCommandsAreBarriers(t *testing.T) {
	rec := NewRecorder(60, 60)
	rec.ResetClip()
	rec.DrawRectangle(0, 0, 10, 10)
	rec.Clip()
	rec.ResetClip()
	original := rec.FinishRecording()
	// ClearClip | SetClip | ClearClip: the SetClip must split the run and
	// every command must survive.
	out, stats := Compress(original, CompressOptions{Enabled: true})
	if len(out.Commands()) != len(original.Commands()) {
		t.Fatalf("len = %d, want unchanged %d; types %v",
			len(out.Commands()), len(original.Commands()), types(out.Commands()))
	}
	if stats.RemovedCommands != 0 || stats.KeptBarrier != 1 || stats.KeptLastWrite != 2 {
		t.Errorf("stats = %+v", stats)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressPreservesResourceReferences(t *testing.T) {
	rec := NewRecorder(80, 80)
	rec.SetFillRGB(1, 0, 0)
	rec.SetFillRGB(0, 1, 0)
	rec.FillRectangle(1, 1, 10, 10)
	original := rec.FinishRecording()

	out, _ := Compress(original, CompressOptions{Enabled: true})
	if out.Resources() != original.Resources() {
		t.Fatal("resource pool must be shared, not rebuilt")
	}
	if out.Resources().BrushCount() != original.Resources().BrushCount() {
		t.Fatal("resource pool contents changed")
	}

	var fill SetFillStyleCommand
	var rect FillRectCommand
	for _, cmd := range out.Commands() {
		switch c := cmd.(type) {
		case SetFillStyleCommand:
			fill = c
		case FillRectCommand:
			rect = c
		}
	}
	// The surviving fill command and the draw reference distinct pool
	// entries; what matters is that both still resolve in the shared pool.
	if original.Resources().GetBrush(fill.Brush) == nil {
		t.Fatal("folded fill command references a missing brush resource")
	}
	if original.Resources().GetBrush(rect.Brush) == nil {
		t.Fatal("drawing command references a missing brush resource")
	}
}

func TestCompressNonFiniteMatrixDoesNotLoopAndStillFolds(t *testing.T) {
	nan := math.NaN()
	rec := NewRecorder(40, 40)
	rec.SetTransform(Matrix{A: nan, E: 1})
	rec.SetTransform(Matrix{A: math.Inf(1), E: 1})
	rec.SetTransform(Translate(2, 3))
	original := rec.FinishRecording()

	out, stats := Compress(original, CompressOptions{Enabled: true})
	if len(out.Commands()) != 1 {
		t.Fatalf("len = %d, want 1", len(out.Commands()))
	}
	if got := out.Commands()[0].(SetTransformCommand).Matrix; got != Translate(2, 3) {
		t.Fatalf("survivor = %#v", got)
	}
	if stats.NonFiniteMatrices != 2 {
		t.Errorf("NonFiniteMatrices = %d, want 2", stats.NonFiniteMatrices)
	}
	// NaN != NaN makes the first replacement "superseded", never "redundant".
	if stats.SupersededCommands != 2 {
		t.Errorf("stats = %+v", stats)
	}
	assertStatsInvariants(t, stats)
}

type syntheticCommand struct{ kind CommandType }

func (s syntheticCommand) Type() CommandType { return s.kind }

func TestCompressUnknownAndNilCommandsAreRetained(t *testing.T) {
	rec := NewRecorder(40, 40)
	rec.SetLineWidth(1)
	rec.SetLineWidth(2)
	original := rec.FinishRecording()
	// Inject unknown-typed and nil commands directly into the raw recording.
	original.commands = append(original.commands,
		syntheticCommand{kind: CommandType(255)},
		nil,
	)

	out, stats := Compress(original, CompressOptions{Enabled: true})
	if len(out.Commands()) != 3 { // folded width + unknown + nil
		t.Fatalf("len = %d, want 3; types %v", len(out.Commands()), types(out.Commands()))
	}
	if _, ok := out.Commands()[1].(syntheticCommand); !ok {
		t.Fatalf("unknown command dropped or reordered: %T", out.Commands()[1])
	}
	if out.Commands()[2] != nil {
		t.Fatal("nil command must be retained")
	}
	if stats.KeptUnknown != 2 {
		t.Errorf("KeptUnknown = %d, want 2", stats.KeptUnknown)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressDeepStackAndUnbalancedRestore(t *testing.T) {
	rec := NewRecorder(40, 40)
	const depth = 500
	for range depth {
		rec.Save()
	}
	for range depth {
		rec.Restore()
	}
	rec.Restore() // unmatched: recorder itself drops this one, feed it raw.
	original := rec.FinishRecording()
	original.commands = append(original.commands, RestoreCommand{})

	out, stats := Compress(original, CompressOptions{Enabled: true})
	if len(out.Commands()) != len(original.commands) {
		t.Fatalf("structural commands must all survive: %d vs %d",
			len(out.Commands()), len(original.commands))
	}
	if stats.MaxStackDepth != depth {
		t.Errorf("MaxStackDepth = %d, want %d", stats.MaxStackDepth, depth)
	}
	if stats.UnbalancedRestore != 1 {
		t.Errorf("UnbalancedRestore = %d, want 1", stats.UnbalancedRestore)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressLongSequenceIsLinearAndStable(t *testing.T) {
	build := func() *Recording {
		rec := NewRecorder(200, 200)
		for i := range 10000 {
			rec.SetLineWidth(float64(i))
			rec.SetLineCap(LineCap(i % 3))
			if i%97 == 0 {
				rec.DrawCircle(float64(i), 0, 2)
				rec.Fill()
			}
		}
		return rec.FinishRecording()
	}

	first := build()
	out, stats := Compress(first, CompressOptions{Enabled: true})
	assertStatsInvariants(t, stats)
	if stats.RemovedCommands <= 0 {
		t.Fatalf("expected removals in long sequence, stats %+v", stats)
	}
	// Idempotence: compressing again changes nothing.
	again, againStats := Compress(out, CompressOptions{Enabled: true})
	if len(again.Commands()) != len(out.Commands()) {
		t.Fatalf("second pass changed length: %d vs %d",
			len(again.Commands()), len(out.Commands()))
	}
	if againStats.RemovedCommands != 0 || againStats.OriginalCommands != len(out.Commands()) {
		t.Fatalf("second pass should be a no-op, got %+v", againStats)
	}
	// Determinism: a fresh build compresses to the identical type stream.
	second := build()
	out2, _ := Compress(second, CompressOptions{Enabled: true})
	if !equalTypes(types(out.Commands()), types(out2.Commands())) {
		t.Fatal("compression is not deterministic across builds")
	}
}

func TestCompressSetDashRepeatedAndDifferent(t *testing.T) {
	rec := NewRecorder(40, 40)
	rec.SetDash(1, 2)
	rec.SetDash(1, 2) // identical payload -> redundant
	rec.SetDash(3, 4) // different -> supersedes
	original := rec.FinishRecording()

	out, stats := Compress(original, CompressOptions{Enabled: true})
	if len(out.Commands()) != 1 {
		t.Fatalf("len = %d, want 1", len(out.Commands()))
	}
	got := out.Commands()[0].(SetDashCommand)
	if len(got.Pattern) != 2 || got.Pattern[0] != 3 || got.Pattern[1] != 4 {
		t.Fatalf("dash survivor = %#v", got)
	}
	if stats.RedundantCommands != 1 || stats.SupersededCommands != 1 {
		t.Errorf("stats = %+v", stats)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressClipRoundRectIsBarrier(t *testing.T) {
	rec := NewRecorder(40, 40)
	rec.ResetClip()
	rec.ClipRoundRect(0, 0, 10, 10, 2)
	rec.ResetClip()
	original := rec.FinishRecording()

	out, stats := Compress(original, CompressOptions{Enabled: true})
	if len(out.Commands()) != 3 {
		t.Fatalf("len = %d, want 3; types %v", len(out.Commands()), types(out.Commands()))
	}
	if stats.KeptBarrier != 1 || stats.RemovedCommands != 0 {
		t.Errorf("stats = %+v", stats)
	}
	assertStatsInvariants(t, stats)
}

func TestCompressPreservesStrokeRectStateWrapper(t *testing.T) {
	rec := NewRecorder(100, 100)
	rec.SetStrokeRGB(0, 0, 1)
	rec.SetLineWidth(2)
	rec.SetLineWidth(3) // folded away before any barrier
	rec.SetTransform(Matrix{A: 2, E: 2})
	rec.StrokeRectangle(10, 10, 20, 30) // emits Save, SetTransform(identity), Stroke, Restore
	rec.SetLineWidth(4)

	out, stats := Compress(rec.FinishRecording(), CompressOptions{Enabled: true})
	var saveAt, identityAt, strokeAt, restoreAt = -1, -1, -1, -1
	for i, cmd := range out.Commands() {
		switch c := cmd.(type) {
		case SaveCommand:
			saveAt = i
		case RestoreCommand:
			restoreAt = i
		case StrokePathCommand:
			strokeAt = i
		case SetTransformCommand:
			if identityAt == -1 && c.Matrix.IsIdentity() {
				identityAt = i
			}
		}
	}
	if saveAt < 0 || identityAt != saveAt+1 || strokeAt != identityAt+1 || restoreAt != strokeAt+1 {
		t.Fatalf("stroke wrapper broken: save=%d identity=%d stroke=%d restore=%d, types=%v",
			saveAt, identityAt, strokeAt, restoreAt, types(out.Commands()))
	}
	// The outer non-identity transform and the folded width produce exactly
	// one removal; no wrapper command is touched.
	if stats.RemovedCommands != 1 || stats.KeptBarrier != 3 {
		t.Errorf("stats = %+v, want 1 removal, 3 barriers", stats)
	}
	assertStatsInvariants(t, stats)
}

func TestRecorderSetCompressionEnabled(t *testing.T) {
	build := func(enabled bool) *Recording {
		rec := NewRecorder(40, 40)
		rec.SetCompressionEnabled(enabled)
		rec.Translate(1, 0)
		rec.Translate(2, 0)
		rec.Translate(3, 0)
		return rec.FinishRecording()
	}

	off := build(false)
	if len(off.Commands()) != 3 {
		t.Fatalf("default compression off, got %d commands", len(off.Commands()))
	}
	on := build(true)
	if len(on.Commands()) != 1 {
		t.Fatalf("compression on, got %d commands, want 1", len(on.Commands()))
	}
	if on.Resources().BrushCount() != off.Resources().BrushCount() {
		t.Error("compressed recording lost or rebuilt the resource pool")
	}
}

func equalTypes(a, b []CommandType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
