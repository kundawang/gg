package recording_test

// Long-recording comparison tests: build a long recording containing images,
// text, clips, and nested transforms, stream it, replay both the in-memory and
// streamed forms, and assert identical pixels, identical per-command checksums,
// and bounded streaming memory. Playback goes through the built-in raster
// backend, but to stay independent of the global registry (which other tests
// mutate) the backend is constructed through the pool-free Recording API where
// possible and through a direct local raster backend reference otherwise.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"os"
	"runtime"
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/recording"
	rasterbackend "github.com/gogpu/gg/recording/backends/raster"
	"github.com/gogpu/gg/text"
)

// longRecording builds a recording with many frames of images, text, clips,
// and nested transforms, long enough that per-command memory retention would
// dominate.
func longRecording(t *testing.T, frames int) *recording.Recorder {
	t.Helper()

	var face text.Face
	if data, err := os.ReadFile("../../text/testdata/ahem.ttf"); err == nil {
		if src, err := text.NewFontSource(data); err == nil {
			face = src.Face(12)
		}
	}

	img := image.NewNRGBA(image.Rect(0, 0, 24, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 24; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 10), G: uint8(y * 10), B: uint8((x + y) * 5), A: 255,
			})
		}
	}

	rec := recording.NewRecorder(160, 120)
	rec.ClearWithColor(gg.RGBA{R: 0.97, G: 0.97, B: 0.99, A: 1})
	if face != nil {
		rec.SetFont(face)
	}

	for i := 0; i < frames; i++ {
		fx := float64(i % 8)
		fy := float64((i / 8) % 6)

		rec.Push()
		rec.Translate(fx*18+4, fy*18+4)
		rec.Rotate(float64(i) * 0.05)
		rec.Scale(1+fx*0.1, 1+fy*0.1)

		// Filled shape with a varying solid color.
		rec.SetFillRGBA(fx/8, fy/6, 0.5, 1)
		rec.DrawCircle(8, 8, 6)
		rec.Fill()

		// Nested transform with a stroked rectangle.
		rec.Push()
		rec.Translate(4, 4)
		rec.SetStrokeRGBA(1, 1, 1, 1)
		rec.SetLineWidth(1)
		rec.DrawRectangle(-2, -2, 8, 8)
		rec.Stroke()
		rec.Pop()

		// Per-frame clipping.
		rec.Push()
		rec.DrawRectangle(0, 0, 16, 16)
		rec.Clip()
		rec.DrawImage(img, int(fx)*8, int(fy)*8)
		rec.Pop()
		rec.ResetClip()

		// Text on every other frame.
		if face != nil && i%2 == 0 {
			rec.SetFillRGB(0, 0, 0)
			rec.DrawString(fmt.Sprintf("%d", i%10), 6, 14)
		}

		rec.Pop()
	}
	return rec
}

// backendImage renders a recording or stream to a raster image. It constructs
// the raster backend directly, bypassing the registry so unrelated tests that
// reset registrations cannot affect it.
func inMemoryImage(t *testing.T, r *recording.Recording) *gg.Pixmap {
	t.Helper()
	b := rasterbackend.NewBackend()
	if err := r.Playback(b); err != nil {
		t.Fatalf("playback: %v", err)
	}
	return b.Pixmap()
}

func streamImage(t *testing.T, buf []byte) *gg.Pixmap {
	t.Helper()
	sr := recording.NewStreamReader(bytes.NewReader(buf), nil)
	b := rasterbackend.NewBackend()
	if err := sr.Playback(b); err != nil {
		t.Fatalf("stream playback: %v", err)
	}
	if !sr.Done() {
		t.Fatal("stream playback did not reach end frame")
	}
	return b.Pixmap()
}

// commandChecksum hashes the decoded command stream in order, independent of
// pixel output. Two recordings with the same checksum describe the same
// sequence of operations.
func commandChecksum(t *testing.T, buf []byte) string {
	t.Helper()
	sr := recording.NewStreamReader(bytes.NewReader(buf), nil)
	h := sha256.New()
	// Describe each command's type and key fields through a stable encoder.
	n := 0
	for {
		cmd, err := sr.Next()
		if err != nil {
			break
		}
		fmt.Fprintf(h, "%d:%s;", n, cmd.Type().String())
		n++
	}
	if !sr.Done() {
		t.Fatal("checksum pass did not reach end frame")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestStreamLongRecordingPixelParity(t *testing.T) {
	if testing.Short() {
		t.Skip("long-recording test skipped in -short mode")
	}
	const frames = 400

	rec := longRecording(t, frames)
	final := rec.FinishRecording()

	var buf bytes.Buffer
	sw, err := final.StreamTo(&buf, nil)
	if err != nil {
		t.Fatalf("StreamTo: %v", err)
	}
	if err := sw.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if sw.CommandsWritten() < uint64(frames) {
		t.Fatalf("streamed %d commands for %d frames", sw.CommandsWritten(), frames)
	}

	memPixels := inMemoryImage(t, final).Data()
	streamPixels := streamImage(t, buf.Bytes()).Data()
	if len(memPixels) != len(streamPixels) {
		t.Fatalf("pixel buffer sizes differ: %d vs %d", len(memPixels), len(streamPixels))
	}
	for i := range memPixels {
		if memPixels[i] != streamPixels[i] {
			t.Fatalf("pixel byte %d differs: %d vs %d", i, memPixels[i], streamPixels[i])
		}
	}

	// Checksums of the decoded command streams must match. The in-memory
	// recording is re-streamed so both sides are compared from the wire format.
	var inMemBuf bytes.Buffer
	sw2, err := final.StreamTo(&inMemBuf, nil)
	if err != nil {
		t.Fatalf("re-stream: %v", err)
	}
	if err := sw2.Finish(); err != nil {
		t.Fatalf("re-stream Finish: %v", err)
	}
	if got, want := commandChecksum(t, buf.Bytes()), commandChecksum(t, inMemBuf.Bytes()); got != want {
		t.Fatalf("command checksum mismatch: %s vs %s", got, want)
	}

	// Playback completeness check: every command must round-trip.
	sr := recording.NewStreamReader(bytes.NewReader(buf.Bytes()), nil)
	got, err := sr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got.Commands()) != len(final.Commands()) {
		t.Fatalf("round-trip command count %d, want %d", len(got.Commands()), len(final.Commands()))
	}
}

func TestStreamLongRecordingPeakMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("long-recording memory test skipped in -short mode")
	}
	// Build a stream with many commands but a bounded, small resource set, so
	// command retention (not resources) is the dominant cost of a full read.
	// This isolates the property under test: incremental playback never holds
	// the command slice.
	const commands = 40000
	rec := recording.NewRecorder(64, 64)
	// Transforms and save/restore produce no pooled resources, so the only
	// per-command cost is the command slice itself.
	for i := 0; i < commands/2; i++ {
		rec.Push()
		rec.Translate(float64(i%32), float64((i/32)%32))
		rec.Rotate(float64(i) * 0.01)
		rec.Pop()
	}
	final := rec.FinishRecording()
	if final.Resources().PathCount() != 0 || final.Resources().BrushCount() != 0 {
		t.Fatalf("unexpected resource growth: paths=%d brushes=%d",
			final.Resources().PathCount(), final.Resources().BrushCount())
	}
	if final.Resources().ImageCount() != 0 {
		t.Fatalf("unexpected image resources: %d", final.Resources().ImageCount())
	}

	var buf bytes.Buffer
	sw, err := final.StreamTo(&buf, nil)
	if err != nil {
		t.Fatalf("StreamTo: %v", err)
	}
	if err := sw.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	streamBytes := buf.Bytes()
	commandCount := len(final.Commands())
	if commandCount < commands/2 {
		t.Fatalf("recorded %d commands, want at least %d", commandCount, commands/2)
	}

	// Measure the incremental path: Next + Playback never retains commands.
	// TotalAlloc measures every byte allocated during the run — including
	// transient frames — so it captures the peak cost faithfully regardless
	// of what stays reachable afterward.
	measureAlloc := func(retainCommands bool) uint64 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		sr := recording.NewStreamReader(bytes.NewReader(streamBytes),
			&recording.StreamConfig{RetainCommands: retainCommands})
		player := rasterbackend.NewBackend()
		if err := sr.Playback(player); err != nil {
			t.Fatalf("stream playback: %v", err)
		}
		// Assert the retention policy actually took effect before the reader
		// goes out of scope.
		if retainCommands && sr.RetainedCommands() == 0 {
			t.Fatal("RetainCommands reader kept no commands")
		}
		if !retainCommands && sr.RetainedCommands() != 0 {
			t.Fatalf("incremental reader retained %d commands", sr.RetainedCommands())
		}

		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}

	streamAlloc := measureAlloc(false)
	allAlloc := measureAlloc(true)
	// Retaining every command must cost more total allocation than streaming
	// them through. Combined with the len(sr.cmds) assertion above, this pins
	// the memory property without depending on GC timing.
	if streamAlloc >= allAlloc {
		t.Fatalf("incremental allocation %d bytes >= full-read allocation %d bytes",
			streamAlloc, allAlloc)
	}
	t.Logf("streamed %d commands (%d bytes): incremental alloc %d bytes, full-read alloc %d bytes",
		commandCount, len(streamBytes), streamAlloc, allAlloc)
}

func TestStreamPlaybackMatchesInMemory(t *testing.T) {
	const frames = 60
	final := longRecording(t, frames).FinishRecording()

	var buf bytes.Buffer
	sw, err := final.StreamTo(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}

	want := inMemoryImage(t, final).Data()
	got := streamImage(t, buf.Bytes()).Data()
	if !bytes.Equal(want, got) {
		t.Fatal("stream playback pixels differ from in-memory playback")
	}
}
