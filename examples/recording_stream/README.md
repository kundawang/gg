# Recording Stream Example

Demonstrates the incremental recording stream: a producer records animation
frames into an `io.Pipe` while a consumer decodes and renders the same bytes in
another goroutine, without either side holding the full recording in memory.

## What it does

1. Producer: `rec.StreamTo(pw, nil)` records a moving circle and a rotating
   clipped sprite, one frame at a time. Every 8 frames it calls `rec.Reset()`
   to drop accumulated in-memory commands — the stream retains everything.
2. Consumer: `recording.NewStreamReader(pr, nil)` decodes frames as they
   arrive and calls `sr.Playback(backend)`, which renders incrementally and
   never builds a command slice.
3. The last rendered frame is saved to `stream_output.png`.

In a real deployment the two ends would be separate processes connected by a
pipe, socket, or file.

## Running

```bash
go run .            # 24 frames (default)
go run . -frames 60 # more frames
```

Output: `stream_output.png`

## Key Concepts

### Producer: incremental recording

```go
sw, _ := rec.StreamTo(w, nil) // w is any io.Writer
// ... draw a frame ...
sw.Finish() // writes the end frame and whole-stream checksum
```

### Consumer: incremental playback

```go
sr := recording.NewStreamReader(r, nil)
sr.Playback(backend) // renders while the producer is still recording
```

### Consumer: assembled playback

```go
sr := recording.NewStreamReader(r, nil)
rec, _ := sr.ReadAll() // builds a complete Recording in memory
rec.Playback(backend)
```

## Wire format

Every frame carries a 16-byte header (magic, version, endianness, kind,
payload length, CRC-32C) followed by the payload. The first frame describes the
canvas; resource frames define paths, brushes, images, and fonts by explicit
ID; command frames carry one drawing command; the end frame carries resource
and command totals plus a running checksum over the whole stream.

Errors — truncation, unknown tags, oversized lengths, duplicate or unresolved
resources, an unfinished stream, cancellation, and underlying I/O failures —
are returned as `*recording.StreamError` with the chunk (frame) number, the
command number where applicable, and the byte offset.
