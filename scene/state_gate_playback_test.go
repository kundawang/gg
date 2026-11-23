package scene_test

// Recording-surface gate tests.
//
// recording.Playback drives export backends with Save/Restore/SetClip/
// ClearClip. These tests play a recording into gateRecordingBackend, which
// forwards the state calls to a gateRecorder, so the gate audits the same
// contract raster/PDF/SVG backends implement. Fault doubles (dropRestore,
// duplicateClear, failAfterDraws) verify the gate fails for the exact failure
// modes from the incident report.

import (
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/recording"
	"github.com/gogpu/gg/scene"
)

// gatePlayRecording records fn and plays the result into a gate backend,
// returning the frame report.
func gatePlayRecording(t *testing.T, label string, seed int64, fn func(rec *recording.Recorder), configure func(*gateRecordingBackend)) gateReport {
	t.Helper()
	rec := recording.NewRecorder(100, 100)
	fn(rec)
	recordingData := rec.FinishRecording()

	g := newGateRecorder(label, seed)
	backend := newGateRecordingBackend(g)
	if configure != nil {
		configure(backend)
	}

	g.BeginFrame(label)
	if err := recordingData.Playback(backend); err != nil {
		g.BackendError("playback: " + err.Error())
	}
	return g.EndFrame()
}

// drawWithinClip records a clip plus one fill.
func drawWithinClip(rec *recording.Recorder) {
	rec.Save()
	rec.DrawRectangle(10, 10, 30, 30)
	rec.Clip()
	rec.SetRGB(0, 1, 0)
	rec.FillRectangle(0, 0, 100, 100)
	rec.Restore()
}

func TestGateRecordingBalancedPlayback(t *testing.T) {
	report := gatePlayRecording(t, "rec-balanced", 11, func(rec *recording.Recorder) {
		rec.SetRGB(1, 0, 0)
		rec.FillRectangle(0, 0, 10, 10)
		drawWithinClip(rec)
		rec.SetRGB(0, 0, 1)
		rec.FillRectangle(0, 0, 5, 5)
	}, nil)

	if !report.ok() {
		t.Fatalf("balanced recording rejected by gate:\n%s", report)
	}
	if report.SaveDepth != 0 || report.ClipDepth != 0 {
		t.Fatalf("balanced recording left state open: %s", report.summary())
	}
}

func TestGateRecordingNestedSavesAreLegal(t *testing.T) {
	report := gatePlayRecording(t, "rec-nested", 12, func(rec *recording.Recorder) {
		rec.Save()
		rec.Translate(5, 5)
		rec.Save()
		rec.Translate(5, 5)
		rec.FillRectangle(0, 0, 5, 5)
		rec.Restore()
		rec.Restore()
	}, nil)

	if !report.ok() {
		t.Fatalf("nested recording rejected by gate:\n%s", report)
	}
	if report.MaxDepth < 2 {
		t.Fatalf("nested saves should be detected as legal nesting: %s", report.summary())
	}
}

// --- dropped Restore -------------------------------------------------------

func TestGateRecordingFailsOnDroppedRestore(t *testing.T) {
	report := gatePlayRecording(t, "rec-dropped-restore", 13, func(rec *recording.Recorder) {
		rec.Save()
		rec.Translate(10, 10)
		rec.FillRectangle(0, 0, 10, 10)
		rec.Restore()
	}, func(b *gateRecordingBackend) {
		b.dropRestore = true
	})

	if report.ok() {
		t.Fatalf("gate passed a recording whose Restore was dropped:\n%s", report)
	}
	requireCategory(t, report, gateUnpairedPush)
	requireDiagnostic(t, report)
	if report.SaveDepth == 0 {
		t.Fatalf("dropped Restore should leave saved state open: %s", report.summary())
	}
}

// --- duplicate ClearClip ---------------------------------------------------

func TestGateRecordingFailsOnDuplicateClearClip(t *testing.T) {
	report := gatePlayRecording(t, "rec-duplicate-clear", 14, func(rec *recording.Recorder) {
		rec.DrawRectangle(10, 10, 30, 30)
		rec.Clip()
		rec.ResetClip()
		rec.FillRectangle(0, 0, 10, 10)
	}, func(b *gateRecordingBackend) {
		b.duplicateClear = true
	})

	if report.ok() {
		t.Fatalf("gate passed duplicate ClearClip:\n%s", report)
	}
	requireCategory(t, report, gateDuplicateClear)
	requireDiagnostic(t, report)
}

func TestGateRecordingEmptyStackClearClip(t *testing.T) {
	// A single ResetClip with no clip installed is an empty-stack error, not a
	// duplicate; the gate must tell the two apart.
	report := gatePlayRecording(t, "rec-empty-clear", 15, func(rec *recording.Recorder) {
		rec.ResetClip()
		rec.FillRectangle(0, 0, 10, 10)
	}, nil)

	if report.ok() {
		t.Fatalf("gate passed ClearClip with no active clip:\n%s", report)
	}
	requireCategory(t, report, gateEmptyStack)
	requireDiagnostic(t, report)
}

// --- backend failure -------------------------------------------------------

func TestGateRecordingBackendFailure(t *testing.T) {
	report := gatePlayRecording(t, "rec-backend-fail", 16, func(rec *recording.Recorder) {
		rec.Save()
		rec.DrawRectangle(10, 10, 30, 30)
		rec.Clip()
		rec.FillRectangle(0, 0, 10, 10)
		rec.FillRectangle(0, 0, 10, 10)
		rec.Restore()
	}, func(b *gateRecordingBackend) {
		b.failAfterDraws = 1
	})

	requireCategory(t, report, gateBackendError)
	if report.ok() {
		t.Fatalf("backend failure must fail the gate:\n%s", report)
	}
	if len(report.BackendErrors) == 0 {
		t.Fatalf("backend failure must be recorded: %s", report.summary())
	}
	// Backend failure is reported independently of scene state; the recording
	// itself was balanced, so no state leak should be fabricated.
	if report.SaveDepth != 0 || report.ClipDepth != 0 {
		t.Fatalf("backend failure should not fabricate state leaks: %s", report.summary())
	}
}

// --- cross-layer / cross-frame diagnostics from scene encodings -----------

func TestGateCrossLayerClipLeak(t *testing.T) {
	s := scene.NewScene()
	s.PushLayer(scene.BlendNormal, 1, nil)
	s.PushClip(scene.NewRectShape(0, 0, 40, 40))
	s.Fill(scene.FillNonZero, scene.IdentityAffine(), gateBrush(), gateRect(0, 0, 10, 10))
	// Deliberately PopLayer without PopClip: the clip is still open when the
	// layer ends.
	s.PopLayer()

	g := newGateRecorder("cross-layer", 17)
	report := g.ReplayScene("cross-layer", s)
	if report.ok() {
		t.Fatalf("gate passed a clip left open across PopLayer:\n%s", report)
	}
	requireCategory(t, report, gateCrossLayerLeak)
	requireDiagnostic(t, report)
}

func TestGateCrossFrameLeak(t *testing.T) {
	s := scene.NewScene()
	s.PushClip(scene.NewRectShape(0, 0, 40, 40))
	s.Fill(scene.FillNonZero, scene.IdentityAffine(), gateBrush(), gateRect(0, 0, 10, 10))
	// No PopClip: frame one leaks a clip.

	g := newGateRecorder("cross-frame", 18)
	first := g.ReplayScene("frame-1", s)
	if first.ok() {
		t.Fatalf("frame with an unclosed clip should fail:\n%s", first)
	}
	requireCategory(t, first, gateUnpairedPush)

	clean := scene.NewScene()
	clean.Fill(scene.FillNonZero, scene.IdentityAffine(), gateBrush(), gateRect(0, 0, 10, 10))
	second := g.ReplayScene("frame-2", clean)
	if second.ok() {
		t.Fatalf("clean frame after a leak must still report carried state:\n%s", second)
	}
	requireCategory(t, second, gateCrossFrameLeak)
	if second.Carried == "" {
		t.Fatalf("second frame should report carried state: %s", second.summary())
	}
}

// --- no side effects on production rendering -------------------------------

// TestGateDoesNotAffectRendering renders the same scene before and after
// auditing it through the gate and requires byte-identical output. The gate is
// an observer; if it ever mutates the scene or its encoding this fails.
func TestGateDoesNotAffectRendering(t *testing.T) {
	s := scene.NewScene()
	s.PushClip(scene.NewRectShape(4, 4, 40, 40))
	s.PushLayer(scene.BlendNormal, 0.5, nil)
	s.Fill(scene.FillNonZero, scene.IdentityAffine(), gateBrush(), gateRect(0, 0, 48, 48))
	s.PopLayer()
	s.PopClip()
	s.Fill(scene.FillNonZero, scene.TranslateAffine(20, 20), gateBrush(), gateRect(0, 0, 8, 8))

	gg.CloseAccelerator()
	before := gateRenderPixmap(t, s)

	g := newGateRecorder("observer", 19)
	report := g.ReplayScene("observer", s)
	if !report.ok() {
		t.Fatalf("scene should be balanced: \n%s", report)
	}

	after := gateRenderPixmap(t, s)
	if len(before.Data()) != len(after.Data()) {
		t.Fatalf("pixmap size changed: %d vs %d", len(before.Data()), len(after.Data()))
	}
	for i := range before.Data() {
		if before.Data()[i] != after.Data()[i] {
			t.Fatalf("gate altered rendering output at byte %d: %d vs %d",
				i, before.Data()[i], after.Data()[i])
		}
	}
}

// gateRenderPixmap renders a scene with the real CPU renderer.
func gateRenderPixmap(t *testing.T, s *scene.Scene) *gg.Pixmap {
	t.Helper()
	r := scene.NewRenderer(48, 48)
	if r == nil {
		t.Fatal("NewRenderer returned nil")
	}
	defer r.Close()
	target := gg.NewPixmap(48, 48)
	if err := r.Render(target, s); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return target
}

// --- transform contamination ---------------------------------------------

// TestGateDetectsStaleTransformAfterText is a regression for a real bug the
// gate surfaced: Scene.encodeTextRun emits TagTransform directly but never
// updates lastEncodedTransform. When text is drawn under a PushTransform and
// the transform is then popped, a following Identity draw is delta-skipped, so
// the renderer keeps the text transform and paints that draw in the wrong
// place. The gate must observe the stale transform at frame end.
func TestGateDetectsStaleTransformAfterText(t *testing.T) {
	face, ok := gateTextFace(t, 20)
	if !ok {
		t.Skip("bundled test font unavailable")
	}

	s := scene.NewScene()
	s.PushTransform(scene.TranslateAffine(50, 50))
	if err := s.DrawText("A", face, 0, 0, scene.SolidBrush(gg.RGBA{R: 1, A: 1})); err != nil {
		t.Fatalf("DrawText: %v", err)
	}
	s.PopTransform()
	s.Fill(scene.FillNonZero, scene.IdentityAffine(), gateBrush(), gateRect(0, 0, 10, 10))

	g := newGateRecorder("stale-transform", 21)
	report := g.ReplayScene("stale-transform", s)

	// The encoding is structurally balanced (no open clip/layer), so the gate
	// must not misreport it as a push imbalance...
	if report.Category == gateUnpairedPush || report.Category == gateEmptyStack {
		t.Fatalf("stale transform misclassified as a stack imbalance:\n%s", report)
	}
	// ...but the surviving transform must be visible at frame end.
	if report.Transform.IsIdentity() {
		t.Fatalf("gate lost the stale transform (transform=%s); it cannot detect pollution\n%s",
			formatGateAffine(report.Transform), report)
	}
	if report.Transform.C != 50 || report.Transform.F != 50 {
		t.Fatalf("stale transform = %s, want translate(50,50)", formatGateAffine(report.Transform))
	}
	t.Logf("gate observed stale transform at frame end (known bug): %s", report.summary())
}

// TestGateRenderSubstitution verifies the gate is a drop-in for the real
// renderer's Render call shape, so existing rendering tests can swap in the
// gate without touching how they build scenes.
func TestGateRenderSubstitution(t *testing.T) {
	s := scene.NewScene()
	s.PushClip(scene.NewRectShape(0, 0, 20, 20))
	s.Fill(scene.FillNonZero, scene.IdentityAffine(), gateBrush(), gateRect(0, 0, 10, 10))
	s.PopClip()

	g := newGateRecorder("substitute", 22)
	target := gg.NewPixmap(32, 32)
	if err := g.Render(target, s); err != nil {
		t.Fatalf("Render: %v", err)
	}
	r := g.LastReport()
	if !r.ok() {
		t.Fatalf("substituted Render produced an unbalanced report:\n%s", r)
	}
	// The gate must not have drawn into the target.
	for _, b := range target.Data() {
		if b != 0 {
			t.Fatal("gate Render wrote into the render target; it must be an observer")
		}
	}
	if len(g.Reports()) != 1 {
		t.Fatalf("Reports() = %d, want 1", len(g.Reports()))
	}
}
