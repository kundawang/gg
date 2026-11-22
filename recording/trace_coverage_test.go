package recording

import (
	"errors"
	"image"
	"image/color"
	"math"
	"strings"
	"testing"

	"github.com/gogpu/gg"
)

// This file closes structural coverage gaps in the trace summary and
// validation helpers: text stroking, every pooled image type, nil and
// empty resources, and the remaining enum/error branches.

// TestTraceStrokeText exercises the StrokeText command path and its stroke
// parameter summary.
func TestTraceStrokeText(t *testing.T) {
	face := loadGoRegularFace(t)
	rec := NewRecorder(200, 60)
	rec.SetFont(face)
	rec.SetFontSize(20)
	rec.SetStrokeRGB(0, 0, 0)
	rec.SetLineWidth(1.5)
	rec.SetLineCap(LineCapSquare)
	rec.SetLineJoin(LineJoinRound)
	rec.StrokeString("Outlined", 10, 40)
	r := rec.FinishRecording()

	events, err := r.TraceEvents(&traceSpyBackend{})
	if err != nil {
		t.Fatalf("TraceEvents: %v", err)
	}
	var found bool
	for _, ev := range events {
		if ev.Type != "StrokeText" {
			continue
		}
		found = true
		if ev.Params["text"] != "Outlined" {
			t.Fatalf("text = %v, want Outlined", ev.Params["text"])
		}
		if ev.Params["strokeWidth"] != 1.5 {
			t.Fatalf("strokeWidth = %v, want 1.5", ev.Params["strokeWidth"])
		}
		if ev.Params["cap"] != "square" || ev.Params["join"] != "round" {
			t.Fatalf("cap/join = %v/%v, want square/round", ev.Params["cap"], ev.Params["join"])
		}
		if ev.Resources == nil || ev.Resources.Font == nil || ev.Resources.Font.Nil {
			t.Fatalf("StrokeText font summary wrong: %+v", ev.Resources)
		}
		if ev.Resources.Brush == nil {
			t.Fatal("StrokeText missing brush summary")
		}
	}
	if !found {
		t.Fatal("no StrokeText event emitted")
	}
}

// TestTraceImageTypeSummaries covers the concrete image type switch in the
// byte-estimate helper and the nil-image branch.
func TestTraceImageTypeSummaries(t *testing.T) {
	cases := []struct {
		name      string
		img       image.Image
		wantKind  string
		wantBytes int
	}{
		{"rgba", image.NewRGBA(image.Rect(0, 0, 3, 2)), "*image.RGBA", 3 * 2 * 4},
		{"rgba64", image.NewRGBA64(image.Rect(0, 0, 3, 2)), "*image.RGBA64", 3 * 2 * 8},
		{"nrgba", image.NewNRGBA(image.Rect(0, 0, 3, 2)), "*image.NRGBA", 3 * 2 * 4},
		{"nrgba64", image.NewNRGBA64(image.Rect(0, 0, 3, 2)), "*image.NRGBA64", 3 * 2 * 8},
		{"gray", image.NewGray(image.Rect(0, 0, 3, 2)), "*image.Gray", 3 * 2},
		{"gray16", image.NewGray16(image.Rect(0, 0, 3, 2)), "*image.Gray16", 3 * 2 * 2},
		{"alpha", image.NewAlpha(image.Rect(0, 0, 3, 2)), "*image.Alpha", 3 * 2},
		{"alpha16", image.NewAlpha16(image.Rect(0, 0, 3, 2)), "*image.Alpha16", 3 * 2 * 2},
		{"paletted", image.NewPaletted(image.Rect(0, 0, 3, 2), color.Palette{image.Black, image.White}), "*image.Paletted", 3 * 2},
		{"ycbcr", image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420), "*image.YCbCr", 4 * 4 * 3 / 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := summarizeImage(tc.img, ImageRef(0))
			if info.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", info.Kind, tc.wantKind)
			}
			if info.Bytes != tc.wantBytes {
				t.Fatalf("bytes = %d, want %d", info.Bytes, tc.wantBytes)
			}
			bounds := tc.img.Bounds()
			if info.Width != bounds.Dx() || info.Height != bounds.Dy() {
				t.Fatalf("dims = %dx%d, want %dx%d", info.Width, info.Height, bounds.Dx(), bounds.Dy())
			}
		})
	}

	// Nil image branch and the generic fallback type. A Uniform has no
	// finite bounds, so it takes the default per-pixel estimate rather than
	// a type-specific stride.
	if got := summarizeImage(nil, ImageRef(1)); got.Kind != "nil" || got.Ref != 1 {
		t.Fatalf("nil image summary = %+v", got)
	}
	generic := summarizeImage(image.NewUniform(gg.Black), ImageRef(2))
	if generic.Kind != "*image.Uniform" {
		t.Fatalf("uniform image kind = %q, want *image.Uniform", generic.Kind)
	}
	if generic.Width <= 0 || generic.Bytes <= 0 {
		t.Fatalf("uniform image summary missing size estimate: %+v", generic)
	}
}

// TestTraceNilAndEmptyResources covers nil paths/brushes in summaries.
func TestTraceNilAndEmptyResources(t *testing.T) {
	// A recording whose resource pool holds a nil path and nil brush.
	rec := NewRecorder(40, 40)
	nilPathRef := rec.resources.AddPath(nil)
	nilBrushRef := rec.resources.AddBrush(nil)
	rec.commands = append(rec.commands,
		SetClipCommand{Path: nilPathRef, Rule: FillRuleNonZero},
		FillPathCommand{Path: nilPathRef, Brush: nilBrushRef, Rule: FillRuleEvenOdd},
	)
	r := rec.FinishRecording()

	events, err := r.TraceEvents(&traceSpyBackend{})
	if err != nil {
		t.Fatalf("TraceEvents with nil resources: %v", err)
	}
	for _, ev := range events {
		if ev.Resources == nil {
			continue
		}
		if ev.Resources.Path != nil && !ev.Resources.Path.Empty {
			t.Fatalf("nil path summary not marked empty: %+v", ev.Resources.Path)
		}
		if ev.Resources.Brush != nil && ev.Resources.Brush.Kind != "nil" {
			t.Fatalf("nil brush summary kind = %q, want nil", ev.Resources.Brush.Kind)
		}
	}
}

// TestTraceEmptyPathSummary covers a non-nil but verb-free path.
func TestTraceEmptyPathSummary(t *testing.T) {
	empty := gg.NewPath()
	info, err := summarizePath(empty, PathRef(7))
	if err != nil {
		t.Fatalf("summarizePath: %v", err)
	}
	if !info.Empty || info.Verbs != 0 || info.Ref != 7 || info.VerbKinds != nil {
		t.Fatalf("empty path summary = %+v", info)
	}
}

// TestTraceEnumNames exercises the non-default enum name branches.
func TestTraceEnumNames(t *testing.T) {
	if got := lineCapName(LineCapButt); got != "butt" {
		t.Fatalf("butt = %q", got)
	}
	if got := lineCapName(LineCapRound); got != "round" {
		t.Fatalf("round = %q", got)
	}
	if got := lineCapName(LineCapSquare); got != "square" {
		t.Fatalf("square = %q", got)
	}
	if got := lineJoinName(LineJoinMiter); got != "miter" {
		t.Fatalf("miter = %q", got)
	}
	if got := lineJoinName(LineJoinRound); got != "round" {
		t.Fatalf("round join = %q", got)
	}
	if got := lineJoinName(LineJoinBevel); got != "bevel" {
		t.Fatalf("bevel = %q", got)
	}
	if got := fillRuleName(FillRuleNonZero); got != "nonZero" {
		t.Fatalf("nonZero = %q", got)
	}
	if got := fillRuleName(FillRuleEvenOdd); got != "evenOdd" {
		t.Fatalf("evenOdd = %q", got)
	}
	if got := interpolationName(InterpolationNearest); got != "nearest" {
		t.Fatalf("nearest = %q", got)
	}
	if got := interpolationName(InterpolationBilinear); got != "bilinear" {
		t.Fatalf("bilinear = %q", got)
	}
}

// TestTraceCapString covers the diagnostic-string truncation helper and its
// no-op short-string branch.
func TestTraceCapString(t *testing.T) {
	if got := capString("short", 100); got != "short" {
		t.Fatalf("short string altered: %q", got)
	}
	long := strings.Repeat("z", 30)
	got := capString(long, 10)
	if len(got) != 10 || !strings.HasSuffix(got, "...") {
		t.Fatalf("capString = %q (len %d), want 10 chars ending ...", got, len(got))
	}
}

// TestTraceNonFiniteInTextAndStroke covers NaN/Inf in the remaining numeric
// command fields: text coordinates, font size and stroke metrics.
func TestTraceNonFiniteInTextAndStroke(t *testing.T) {
	cases := []struct {
		name  string
		build func(rec *Recorder)
		want  string
	}{
		{
			name:  "textX",
			build: func(rec *Recorder) { rec.DrawString("x", math.NaN(), 1) },
			want:  "x",
		},
		{
			// With an identity CTM, NaN propagates straight to the stored Y
			// coordinate. (Inf is unsuitable here because 0*Inf is NaN under
			// IEEE754 once any transform arithmetic touches it.) The
			// validator reports sorted field names, so assert the failing
			// command context rather than which of x/y is named first.
			name:  "textY",
			build: func(rec *Recorder) { rec.DrawString("x", 1, math.NaN()) },
			want:  "DrawText",
		},
		{
			name: "strokeWidth",
			build: func(rec *Recorder) {
				rec.SetLineWidth(math.NaN())
				rec.MoveTo(0, 0)
				rec.LineTo(1, 1)
				rec.Stroke()
			},
			// SetLineWidth validates under its own "width" field before the
			// stroke command's "strokeWidth" summary is ever built.
			want: "width",
		},
		{
			name: "dashOffset",
			build: func(rec *Recorder) {
				rec.SetDash(2)
				rec.SetDashOffset(math.Inf(-1))
			},
			// SetDashOffset emits a SetDash command whose offset field is
			// validated as "offset".
			want: "offset",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := NewRecorder(40, 40)
			tc.build(rec)
			r := rec.FinishRecording()
			err := r.TracePlayback(&traceSpyBackend{}, TraceOptions{Writer: &strings.Builder{}})
			if !errors.Is(err, ErrNonFinite) {
				t.Fatalf("err = %v, want ErrNonFinite", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q should name %q", err.Error(), tc.want)
			}
		})
	}
}

// TestTraceNonFiniteInColorAndImage ensures NaN colors and image options are
// rejected with field-naming errors.
func TestTraceNonFiniteInColorAndImage(t *testing.T) {
	rec := NewRecorder(40, 40)
	rec.SetFillStyle(NewSolidBrush(gg.RGBA{R: math.NaN(), G: 0, B: 0, A: 1}))
	rec.FillRectangle(0, 0, 5, 5)
	r := rec.FinishRecording()
	err := r.TracePlayback(&traceSpyBackend{}, TraceOptions{Writer: &strings.Builder{}})
	if !errors.Is(err, ErrNonFinite) || !strings.Contains(err.Error(), "r") {
		t.Fatalf("NaN color err = %v", err)
	}

	// Non-finite image alpha.
	rec2 := NewRecorder(40, 40)
	rec2.DrawImage(image.NewRGBA(image.Rect(0, 0, 2, 2)), 0, 0)
	r2 := rec2.FinishRecording()
	for i, cmd := range r2.commands {
		if di, ok := cmd.(DrawImageCommand); ok {
			di.Options.Alpha = math.NaN()
			r2.commands[i] = di
			break
		}
	}
	err = r2.TracePlayback(&traceSpyBackend{}, TraceOptions{Writer: &strings.Builder{}})
	if !errors.Is(err, ErrNonFinite) || !strings.Contains(err.Error(), "imageAlpha") {
		t.Fatalf("NaN image alpha err = %v", err)
	}
}

// TestTraceOversizedFontFamily verifies the string gate also applies to the
// font family field, not just text content.
func TestTraceOversizedFontFamily(t *testing.T) {
	longFamily := strings.Repeat("F", DefaultMaxStringLength+1)
	rec := NewRecorder(200, 60)
	rec.SetFontFamily(longFamily)
	rec.DrawString("x", 0, 10)
	r := rec.FinishRecording()

	_, err := r.TraceEvents(&traceSpyBackend{})
	if !errors.Is(err, ErrStringTooLong) || !strings.Contains(err.Error(), "fontFamily") {
		t.Fatalf("err = %v, want ErrStringTooLong naming fontFamily", err)
	}
}

// TestTraceKnownScalarCommands covers the parameter-only style dispatch.
func TestTraceKnownScalarCommands(t *testing.T) {
	params, _, err := describeStyleScalar(SetAntiAliasCommand{Enabled: true})
	if err != nil {
		t.Fatalf("describeStyleScalar: %v", err)
	}
	if params["enabled"] != true {
		t.Fatalf("enabled = %v, want true", params["enabled"])
	}
	params, _, err = describeStyleScalar(SetLineCapCommand{Cap: LineCapRound})
	if err != nil {
		t.Fatalf("line cap: %v", err)
	}
	if params["cap"] != "round" {
		t.Fatalf("cap = %v, want round", params["cap"])
	}
}
