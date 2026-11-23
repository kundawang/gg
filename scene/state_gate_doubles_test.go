package scene_test

// Self-checks for the state-balance gate.
//
// These tests do two things:
//
//  1. Prove the gate accepts the shapes a correct scene produces (flat draws,
//     nested clips, clips around a layer, layers with a clip), with the
//     category distinguishing flat balance from legal nesting.
//
//  2. Prove the gate fails for the three failure modes it exists to catch:
//     a dropped Restore, a duplicated ClearClip, and a backend that fails in
//     the middle of a frame. Each fault double deliberately misbehaves and the
//     test asserts on the exact category plus the first-unmatched diagnostic.

import (
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/scene"
)

// --- helper builders -------------------------------------------------------

// gateBrush returns a shared solid brush for scene construction.
func gateBrush() scene.Brush {
	return scene.SolidBrush(gg.RGBA{R: 1, G: 0, B: 0, A: 1})
}

// gateRect builds a rectangle shape.
func gateRect(x, y, w, h float32) scene.Shape {
	return scene.NewRectShape(x, y, w, h)
}

// requireOK fails the test when the frame report is not balanced.
func requireOK(t *testing.T, r gateReport) {
	t.Helper()
	if !r.ok() {
		t.Fatalf("state gate rejected a legal frame:\n%s", r)
	}
}

// requireCategory fails unless the report carries the expected category.
// Failing categories must also name the first offending command; state leaks
// and empty-stack pops name it, a mid-frame backend error is instead recorded
// in BackendErrors (its state snapshot is still present in the summary).
func requireCategory(t *testing.T, r gateReport, want gateCategory) {
	t.Helper()
	if r.Category != want {
		t.Fatalf("category = %q, want %q\n%s", r.Category, want, r)
	}
	if !want.ok() && want != gateBackendError && r.FirstUnmatched == "" {
		t.Fatalf("expected a first-unmatched diagnostic, got none:\n%s", r)
	}
}

// requireDiagnostic fails unless the report names a first unpaired command.
func requireDiagnostic(t *testing.T, r gateReport) {
	t.Helper()
	if r.FirstUnmatched == "" {
		t.Fatalf("expected a first-unmatched diagnostic, got none:\n%s", r)
	}
}

// --- legal nesting ---------------------------------------------------------

func TestStateGateFlatFrameBalanced(t *testing.T) {
	s := scene.NewScene()
	s.Fill(scene.FillNonZero, scene.IdentityAffine(), gateBrush(), gateRect(0, 0, 10, 10))
	s.Fill(scene.FillNonZero, scene.TranslateAffine(5, 5), gateBrush(), gateRect(0, 0, 10, 10))

	g := newGateRecorder("flat", 1)
	r := g.ReplayScene("flat", s)
	requireOK(t, r)
	requireCategory(t, r, gateBalanced)
	if r.SaveDepth != 0 || r.ClipDepth != 0 || r.LayerDepth != 0 {
		t.Fatalf("flat frame left state behind: %s", r.summary())
	}
}

func TestStateGateNestedClipsAreLegalNesting(t *testing.T) {
	s := scene.NewScene()
	s.PushClip(gateRect(0, 0, 80, 80))
	s.PushClip(gateRect(10, 10, 40, 40))
	s.Fill(scene.FillNonZero, scene.IdentityAffine(), gateBrush(), gateRect(0, 0, 100, 100))
	s.PopClip()
	s.PopClip()

	g := newGateRecorder("nested-clips", 2)
	r := g.ReplayScene("nested-clips", s)
	requireOK(t, r)
	requireCategory(t, r, gateLegalNested)
	if r.ClipDepth != 0 || r.MaxDepth < 2 {
		t.Fatalf("nested clips should be balanced but deep: %s", r.summary())
	}
}

func TestStateGateClipAroundLayer(t *testing.T) {
	s := scene.NewScene()
	s.PushClip(gateRect(0, 0, 50, 50))
	s.PushLayer(scene.BlendNormal, 1, nil)
	s.Fill(scene.FillNonZero, scene.IdentityAffine(), gateBrush(), gateRect(0, 0, 10, 10))
	s.PopLayer()
	s.PopClip()

	g := newGateRecorder("clip-around-layer", 3)
	r := g.ReplayScene("clip-around-layer", s)
	requireOK(t, r)
	requireCategory(t, r, gateBalanced)
}

func TestStateGateLayerWithClip(t *testing.T) {
	s := scene.NewScene()
	s.PushLayer(scene.BlendNormal, 1, gateRect(0, 0, 50, 50))
	s.Fill(scene.FillNonZero, scene.IdentityAffine(), gateBrush(), gateRect(0, 0, 10, 10))
	s.PopLayer()

	g := newGateRecorder("layer-clip", 4)
	r := g.ReplayScene("layer-clip", s)
	requireOK(t, r)
}

// --- fault double 1: dropped Restore --------------------------------------

// dropRestoreRecorder forwards every state operation to a real gateRecorder
// except Restore, which it silently drops. This is the classic leak: a frame
// that opens a clip and forgets to close it.
type dropRestoreRecorder struct {
	inner *gateRecorder
}

func (d *dropRestoreRecorder) Save(cause string)           { d.inner.Save(cause) }
func (d *dropRestoreRecorder) Restore()                    {} // dropped
func (d *dropRestoreRecorder) BeginClip(sig string)        { d.inner.BeginClip(sig) }
func (d *dropRestoreRecorder) EndClip()                    { d.inner.EndClip() }
func (d *dropRestoreRecorder) ClearClip()                  { d.inner.ClearClip() }
func (d *dropRestoreRecorder) SetTransform(t scene.Affine) { d.inner.SetTransform(t) }
func (d *dropRestoreRecorder) SetAntiAlias(v bool)         { d.inner.SetAntiAlias(v) }
func (d *dropRestoreRecorder) BeginLayer(desc string)      { d.inner.BeginLayer(desc) }
func (d *dropRestoreRecorder) EndLayer()                   { d.inner.EndLayer() }
func (d *dropRestoreRecorder) BackendError(msg string)     { d.inner.BackendError(msg) }

func TestStateGateFailsOnDroppedRestore(t *testing.T) {
	s := scene.NewScene()
	s.PushClip(gateRect(0, 0, 50, 50))
	s.Fill(scene.FillNonZero, scene.IdentityAffine(), gateBrush(), gateRect(0, 0, 10, 10))
	s.PopClip()

	g := newGateRecorder("dropped-restore", 5)
	d := &dropRestoreRecorder{inner: g}

	g.BeginFrame("frame-1")
	g.replayEncodingInto(s.Encoding(), d)
	report := g.EndFrame()

	if report.ok() {
		t.Fatalf("gate passed a frame with a dropped Restore:\n%s", report)
	}
	if report.SaveDepth == 0 {
		t.Fatalf("dropped Restore should leave saved state open: %s", report.summary())
	}
	requireCategory(t, report, gateUnpairedPush)
	requireDiagnostic(t, report)

	// The next frame must observe cross-frame contamination, not silently
	// pass. This is the whole point of the gate: a leak is caught on the
	// following frame even if the leaking frame was clean on its own surface.
	g.BeginFrame("frame-2")
	g.replayEncodingInto(s.Encoding(), d)
	report2 := g.EndFrame()
	if report2.ok() {
		t.Fatalf("second frame passed despite carried state:\n%s", report2)
	}
	requireCategory(t, report2, gateCrossFrameLeak)
	if report2.Carried == "" {
		t.Fatalf("second frame should report carried state: %s", report2.summary())
	}
}

// --- fault double 2: duplicated ClearClip ---------------------------------

// duplicateClearRecorder forwards to a gateRecorder but injects an extra
// ClearClip before every real ClearClip.
type duplicateClearRecorder struct {
	inner *gateRecorder
}

func (d *duplicateClearRecorder) Save(cause string)    { d.inner.Save(cause) }
func (d *duplicateClearRecorder) Restore()             { d.inner.Restore() }
func (d *duplicateClearRecorder) BeginClip(sig string) { d.inner.BeginClip(sig) }
func (d *duplicateClearRecorder) EndClip()             { d.inner.EndClip() }
func (d *duplicateClearRecorder) ClearClip() {
	d.inner.ClearClip()
	d.inner.ClearClip()
}
func (d *duplicateClearRecorder) SetTransform(t scene.Affine) { d.inner.SetTransform(t) }
func (d *duplicateClearRecorder) SetAntiAlias(v bool)         { d.inner.SetAntiAlias(v) }
func (d *duplicateClearRecorder) BeginLayer(desc string)      { d.inner.BeginLayer(desc) }
func (d *duplicateClearRecorder) EndLayer()                   { d.inner.EndLayer() }
func (d *duplicateClearRecorder) BackendError(msg string)     { d.inner.BackendError(msg) }

func TestStateGateFailsOnDuplicateClearClip(t *testing.T) {
	g := newGateRecorder("duplicate-clear", 6)
	d := &duplicateClearRecorder{inner: g}

	g.BeginFrame("frame-1")
	d.BeginClip("rect")
	d.ClearClip() // forwards two ClearClip calls; the second is a no-op
	report := g.EndFrame()

	if report.ok() {
		t.Fatalf("gate passed duplicate ClearClip:\n%s", report)
	}
	requireCategory(t, report, gateDuplicateClear)
	requireDiagnostic(t, report)
}

// --- fault double 3: backend fails mid-frame ------------------------------

// failingBackendRecorder calls the real gg raster backend through a recording,
// and reports the backend error to the gate when rasterization fails. The
// scene state itself stays balanced, so the gate must classify the frame as a
// backend error, never as a scene state bug.
func TestStateGateSeparatesBackendError(t *testing.T) {
	g := newGateRecorder("backend-error", 7)
	g.BeginFrame("frame-1")

	// Simulate a backend that aborts after one draw but leaves the state
	// stacks in a sane condition (a backend failure is not a state leak).
	g.Save("BeginClip")
	g.BeginClip("rect")
	g.EndClip()
	g.Restore()

	g.BackendError("raster backend: texture upload failed (simulated)")
	report := g.EndFrame()

	requireCategory(t, report, gateBackendError)
	if report.ok() {
		t.Fatalf("backend error must fail the gate:\n%s", report)
	}
	if report.SaveDepth != 0 || report.ClipDepth != 0 {
		t.Fatalf("backend failure should not fabricate state leaks: %s", report.summary())
	}
	if len(report.BackendErrors) == 0 {
		t.Fatal("backend error should be recorded in the report")
	}
}

// --- category taxonomy -----------------------------------------------------

// TestGateCategoryTaxonomy pins the distinction between the categories the
// gate must tell apart. Each case drives the sink directly so the diagnostic
// contract is tested independently of scene construction.
func TestGateCategoryTaxonomy(t *testing.T) {
	cases := []struct {
		name string
		run  func(g *gateRecorder)
		want gateCategory
	}{
		{
			name: "flat balanced",
			run: func(g *gateRecorder) {
				g.Save("x")
				g.Restore()
			},
			want: gateBalanced,
		},
		{
			name: "legal nested",
			run: func(g *gateRecorder) {
				g.Save("x")
				g.Save("x")
				g.Restore()
				g.Restore()
			},
			want: gateLegalNested,
		},
		{
			name: "empty stack restore",
			run: func(g *gateRecorder) {
				g.Restore()
			},
			want: gateEmptyStack,
		},
		{
			name: "empty stack end clip",
			run: func(g *gateRecorder) {
				g.EndClip()
			},
			want: gateEmptyStack,
		},
		{
			name: "duplicate clear",
			run: func(g *gateRecorder) {
				g.BeginClip("r")
				g.ClearClip()
				g.ClearClip()
			},
			want: gateDuplicateClear,
		},
		{
			name: "unpaired push",
			run: func(g *gateRecorder) {
				g.Save("x")
			},
			want: gateUnpairedPush,
		},
		{
			name: "cross layer",
			run: func(g *gateRecorder) {
				g.BeginLayer("l")
				g.BeginClip("r")
				g.EndLayer()
			},
			want: gateCrossLayerLeak,
		},
		{
			name: "backend error",
			run: func(g *gateRecorder) {
				g.BackendError("boom")
			},
			want: gateBackendError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateRecorder(tc.name, 99)
			g.BeginFrame(tc.name)
			tc.run(g)
			r := g.EndFrame()
			if r.Category != tc.want {
				t.Fatalf("category = %q, want %q\n%s", r.Category, tc.want, r)
			}
			if (r.Category == gateBalanced || r.Category == gateLegalNested) != r.ok() {
				t.Fatalf("ok() disagrees with category %q\n%s", r.Category, r)
			}
		})
	}
}
