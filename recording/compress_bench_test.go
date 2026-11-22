package recording_test

import (
	"testing"

	"github.com/gogpu/gg/recording"
	"github.com/gogpu/gg/recording/backends/raster"
)

func BenchmarkCompressHUD(b *testing.B) {
	r := buildHUDRecording()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = r.Compress(recording.CompressOptions{Enabled: true})
	}
}

func BenchmarkHUDPlayback(b *testing.B) {
	original := buildHUDRecording()
	compressed, _ := original.Compress(recording.CompressOptions{Enabled: true})

	b.Run("original", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			backend := raster.NewBackend()
			if err := original.Playback(backend); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("compressed", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			backend := raster.NewBackend()
			if err := compressed.Playback(backend); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkCompressLongSequence(b *testing.B) {
	rec := recording.NewRecorder(400, 400)
	for i := range 5000 {
		rec.SetLineWidth(float64(i))
		rec.Translate(1, 0)
		if i%50 == 0 {
			rec.FillRectangle(0, 0, 5, 5)
		}
	}
	r := rec.FinishRecording()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = r.Compress(recording.CompressOptions{Enabled: true})
	}
}
