package raster

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"testing"

	"github.com/gogpu/gg/recording"
)

// buildTimelineRecording records a labeled multi-layer scene exercising
// nested transforms, clipping, an image, and text — the shape of content a
// timeline editor would scrub through.
func buildTimelineRecording(t *testing.T) *recording.Recording {
	t.Helper()
	face := loadGoRegular(t)

	rec := recording.NewRecorder(200, 200)
	rec.SetFont(face)
	rec.SetFontSize(20)

	rec.SetFillRGB(1, 1, 1)
	rec.FillRectangle(0, 0, 200, 200)

	rec.Mark("layer1")
	rec.Save()
	rec.Translate(50, 40)
	rec.Rotate(0.3)
	rec.SetFillRGB(0.9, 0.1, 0.1)
	rec.DrawCircle(30, 30, 25)
	rec.Fill()
	rec.Save()
	rec.Scale(1.5, 0.75)
	rec.SetStrokeRGB(0, 0, 0.8)
	rec.SetLineWidth(3)
	rec.SetDash(6, 3)
	rec.DrawRectangle(-20, 50, 70, 30)
	rec.Stroke()
	rec.Restore()

	rec.Mark("layer2")
	rec.DrawRectangle(10, 10, 120, 120)
	rec.Clip()
	rec.SetFillRGB(0.1, 0.7, 0.2)
	rec.FillRectangle(0, 90, 200, 60)
	rec.ClipRoundRect(120, 20, 60, 50, 10)
	rec.SetFillRGBA(0.2, 0.2, 0.9, 0.8)
	rec.FillRectangle(100, 10, 90, 70)
	rec.ResetClip()
	rec.Restore()

	rec.Mark("layer3")
	img := image.NewRGBA(image.Rect(0, 0, 24, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 24; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 10), G: uint8(y * 10), B: 200, A: 255})
		}
	}
	rec.DrawImage(img, 150, 150)
	rec.SetFillRGB(0, 0, 0)
	rec.DrawString("frame", 20, 180)
	rec.SetStrokeRGB(0.5, 0, 0.5)
	rec.SetLineWidth(1)
	rec.StrokeString("edge", 110, 180)

	return rec.FinishRecording()
}

// rgbaPixels normalizes an image to RGBA pixel bytes for comparison.
func rgbaPixels(img image.Image) []byte {
	b := img.Bounds()
	rgba := image.NewRGBA(image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy())).Bounds())
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	return rgba.Pix
}

func TestCursorStitchedPlaybackMatchesFullPlaybackPixels(t *testing.T) {
	r := buildTimelineRecording(t)

	full := NewBackend()
	if err := r.Playback(full); err != nil {
		t.Fatalf("Playback: %v", err)
	}
	want := rgbaPixels(full.Image())

	// Sanity: the full frame actually contains ink.
	if bytes.Equal(want, rgbaPixels(image.NewRGBA(image.Rect(0, 0, 200, 200)))) {
		t.Fatal("full playback produced a blank frame")
	}

	n := len(r.Commands())
	i1, ok := r.LabelIndex("layer1")
	if !ok {
		t.Fatal("missing label layer1")
	}
	i3, ok := r.LabelIndex("layer3")
	if !ok {
		t.Fatal("missing label layer3")
	}

	// Stitch four adjacent ranges — created by index, by single label, and
	// by label pair — into one backend frame.
	stitched := NewBackend()
	sess := r.NewSession(stitched)
	c0, err := r.Cursor(0, i1)
	if err != nil {
		t.Fatalf("Cursor(0, %d): %v", i1, err)
	}
	c1, err := r.CursorForLabel("layer1")
	if err != nil {
		t.Fatalf("CursorForLabel(layer1): %v", err)
	}
	c2, err := r.CursorBetweenLabels("layer2", "layer3")
	if err != nil {
		t.Fatalf("CursorBetweenLabels: %v", err)
	}
	c3, err := r.Cursor(i3, n)
	if err != nil {
		t.Fatalf("Cursor(%d, %d): %v", i3, n, err)
	}
	for i, c := range []*recording.Cursor{c0, c1, c2, c3} {
		if err := sess.Resume(c); err != nil {
			t.Fatalf("Resume cursor %d: %v", i, err)
		}
	}
	if err := sess.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got := rgbaPixels(stitched.Image())
	if !bytes.Equal(want, got) {
		diff := 0
		for i := range want {
			if want[i] != got[i] {
				diff++
			}
		}
		t.Errorf("stitched range playback differs from full playback in %d/%d bytes", diff, len(want))
	}
}

func TestCursorSingleRangeSelfContained(t *testing.T) {
	r := buildTimelineRecording(t)

	// A single mid-stream cursor renders a self-contained frame: the label
	// segment's own drawing appears, rebuilt on top of the state prefix.
	c, err := r.CursorForLabel("layer3")
	if err != nil {
		t.Fatalf("CursorForLabel: %v", err)
	}
	b := NewBackend()
	if err := c.Resume(b); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	blank := rgbaPixels(image.NewRGBA(image.Rect(0, 0, 200, 200)))
	if px := rgbaPixels(b.Image()); bytes.Equal(px, blank) {
		t.Error("layer3 range rendered a blank frame; state prefix or text replay missing")
	}
}
