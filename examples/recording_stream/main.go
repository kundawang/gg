// Example: Streaming a Recording Between Processes
//
// This example shows the incremental recording stream: a producer records a
// long animation frame-by-frame into an io.Pipe while a consumer in another
// goroutine decodes the same stream and renders it without ever holding the
// whole recording in memory. In a real deployment the two ends would be
// separate processes connected by a pipe, socket, or file.
//
// Run:
//
//	go run .            # renders every frame at 1x
//	go run . -frames 60 # render more frames
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"math"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/recording"
	_ "github.com/gogpu/gg/recording/backends/raster"
)

func main() {
	frames := flag.Int("frames", 24, "number of animation frames")
	width := flag.Int("width", 320, "canvas width")
	height := flag.Int("height", 240, "canvas height")
	flag.Parse()

	pr, pw := io.Pipe()

	// Consumer: decode and render as frames arrive.
	type result struct {
		commands uint64
		frames   int
		err      error
	}
	done := make(chan result, 1)
	go func() {
		sr := recording.NewStreamReader(pr, nil)
		backend, err := recording.NewBackend("raster")
		if err != nil {
			done <- result{err: err}
			return
		}
		// Playback consumes frames as they arrive; it never builds a command
		// slice, so memory stays flat regardless of recording length.
		if err := sr.Playback(backend); err != nil {
			done <- result{err: err}
			return
		}
		totals := sr.Totals()
		// Persist the last rendered frame.
		if pb, ok := backend.(recording.PixmapBackend); ok {
			if pm := pb.Pixmap(); pm != nil {
				if err := pm.SavePNG("stream_output.png"); err != nil {
					log.Printf("save png: %v", err)
				}
			}
		}
		done <- result{commands: totals.Commands, frames: 1}
	}()

	// Producer: record frame by frame directly into the stream.
	rec := recording.NewRecorder(*width, *height)
	sw, err := rec.StreamTo(pw, nil)
	if err != nil {
		log.Fatalf("stream: %v", err)
	}

	sprite := makeSprite()
	for i := 0; i < *frames; i++ {
		drawFrame(rec, i, *frames, *width, *height, sprite)
		// Drop accumulated in-memory commands every few frames so the
		// producer side also stays bounded. The stream keeps everything.
		if i%8 == 7 {
			rec.Reset()
		}
	}
	if err := sw.Finish(); err != nil {
		log.Fatalf("finish: %v", err)
	}
	_ = pw.Close()

	res := <-done
	if res.err != nil {
		log.Fatalf("consumer: %v", res.err)
	}
	fmt.Printf("streamed %d commands from %d frames (%d bytes), wrote stream_output.png\n",
		res.commands, *frames, sw.BytesWritten())
}

// drawFrame records one animation frame: a moving circle, a rotating clipped
// image, and a label.
func drawFrame(rec *recording.Recorder, i, total, width, height int, sprite image.Image) {
	t := float64(i) / float64(total)
	rec.ClearWithColor(gg.RGBA{R: 0.08, G: 0.09, B: 0.12, A: 1})

	cx := float64(width) * (0.5 + 0.35*math.Sin(2*math.Pi*t))
	cy := float64(height) * (0.5 + 0.25*math.Sin(4*math.Pi*t))
	rec.SetFillRGBA(0.95, 0.35+0.4*t, 0.25, 1)
	rec.DrawCircle(cx, cy, 24)
	rec.Fill()

	rec.Push()
	rec.Translate(float64(width)/2, float64(height)/2)
	rec.Rotate(2 * math.Pi * t)
	rec.DrawRectangle(-60, -60, 120, 120)
	rec.Clip()
	rec.DrawImage(sprite, -60, -60)
	rec.Pop()
	rec.ResetClip()
}

// makeSprite creates a small placeholder sprite image.
func makeSprite() image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 8),
				G: uint8(y * 8),
				B: uint8((x ^ y) * 8),
				A: 255,
			})
		}
	}
	return img
}
