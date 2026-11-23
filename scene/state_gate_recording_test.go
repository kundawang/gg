package scene_test

// Gate adapter for the recording.Backend surface.
//
// recording.Playback drives third-party backends through Save/Restore/
// SetTransform/SetClip/ClearClip. Those backends are stateful in exactly the
// same way scene renderers are: the raster backend maps them onto
// gg.Context.Push/Pop/SetTransform/Clip/ResetClip. A playback that drops a
// Restore or duplicates a ClearClip leaves the backend dirty for the next
// recording, which is the "color and clip pollution on the next frame"
// symptom this gate exists to catch.
//
// gateRecordingBackend forwards every state call to a gateSink. Test-only:
// it renders nothing.

import (
	"fmt"
	"image"
	"strings"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/recording"
	"github.com/gogpu/gg/scene"
	"github.com/gogpu/gg/text"
)

// Compile-time confirmation that the adapter satisfies recording.Backend.
var _ recording.Backend = (*gateRecordingBackend)(nil)

// gateRecordingBackend adapts recording.Backend onto a gateSink.
type gateRecordingBackend struct {
	sink gateSink

	// dropRestore simulates a playback/backend pair that loses a Restore.
	dropRestore bool
	// duplicateClear simulates a backend that clears the clip twice per request.
	duplicateClear bool
	// failAfterDraws reports a mid-frame backend error once that many drawing
	// commands have completed. Zero disables the fault.
	failAfterDraws int
	draws          int
	failed         bool
}

// newGateRecordingBackend creates a recording backend that forwards state to
// the given sink.
func newGateRecordingBackend(sink gateSink) *gateRecordingBackend {
	return &gateRecordingBackend{sink: sink}
}

// Begin implements recording.Backend.
func (b *gateRecordingBackend) Begin(_, _ int) error {
	b.sink.SetTransform(scene.IdentityAffine())
	return nil
}

// End implements recording.Backend.
func (b *gateRecordingBackend) End() error { return nil }

// Save implements recording.Backend.
func (b *gateRecordingBackend) Save() { b.sink.Save("recording.Save") }

// Restore implements recording.Backend.
func (b *gateRecordingBackend) Restore() {
	if b.dropRestore {
		return
	}
	b.sink.Restore()
}

// SetTransform implements recording.Backend.
func (b *gateRecordingBackend) SetTransform(m recording.Matrix) {
	b.sink.SetTransform(scene.NewAffine(
		float32(m.A), float32(m.B), float32(m.C),
		float32(m.D), float32(m.E), float32(m.F)))
}

// SetClip implements recording.Backend. A SetClip intersects with the current
// clip and stays active until ClearClip or a Restore, which the gate models as
// BeginClip.
func (b *gateRecordingBackend) SetClip(path *gg.Path, _ recording.FillRule) {
	if path == nil {
		return
	}
	b.sink.BeginClip(ggPathSignature(path))
}

// ClearClip implements recording.Backend.
func (b *gateRecordingBackend) ClearClip() {
	b.sink.ClearClip()
	if b.duplicateClear {
		b.sink.ClearClip()
	}
}

// FillPath implements recording.Backend.
func (b *gateRecordingBackend) FillPath(*gg.Path, recording.Brush, recording.FillRule) {
	b.afterDraw()
}

// StrokePath implements recording.Backend.
func (b *gateRecordingBackend) StrokePath(*gg.Path, recording.Brush, recording.Stroke) {
	b.afterDraw()
}

// FillRect implements recording.Backend.
func (b *gateRecordingBackend) FillRect(recording.Rect, recording.Brush) { b.afterDraw() }

// DrawImage implements recording.Backend.
func (b *gateRecordingBackend) DrawImage(image.Image, recording.Rect, recording.Rect, recording.ImageOptions) {
	b.afterDraw()
}

// DrawText implements recording.Backend.
func (b *gateRecordingBackend) DrawText(string, float64, float64, text.Face, recording.Brush) {
	b.afterDraw()
}

// afterDraw counts draws and, once the configured threshold is reached, records
// a mid-frame backend error exactly once.
func (b *gateRecordingBackend) afterDraw() {
	b.draws++
	if b.failAfterDraws > 0 && !b.failed && b.draws >= b.failAfterDraws {
		b.failed = true
		b.sink.BackendError("recording backend: simulated mid-frame draw failure")
	}
}

// ggPathSignature builds a bounded description of a gg.Path for diagnostics.
func ggPathSignature(p *gg.Path) string {
	verbs := p.Verbs()
	coords := p.Coords()
	if len(verbs) == 0 {
		return "empty"
	}
	var b strings.Builder
	ci := 0
	const maxVerbs = 24
	for i, v := range verbs {
		if i >= maxVerbs {
			b.WriteString("...")
			break
		}
		switch v {
		case gg.MoveTo, gg.LineTo:
			if ci+2 <= len(coords) {
				b.WriteString(v.String())
				b.WriteString(":")
				b.WriteString(formatGateFloats(coords[ci], coords[ci+1]))
				ci += 2
			}
		case gg.QuadTo:
			if ci+4 <= len(coords) {
				b.WriteString("Q:")
				b.WriteString(formatGateFloats(coords[ci], coords[ci+1], coords[ci+2], coords[ci+3]))
				ci += 4
			}
		case gg.CubicTo:
			if ci+6 <= len(coords) {
				b.WriteString("C:")
				b.WriteString(formatGateFloats(coords[ci : ci+6]...))
				ci += 6
			}
		case gg.Close:
			b.WriteString("Z")
		}
		if i+1 < len(verbs) && i+1 < maxVerbs {
			b.WriteString(" ")
		}
	}
	return b.String()
}

// formatGateFloats formats coordinates without trailing zeros.
func formatGateFloats(vals ...float64) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprintf("%g", v)
	}
	return strings.Join(parts, ",")
}
