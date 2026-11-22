package raster

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/gogpu/gg/recording"
	"github.com/gogpu/gg/text"
	"golang.org/x/image/font/gofont/goregular"
)

var updatePixelGolden = flag.Bool("update-playback-pixel-golden", false,
	"regenerate text/image/clip playback pixel golden files")

func pixelTestImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 16, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 16; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 16),
				G: uint8(y * 20),
				B: 0x40,
				A: 0xff,
			})
		}
	}
	return img
}

// pixelGoldenRecording builds one recording that combines text, images, clips,
// transforms and Save/Restore nesting. The same builder runs against the
// pre-refactor code to produce the golden, so any divergence in resource
// resolution, clip scoping or transform handling shows up as differing pixels.
func pixelGoldenRecording(fontSrc *text.FontSource) *recording.Recording {
	face := fontSrc.Face(22)
	img := pixelTestImage()

	rec := recording.NewRecorder(160, 120)

	// Background.
	rec.SetRGB(1, 1, 1)
	rec.FillRectangle(0, 0, 160, 120)

	// Unclipped text and image.
	rec.SetFillRGBA(0, 0, 0, 1)
	rec.SetFont(face)
	rec.SetFontSize(22)
	rec.DrawString("Go", 6, 22)
	rec.DrawImage(img, 100, 4)

	// Scaled, translated text and image inside a Save/Restore scope.
	rec.Save()
	rec.Translate(10, 40)
	rec.Scale(1.5, 1.5)
	rec.SetFillRGBA(0.7, 0, 0, 1)
	rec.DrawString("Hi", 0, 12)
	rec.DrawImageScaled(img, 40, 0, 24, 18)
	rec.Restore()

	// Text and image clipped by a transformed rounded rectangle.
	rec.Save()
	rec.Translate(84, 44)
	rec.ClipRoundRect(0, 0, 60, 50, 10)
	rec.Identity()
	rec.SetFillRGBA(0, 0, 0.8, 1)
	rec.SetFont(face)
	rec.SetFontSize(18)
	rec.DrawString("clip me", 82, 62)
	rec.DrawString("second line", 78, 80)
	rec.DrawImageScaled(img, 78, 44, 64, 48)
	rec.Restore()

	// Path clip combined with a rotated stroke rectangle.
	rec.Save()
	rec.DrawRectangle(8, 92, 70, 24)
	rec.Clip()
	rec.SetTransform(recording.Translate(20, 100).Multiply(recording.Rotate(math.Pi / 12)))
	rec.SetStrokeRGBA(0, 0.6, 0, 1)
	rec.SetLineWidth(3)
	rec.StrokeRectangle(0, 0, 40, 12)
	rec.Restore()

	return rec.FinishRecording()
}

// TestPlaybackPixelGolden compares playback pixels against a golden encoding
// produced by the pre-refactor implementation. The golden stores a SHA-256 per
// backend configuration plus a sample of pixel values, so mismatches are
// readable without committing large PNGs.
func TestPlaybackPixelGolden(t *testing.T) {
	fontSrc, err := text.NewFontSource(goregular.TTF)
	if err != nil {
		t.Fatalf("NewFontSource: %v", err)
	}
	t.Cleanup(func() { _ = fontSrc.Close() })

	rec := pixelGoldenRecording(fontSrc)

	configs := []struct {
		name  string
		scale float64
	}{
		{name: "1x", scale: 1},
		{name: "2x", scale: 2},
	}

	goldenPath := filepath.Join("testdata", "playback_pixel_golden.txt")
	var report bytes.Buffer
	for _, cfg := range configs {
		backend := NewBackendWithScale(cfg.scale)
		if err := rec.Playback(backend); err != nil {
			t.Fatalf("%s: Playback: %v", cfg.name, err)
		}
		rgba := toRGBA(backend.Image())
		sum := sha256.Sum256(rgba.Pix)
		report.WriteString(cfg.name + " " + hex.EncodeToString(sum[:]) + "\n")
	}

	if *updatePixelGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(goldenPath, report.Bytes(), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (generate it with the pre-refactor code): %v", err)
	}
	if report.String() != string(want) {
		t.Fatalf("playback pixels differ from golden.\n got:\n%s\nwant:\n%s", report.String(), want)
	}
}
