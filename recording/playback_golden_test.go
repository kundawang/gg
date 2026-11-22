package recording

import (
	"flag"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
	"golang.org/x/image/font/gofont/goregular"
)

// updateGolden regenerates command-level golden files from the current
// implementation. Generate them from the pre-refactor implementation to prove
// command-for-command parity, then run this suite without the flag.
var updateGolden = flag.Bool("update-playback-golden", false, "update playback golden files")

// traceBackend renders nothing. It records every Backend call with its exact
// arguments so a golden file can pin the command stream independently of any
// particular rasterizer.
type traceBackend struct {
	lines []string
}

func (b *traceBackend) logf(format string, args ...any) {
	b.lines = append(b.lines, fmt.Sprintf(format, args...))
}

func (b *traceBackend) Begin(width, height int) error {
	b.logf("Begin %d %d", width, height)
	return nil
}

func (b *traceBackend) End() error {
	b.logf("End")
	return nil
}

func (b *traceBackend) Save() {
	b.logf("Save")
}

func (b *traceBackend) Restore() {
	b.logf("Restore")
}

func (b *traceBackend) SetTransform(m Matrix) {
	b.logf("SetTransform %s", traceMatrix(m))
}

func (b *traceBackend) SetClip(path *gg.Path, rule FillRule) {
	b.logf("SetClip %s %v", tracePath(path), rule)
}

func (b *traceBackend) ClearClip() {
	b.logf("ClearClip")
}

func (b *traceBackend) FillPath(path *gg.Path, brush Brush, rule FillRule) {
	b.logf("FillPath %s %s %v", tracePath(path), traceBrush(brush), rule)
}

func (b *traceBackend) StrokePath(path *gg.Path, brush Brush, stroke Stroke) {
	b.logf("StrokePath %s %s %s", tracePath(path), traceBrush(brush), traceStroke(stroke))
}

func (b *traceBackend) FillRect(rect Rect, brush Brush) {
	b.logf("FillRect %s %s", traceRect(rect), traceBrush(brush))
}

func (b *traceBackend) DrawImage(img image.Image, src, dst Rect, opts ImageOptions) {
	bounds := "<nil>"
	if img != nil {
		bounds = img.Bounds().String()
	}
	b.logf("DrawImage %s %s %s %v %.3f", bounds, traceRect(src), traceRect(dst), opts.Interpolation, opts.Alpha)
}

func (b *traceBackend) DrawText(s string, x, y float64, face text.Face, brush Brush) {
	size := "<nil>"
	if face != nil {
		size = fmt.Sprintf("%.3f", face.Size())
	}
	b.logf("DrawText %q %s %s face=%s %s", s, traceFloat(x), traceFloat(y), size, traceBrush(brush))
}

func traceFloat(v float64) string {
	return fmt.Sprintf("%.6g", v)
}

func traceMatrix(m Matrix) string {
	return fmt.Sprintf("[%s %s %s %s %s %s]",
		traceFloat(m.A), traceFloat(m.B), traceFloat(m.C),
		traceFloat(m.D), traceFloat(m.E), traceFloat(m.F))
}

func traceRect(r Rect) string {
	return fmt.Sprintf("[%s %s %s %s]",
		traceFloat(r.MinX), traceFloat(r.MinY), traceFloat(r.MaxX), traceFloat(r.MaxY))
}

func tracePath(p *gg.Path) string {
	if p == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString("{")
	p.Iterate(func(verb gg.PathVerb, coords []float64) {
		b.WriteString(verb.String())
		b.WriteString("(")
		for i, c := range coords {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(traceFloat(c))
		}
		b.WriteString(")")
	})
	b.WriteString("}")
	return b.String()
}

func traceBrush(brush Brush) string {
	switch b := brush.(type) {
	case nil:
		return "<nil>"
	case SolidBrush:
		c := b.Color
		return fmt.Sprintf("solid(%.3f,%.3f,%.3f,%.3f)", c.R, c.G, c.B, c.A)
	case *LinearGradientBrush:
		return fmt.Sprintf("linear(%s,%s stops=%d)", traceFloat(b.Start.X), traceFloat(b.Start.Y), len(b.Stops))
	case *RadialGradientBrush:
		return fmt.Sprintf("radial(%s,%s r=%s..%s stops=%d)",
			traceFloat(b.Center.X), traceFloat(b.Center.Y), traceFloat(b.StartRadius), traceFloat(b.EndRadius), len(b.Stops))
	case *SweepGradientBrush:
		return fmt.Sprintf("sweep(%s,%s stops=%d)", traceFloat(b.Center.X), traceFloat(b.Center.Y), len(b.Stops))
	default:
		return fmt.Sprintf("brush(%T)", brush)
	}
}

func traceStroke(s Stroke) string {
	return fmt.Sprintf("stroke(w=%s cap=%d join=%d miter=%s dash=%v off=%s)",
		traceFloat(s.Width), s.Cap, s.Join, traceFloat(s.MiterLimit), s.DashPattern, traceFloat(s.DashOffset))
}

// playbackGoldenScenarios builds recordings that, together, cover every
// command type and the state-sensitive paths: Save/Restore nesting, unbalanced
// restores, clips under transforms, isolated StrokeRect playback, text, images
// and style no-ops.
func playbackGoldenScenarios(t *testing.T) map[string]*Recording {
	t.Helper()

	src, err := text.NewFontSource(goregular.TTF)
	if err != nil {
		t.Fatalf("NewFontSource: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	face := src.Face(24)

	img := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for i := range img.Pix {
		img.Pix[i] = 0x40
	}

	scenarios := make(map[string]*Recording)

	// Every command type in one recording, including style no-ops.
	all := NewRecorder(120, 90)
	all.SetRGB(1, 0, 0)
	all.SetFillRGBA(0, 1, 0, 0.5)
	all.SetStrokeRGBA(0, 0, 1, 0.25)
	all.SetHexColor("#123456")
	all.SetFillStyle(NewSolidBrush(gg.RGBA{R: 0.1, G: 0.2, B: 0.3, A: 1}))
	all.SetStrokeStyle(NewSolidBrush(gg.RGBA{R: 0.3, G: 0.2, B: 0.1, A: 0.9}))
	all.SetLineWidth(2.5)
	all.SetLineCap(LineCapRound)
	all.SetLineJoin(LineJoinBevel)
	all.SetMiterLimit(6)
	all.SetDash(4, 2)
	all.SetDashOffset(1.5)
	all.SetFillRule(FillRuleEvenOdd)
	all.SetAntiAlias(false)
	all.Save()
	all.Translate(10, 12)
	all.Scale(1.5, 0.75)
	all.Rotate(0.3)
	all.Shear(0.1, 0.2)
	all.SetTransform(Identity().Multiply(Translate(2, 3)))
	all.Identity()
	all.DrawEllipticalArc(30, 30, 20, 10, 0, 2)
	all.DrawRoundedRectangle(5, 5, 40, 30, 6)
	all.FillPreserve()
	all.StrokePreserve()
	all.ClearPath()
	all.DrawImage(img, 10, 10)
	all.DrawImageAnchored(img, 50, 40, 0.5, 0.5)
	all.DrawImageScaled(img, 60, 10, 20, 15)
	all.SetFont(face)
	all.SetFontSize(24)
	all.SetFontFamily("Go")
	all.DrawString("golden", 8, 70)
	all.DrawStringAnchored("anch", 60, 70, 0.5, 0.5)
	all.StrokeString("outline", 80, 82)
	all.StrokeStringAnchored("anch2", 100, 82, 1, 1)
	all.ClearDash()
	all.Restore()
	scenarios["all_commands"] = all.FinishRecording()

	// Nested Save/Restore with clips and transforms.
	nested := NewRecorder(100, 100)
	nested.SetFillRule(FillRuleNonZero)
	nested.Save()
	nested.Translate(10, 10)
	nested.DrawRectangle(0, 0, 20, 20)
	nested.Clip()
	nested.Save()
	nested.Scale(2, 2)
	nested.ClipRoundRect(5, 5, 30, 20, 4)
	nested.DrawRectangle(5, 5, 10, 10)
	nested.Fill()
	nested.Restore()
	nested.ResetClip()
	nested.Restore()
	nested.DrawCircle(50, 50, 20)
	nested.Stroke()
	scenarios["nested_state"] = nested.FinishRecording()

	// ClipRoundRect under every transform family, to pin the uniform vs full
	// path branch used by playback.
	transforms := map[string]Matrix{
		"identity":  Identity(),
		"translate": Translate(15, 8),
		"scale":     Translate(5, 5).Multiply(Scale(1.5, 0.75)),
		"uniform":   Translate(5, 5).Multiply(Scale(2, 2)),
		"rotate":    Translate(60, 5).Multiply(Rotate(0.5)),
		"shear":     Translate(5, 5).Multiply(Shear(0.6, 0.2)),
		"mirror":    Translate(130, 0).Multiply(Scale(-1, 1)),
	}
	for name, transform := range transforms {
		rec := NewRecorder(120, 120)
		rec.SetTransform(transform)
		rec.ClipRoundRect(20, 20, 40, 30, 12)
		rec.ClipRoundRect(10, 10, 20, 20, 0)
		rec.SetRGB(1, 0, 0)
		rec.FillRectangle(0, 0, 120, 120)
		scenarios["clip_roundrect_"+name] = rec.FinishRecording()
	}

	// StrokeRect in the compact form and the expanded path form.
	for name, transform := range map[string]Matrix{
		"compact":  Translate(10, 15),
		"rotation": {A: 0, B: -1, C: 100, D: 1, E: 0},
		"negative": Identity(),
	} {
		rec := NewRecorder(120, 120)
		rec.SetStrokeRGB(0, 0, 1)
		rec.SetLineWidth(3)
		rec.SetDash(4, 2)
		rec.SetTransform(transform)
		if name == "negative" {
			rec.StrokeRectangle(80, 80, -60, -60)
		} else {
			rec.StrokeRectangle(10, 20, 30, 40)
		}
		scenarios["stroke_rect_"+name] = rec.FinishRecording()
	}

	// Unbalanced Restore: playback must still forward Restore to the backend
	// while leaving the mirrored transform alone.
	unbalanced := NewRecorder(50, 50)
	unbalanced.Translate(7, 9)
	unbalanced.Restore()
	unbalanced.Pop()
	unbalanced.DrawPoint(1, 2, 3)
	unbalanced.Fill()
	scenarios["unbalanced_restore"] = unbalanced.FinishRecording()

	return scenarios
}

func TestPlaybackCommandGolden(t *testing.T) {
	scenarios := playbackGoldenScenarios(t)
	goldenPath := filepath.Join("testdata", "playback_command_golden.txt")

	var got strings.Builder
	for _, name := range sortedScenarioNames(scenarios) {
		backend := &traceBackend{}
		if err := scenarios[name].Playback(backend); err != nil {
			t.Fatalf("%s: Playback: %v", name, err)
		}
		fmt.Fprintf(&got, "== %s ==\n", name)
		for _, line := range backend.lines {
			got.WriteString(line)
			got.WriteByte('\n')
		}
	}

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got.String()), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update-playback-golden against the pre-refactor code): %v", err)
	}
	if got.String() != string(want) {
		t.Fatalf("playback command stream differs from golden.\n--- got ---\n%s\n--- want ---\n%s", got.String(), want)
	}
}

// sortedScenarioNames returns scenario names in a deterministic order.
func sortedScenarioNames(scenarios map[string]*Recording) []string {
	names := make([]string, 0, len(scenarios))
	for name := range scenarios {
		names = append(names, name)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// TestPlaybackCommandGoldenCoversAllCommands guards the scenario above: if a
// new CommandType is added without a golden scenario, this fails.
func TestPlaybackCommandGoldenCoversAllCommands(t *testing.T) {
	seen := make(map[CommandType]bool)
	for _, rec := range playbackGoldenScenarios(t) {
		for _, cmd := range rec.Commands() {
			seen[cmd.Type()] = true
		}
	}
	for typ := CommandType(0); typ < CommandType(len(commandTypeNames)); typ++ {
		if !seen[typ] {
			t.Errorf("command type %s is not exercised by the playback golden scenarios", typ)
		}
	}
}
