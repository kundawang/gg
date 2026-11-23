package recording

// This file adds incremental playback: replay a stream to a Backend as
// commands arrive, without ever materializing the whole recording. This is the
// consumer side of the long-recording pipeline — a second process can decode
// and render while the producer is still recording.

import (
	"errors"
	"io"

	"github.com/gogpu/gg"
)

// streamPlayer applies decoded commands to a backend as they arrive. It holds
// only the backend's own state plus the current transform stack, so memory is
// constant regardless of stream length.
type streamPlayer struct {
	backend Backend
	width   int
	height  int

	transform      Matrix
	transformStack []Matrix
	begun          bool
}

// apply replays a single command. Resources were registered by the reader
// before the command was decoded, so lookups always succeed.
func (p *streamPlayer) apply(cmd Command, resources *ResourcePool) {
	switch c := cmd.(type) {
	case SaveCommand:
		p.transformStack = append(p.transformStack, p.transform)
		p.backend.Save()
	case RestoreCommand:
		p.backend.Restore()
		if len(p.transformStack) > 0 {
			p.transform = p.transformStack[len(p.transformStack)-1]
			p.transformStack = p.transformStack[:len(p.transformStack)-1]
		}
	case SetTransformCommand:
		p.transform = c.Matrix
		p.backend.SetTransform(c.Matrix)
	case SetClipCommand:
		setPlaybackClip(p.backend, resources.GetPath(c.Path), c.Rule, p.transform)
	case ClearClipCommand:
		p.backend.ClearClip()
	case ClipRoundRectCommand:
		setPlaybackClip(p.backend, buildClipRoundRectPath(c, p.transform), FillRuleNonZero, p.transform)
	case FillPathCommand:
		p.backend.FillPath(resources.GetPath(c.Path), resources.GetBrush(c.Brush), c.Rule)
	case StrokePathCommand:
		p.backend.StrokePath(resources.GetPath(c.Path), resources.GetBrush(c.Brush), c.Stroke)
	case FillRectCommand:
		p.backend.FillRect(c.Rect, resources.GetBrush(c.Brush))
	case StrokeRectCommand:
		path := buildRectPath(c.Rect)
		p.backend.Save()
		p.backend.SetTransform(Identity())
		p.backend.StrokePath(path, resources.GetBrush(c.Brush), c.Stroke)
		p.backend.Restore()
	case DrawImageCommand:
		p.backend.DrawImage(resources.GetImage(c.Image), c.SrcRect, c.DstRect, c.Options)
	case DrawTextCommand:
		p.backend.DrawText(c.Text, c.X, c.Y, c.Face, resources.GetBrush(c.Brush))
	case StrokeTextCommand:
		p.backend.DrawText(c.Text, c.X, c.Y, c.Face, resources.GetBrush(c.Brush))
	case SetFillStyleCommand, SetStrokeStyleCommand,
		SetLineWidthCommand, SetLineCapCommand, SetLineJoinCommand,
		SetMiterLimitCommand, SetDashCommand, SetFillRuleCommand:
		// State is captured in the drawing commands themselves.
	}
}

// Playback consumes the whole stream and replays it to backend as it goes.
// Memory use is proportional to the backend's own surface and to the in-memory
// resource pool, not to the number of commands. The backend's End is called
// before Playback returns.
func (sr *StreamReader) Playback(backend Backend) error {
	if sr.err != nil {
		return sr.err
	}
	player := &streamPlayer{backend: backend, transform: Identity(), transformStack: make([]Matrix, 0, 8)}
	for {
		cmd, err := sr.Next()
		if !player.begun && sr.canvasSeen {
			if berr := backend.Begin(sr.width, sr.height); berr != nil {
				return berr
			}
			player.begun = true
		}
		if errors.Is(err, io.EOF) {
			if !sr.ended {
				return sr.err
			}
			break
		}
		if err != nil {
			return err
		}
		player.apply(cmd, sr.pool)
	}
	if !player.begun {
		// A stream with no commands still needs Begin/End.
		if err := backend.Begin(sr.width, sr.height); err != nil {
			return err
		}
	}
	return backend.End()
}

// buildRectPath builds an axis-aligned rectangle path matching the one
// Recording.Playback constructs for StrokeRectCommand.
func buildRectPath(rect Rect) *gg.Path {
	return gg.BuildPath().Rect(rect.MinX, rect.MinY, rect.Width(), rect.Height()).Build()
}
