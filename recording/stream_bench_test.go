package recording

// Benchmarks comparing streaming against in-memory recording and playback.

import (
	"bytes"
	"image"
	"testing"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
)

// benchRecording builds a recording with a mix of primitives, repeated.
func benchRecording(b *testing.B, ops int) *Recording {
	b.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	rec := NewRecorder(128, 128)
	for i := 0; i < ops; i++ {
		rec.Push()
		rec.Translate(float64(i%16), float64((i/16)%16))
		rec.SetFillRGBA(float64(i%7)/7, 0.3, 0.6, 1)
		rec.DrawRectangle(0, 0, 6, 6)
		rec.Fill()
		rec.DrawImage(img, 2, 2)
		rec.Pop()
	}
	return rec.FinishRecording()
}

func BenchmarkStreamWrite(b *testing.B) {
	final := benchRecording(b, 200)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		sw, err := final.StreamTo(&buf, nil)
		if err != nil {
			b.Fatal(err)
		}
		if err := sw.Finish(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamReadAll(b *testing.B) {
	final := benchRecording(b, 200)
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()
	data := buf.Bytes()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sr := NewStreamReader(bytes.NewReader(data), nil)
		if _, err := sr.ReadAll(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamNextOnly(b *testing.B) {
	final := benchRecording(b, 200)
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()
	data := buf.Bytes()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sr := NewStreamReader(bytes.NewReader(data), nil)
		for {
			_, err := sr.Next()
			if err != nil {
				break
			}
		}
	}
}

// BenchmarkPlaybackInMemory plays back a fully materialized recording.
func BenchmarkPlaybackInMemory(b *testing.B) {
	final := benchRecording(b, 200)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := final.Playback(&nullBackend{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStreamPlayback decodes and replays a stream incrementally.
func BenchmarkStreamPlayback(b *testing.B) {
	final := benchRecording(b, 200)
	var buf bytes.Buffer
	sw, _ := final.StreamTo(&buf, nil)
	_ = sw.Finish()
	data := buf.Bytes()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sr := NewStreamReader(bytes.NewReader(data), nil)
		if err := sr.Playback(&nullBackend{}); err != nil {
			b.Fatal(err)
		}
	}
}

// nullBackend is a no-op Backend used to isolate decode/playback cost from
// rasterization.
type nullBackend struct{}

func (*nullBackend) Begin(int, int) error                                { return nil }
func (*nullBackend) End() error                                          { return nil }
func (*nullBackend) Save()                                               {}
func (*nullBackend) Restore()                                            {}
func (*nullBackend) SetTransform(Matrix)                                 {}
func (*nullBackend) SetClip(*gg.Path, FillRule)                          {}
func (*nullBackend) ClearClip()                                          {}
func (*nullBackend) FillPath(*gg.Path, Brush, FillRule)                  {}
func (*nullBackend) StrokePath(*gg.Path, Brush, Stroke)                  {}
func (*nullBackend) FillRect(Rect, Brush)                                {}
func (*nullBackend) DrawImage(image.Image, Rect, Rect, ImageOptions)     {}
func (*nullBackend) DrawText(string, float64, float64, text.Face, Brush) {}
