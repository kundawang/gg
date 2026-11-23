package recording

// This file exposes streaming from a finished Recording (already in memory)
// and the helpers that pair a Recorder with a stream.

import (
	"io"
)

// StreamTo writes a finished Recording to w as a stream, returning the writer
// so the caller can seal it with Finish. This is the counterpart to
// Recorder.StreamTo for recordings already materialized in memory; the
// command order and resource IDs match Playback exactly.
func (r *Recording) StreamTo(w io.Writer, cfg *StreamConfig) (*StreamWriter, error) {
	sw := NewStreamWriter(w, cfg)
	if sw.err != nil {
		return nil, sw.err
	}
	if err := sw.attachSize(r.width, r.height); err != nil {
		return nil, err
	}
	if err := sw.emitAllResources(r.resources); err != nil {
		return nil, err
	}
	for _, cmd := range r.commands {
		if err := sw.emitCommand(cmd); err != nil {
			return nil, err
		}
	}
	return sw, nil
}

// emitAllResources writes every resource in a pool in dependency order:
// images (referenced by pattern brushes), brushes, paths, then fonts.
func (w *StreamWriter) emitAllResources(pool *ResourcePool) error {
	for idx, img := range pool.images {
		// #nosec G115 -- pool sizes are bounded well below uint32 max
		if err := w.emitImage(uint32(idx), img); err != nil {
			return err
		}
	}
	for idx, brush := range pool.brushes {
		// #nosec G115 -- pool sizes are bounded well below uint32 max
		if err := w.emitBrush(uint32(idx), brush); err != nil {
			return err
		}
	}
	for idx, path := range pool.paths {
		// #nosec G115 -- pool sizes are bounded well below uint32 max
		if err := w.emitPath(uint32(idx), path); err != nil {
			return err
		}
	}
	for _, face := range pool.fonts {
		if err := w.emitFont(face); err != nil {
			return err
		}
	}
	return nil
}
