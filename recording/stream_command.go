package recording

// This file maps the sealed Command set onto wire-format payloads. Resource
// references are encoded as explicit numeric IDs rather than pool indices so
// a reader can resolve them the moment a command arrives, independent of the
// order in which the in-memory pool was built.

import (
	"fmt"

	"github.com/gogpu/gg"
	"github.com/gogpu/gg/text"
)

// commandTag is the wire tag for each CommandType. Tags are stable and must
// not be renumbered between stream versions.
type commandTag uint8

const (
	tagSave           commandTag = 0x01
	tagRestore        commandTag = 0x02
	tagSetTransform   commandTag = 0x03
	tagSetClip        commandTag = 0x04
	tagClearClip      commandTag = 0x05
	tagClipRoundRect  commandTag = 0x06
	tagFillPath       commandTag = 0x07
	tagStrokePath     commandTag = 0x08
	tagFillRect       commandTag = 0x09
	tagStrokeRect     commandTag = 0x0A
	tagDrawImage      commandTag = 0x0B
	tagDrawText       commandTag = 0x0C
	tagStrokeText     commandTag = 0x0D
	tagSetFillStyle   commandTag = 0x0E
	tagSetStrokeStyle commandTag = 0x0F
	tagSetLineWidth   commandTag = 0x10
	tagSetLineCap     commandTag = 0x11
	tagSetLineJoin    commandTag = 0x12
	tagSetMiterLimit  commandTag = 0x13
	tagSetDash        commandTag = 0x14
	tagSetFillRule    commandTag = 0x15
	tagSetAntiAlias   commandTag = 0x16
)

// commandTagNames names wire tags for error messages.
var commandTagNames = [...]string{
	tagSave:           "Save",
	tagRestore:        "Restore",
	tagSetTransform:   "SetTransform",
	tagSetClip:        "SetClip",
	tagClearClip:      "ClearClip",
	tagClipRoundRect:  "ClipRoundRect",
	tagFillPath:       "FillPath",
	tagStrokePath:     "StrokePath",
	tagFillRect:       "FillRect",
	tagStrokeRect:     "StrokeRect",
	tagDrawImage:      "DrawImage",
	tagDrawText:       "DrawText",
	tagStrokeText:     "StrokeText",
	tagSetFillStyle:   "SetFillStyle",
	tagSetStrokeStyle: "SetStrokeStyle",
	tagSetLineWidth:   "SetLineWidth",
	tagSetLineCap:     "SetLineCap",
	tagSetLineJoin:    "SetLineJoin",
	tagSetMiterLimit:  "SetMiterLimit",
	tagSetDash:        "SetDash",
	tagSetFillRule:    "SetFillRule",
	tagSetAntiAlias:   "SetAntiAlias",
}

// known reports whether the tag is defined.
func (t commandTag) known() bool {
	return int(t) < len(commandTagNames) && commandTagNames[t] != ""
}

// String returns the command name for a tag.
func (t commandTag) String() string {
	if t.known() {
		return commandTagNames[t]
	}
	return fmt.Sprintf("unknown(0x%02x)", uint8(t))
}

// commandTagFor maps a CommandType to its wire tag. The mapping is explicit so
// inserting a new CommandType in command.go cannot silently renumber wire tags
// and break compatibility with previously written streams.
func commandTagFor(t CommandType) (commandTag, bool) {
	switch t {
	case CmdSave:
		return tagSave, true
	case CmdRestore:
		return tagRestore, true
	case CmdSetTransform:
		return tagSetTransform, true
	case CmdSetClip:
		return tagSetClip, true
	case CmdClearClip:
		return tagClearClip, true
	case CmdClipRoundRect:
		return tagClipRoundRect, true
	case CmdFillPath:
		return tagFillPath, true
	case CmdStrokePath:
		return tagStrokePath, true
	case CmdFillRect:
		return tagFillRect, true
	case CmdStrokeRect:
		return tagStrokeRect, true
	case CmdDrawImage:
		return tagDrawImage, true
	case CmdDrawText:
		return tagDrawText, true
	case CmdStrokeText:
		return tagStrokeText, true
	case CmdSetFillStyle:
		return tagSetFillStyle, true
	case CmdSetStrokeStyle:
		return tagSetStrokeStyle, true
	case CmdSetLineWidth:
		return tagSetLineWidth, true
	case CmdSetLineCap:
		return tagSetLineCap, true
	case CmdSetLineJoin:
		return tagSetLineJoin, true
	case CmdSetMiterLimit:
		return tagSetMiterLimit, true
	case CmdSetDash:
		return tagSetDash, true
	case CmdSetFillRule:
		return tagSetFillRule, true
	case CmdSetAntiAlias:
		return tagSetAntiAlias, true
	default:
		return 0, false
	}
}

// encodeCommand appends a command payload: tag byte followed by its fields.
// References are translated from resource-pool indices to stream IDs through
// the encoder's cursor maps, so pool indices and stream IDs can diverge (for
// example after a Recorder.Reset restarts pool indices mid-stream).
func encodeCommand(e *encoder, cmd Command) error {
	if cmd == nil {
		return fmt.Errorf("%w: nil command", ErrUnsupportedCommand)
	}
	tag, ok := commandTagFor(cmd.Type())
	if !ok {
		return fmt.Errorf("%w: %T", ErrUnsupportedCommand, cmd)
	}
	e.u8(uint8(tag))

	err := func() error {
		switch c := cmd.(type) {
		case SaveCommand, RestoreCommand, ClearClipCommand:
		case SetTransformCommand:
			e.matrix(c.Matrix)
		case SetClipCommand:
			e.pathID(c.Path)
			e.u8(uint8(c.Rule))
		case ClipRoundRectCommand:
			e.f64(c.X)
			e.f64(c.Y)
			e.f64(c.W)
			e.f64(c.H)
			e.f64(c.Radius)
		case FillPathCommand:
			e.pathID(c.Path)
			e.brushID(c.Brush)
			e.u8(uint8(c.Rule))
		case StrokePathCommand:
			e.pathID(c.Path)
			e.brushID(c.Brush)
			e.stroke(c.Stroke)
		case FillRectCommand:
			e.rect(c.Rect)
			e.brushID(c.Brush)
		case StrokeRectCommand:
			e.rect(c.Rect)
			e.brushID(c.Brush)
			e.stroke(c.Stroke)
		case DrawImageCommand:
			e.imageID(c.Image)
			e.rect(c.SrcRect)
			e.rect(c.DstRect)
			e.imageOptions(c.Options)
		case DrawTextCommand:
			e.str(c.Text)
			e.f64(c.X)
			e.f64(c.Y)
			e.f64(c.FontSize)
			e.str(c.FontFamily)
			e.brushID(c.Brush)
			e.faceRef(c.Face)
		case StrokeTextCommand:
			e.str(c.Text)
			e.f64(c.X)
			e.f64(c.Y)
			e.f64(c.FontSize)
			e.str(c.FontFamily)
			e.brushID(c.Brush)
			e.faceRef(c.Face)
			e.stroke(c.Stroke)
		case SetFillStyleCommand:
			e.brushID(c.Brush)
		case SetStrokeStyleCommand:
			e.brushID(c.Brush)
		case SetLineWidthCommand:
			e.f64(c.Width)
		case SetLineCapCommand:
			e.u8(uint8(c.Cap))
		case SetLineJoinCommand:
			e.u8(uint8(c.Join))
		case SetMiterLimitCommand:
			e.f64(c.Limit)
		case SetDashCommand:
			e.uvarint(uint64(len(c.Pattern)))
			for _, d := range c.Pattern {
				e.f64(d)
			}
			e.f64(c.Offset)
		case SetFillRuleCommand:
			e.u8(uint8(c.Rule))
		case SetAntiAliasCommand:
			e.boolean(c.Enabled)
		default:
			return fmt.Errorf("%w: %T", ErrUnsupportedCommand, cmd)
		}
		return e.err
	}()
	return err
}

// imageOptions appends image rendering options.
func (e *encoder) imageOptions(o ImageOptions) {
	e.u8(uint8(o.Interpolation))
	e.f64(o.Alpha)
}

// faceRef appends a font resource ID, or InvalidRef for a nil face.
func (e *encoder) faceRef(face text.Face) {
	if face == nil {
		e.uvarint(invalidRefID)
		return
	}
	if id, ok := e.faceIDs[face]; ok {
		e.uvarint(uint64(id))
		return
	}
	// The writer registers every face before encoding the command that uses
	// it; reaching here means a command referenced an unregistered face.
	e.fail(fmt.Errorf("%w: face %T not registered", ErrInvalidData, face))
	e.uvarint(invalidRefID)
}

// decodeCommand parses one command payload, resolving resource references
// against pool as it goes. Unknown tags, out-of-range enums, unresolved
// references, and trailing bytes all produce errors.
func decodeCommand(d *decoder) (Command, error) {
	tag := commandTag(d.u8())
	if d.err != nil {
		return nil, d.err
	}
	if !tag.known() {
		return nil, fmt.Errorf("%w: command tag 0x%02x", ErrUnknownTag, uint8(tag))
	}

	var cmd Command
	switch tag {
	case tagSave:
		cmd = SaveCommand{}
	case tagRestore:
		cmd = RestoreCommand{}
	case tagClearClip:
		cmd = ClearClipCommand{}
	case tagSetTransform:
		cmd = SetTransformCommand{Matrix: d.matrix()}
	case tagSetClip:
		path, rule := d.pathRef(), d.fillRule()
		cmd = SetClipCommand{Path: path, Rule: rule}
	case tagClipRoundRect:
		cmd = ClipRoundRectCommand{
			X: d.f64(), Y: d.f64(), W: d.f64(), H: d.f64(), Radius: d.f64(),
		}
	case tagFillPath:
		path, brush, rule := d.pathRef(), d.brushRef(), d.fillRule()
		cmd = FillPathCommand{Path: path, Brush: brush, Rule: rule}
	case tagStrokePath:
		path, brush, st := d.pathRef(), d.brushRef(), d.stroke()
		cmd = StrokePathCommand{Path: path, Brush: brush, Stroke: st}
	case tagFillRect:
		rect, brush := d.rect(), d.brushRef()
		cmd = FillRectCommand{Rect: rect, Brush: brush}
	case tagStrokeRect:
		rect, brush, st := d.rect(), d.brushRef(), d.stroke()
		cmd = StrokeRectCommand{Rect: rect, Brush: brush, Stroke: st}
	case tagDrawImage:
		img := d.imageRef()
		src, dst := d.rect(), d.rect()
		opts := decodeImageOptions(d)
		cmd = DrawImageCommand{Image: img, SrcRect: src, DstRect: dst, Options: opts}
	case tagDrawText:
		s := d.str()
		x, y, size := d.f64(), d.f64(), d.f64()
		family := d.str()
		brush, face := d.brushRef(), d.faceRef()
		cmd = DrawTextCommand{
			Text: s, X: x, Y: y, FontSize: size, FontFamily: family,
			Face: face, Brush: brush,
		}
	case tagStrokeText:
		s := d.str()
		x, y, size := d.f64(), d.f64(), d.f64()
		family := d.str()
		brush, face := d.brushRef(), d.faceRef()
		st := d.stroke()
		cmd = StrokeTextCommand{
			Text: s, X: x, Y: y, FontSize: size, FontFamily: family,
			Face: face, Brush: brush, Stroke: st,
		}
	case tagSetFillStyle:
		cmd = SetFillStyleCommand{Brush: d.brushRef()}
	case tagSetStrokeStyle:
		cmd = SetStrokeStyleCommand{Brush: d.brushRef()}
	case tagSetLineWidth:
		cmd = SetLineWidthCommand{Width: d.f64()}
	case tagSetLineCap:
		cmd = SetLineCapCommand{Cap: LineCap(d.enum(int(LineCapSquare)))}
	case tagSetLineJoin:
		cmd = SetLineJoinCommand{Join: LineJoin(d.enum(int(LineJoinBevel)))}
	case tagSetMiterLimit:
		cmd = SetMiterLimitCommand{Limit: d.f64()}
	case tagSetDash:
		n := d.count(8)
		var pattern []float64
		if n > 0 && d.err == nil {
			pattern = make([]float64, n)
			for i := range pattern {
				pattern[i] = d.f64()
			}
		}
		cmd = SetDashCommand{Pattern: pattern, Offset: d.f64()}
	case tagSetFillRule:
		cmd = SetFillRuleCommand{Rule: FillRule(d.enum(int(FillRuleEvenOdd)))}
	case tagSetAntiAlias:
		cmd = SetAntiAliasCommand{Enabled: d.boolean()}
	}

	if d.err != nil {
		return nil, d.err
	}
	return cmd, nil
}

// pathRef reads a path reference and translates it to a pool index. A path
// must be defined before the command that uses it.
func (d *decoder) pathRef() PathRef {
	id := uint32(d.uvarint())
	if d.err != nil {
		return PathRef(id)
	}
	ref, ok := d.res.pathRefs[id]
	if !ok {
		d.fail(fmt.Errorf("%w: path %d", ErrUnresolvedResource, id))
		return PathRef(id)
	}
	return ref
}

// brushRef reads a brush reference and translates it to a pool index.
func (d *decoder) brushRef() BrushRef {
	id := uint32(d.uvarint())
	if d.err != nil {
		return BrushRef(id)
	}
	ref, ok := d.res.brushRefs[id]
	if !ok {
		d.fail(fmt.Errorf("%w: brush %d", ErrUnresolvedResource, id))
		return BrushRef(id)
	}
	return ref
}

// imageRef reads an image reference and translates it to a pool index.
func (d *decoder) imageRef() ImageRef {
	id := uint32(d.uvarint())
	if d.err != nil {
		return ImageRef(id)
	}
	ref, ok := d.res.imageRefs[id]
	if !ok {
		d.fail(fmt.Errorf("%w: image %d", ErrUnresolvedResource, id))
		return ImageRef(id)
	}
	return ref
}

// faceRef reads a font reference and resolves it. InvalidRef means no face.
func (d *decoder) faceRef() text.Face {
	ref := FontRef(uint32(d.uvarint()))
	if d.err != nil {
		return nil
	}
	if uint32(ref) == InvalidRef {
		return nil
	}
	face, ok := d.res.fonts[uint32(ref)]
	if !ok {
		d.fail(fmt.Errorf("%w: font %d", ErrUnresolvedResource, uint32(ref)))
		return nil
	}
	return face
}

// fillRule reads and validates a fill rule.
func (d *decoder) fillRule() FillRule {
	return FillRule(d.enum(int(FillRuleEvenOdd)))
}

// enum reads a one-byte enum and rejects values above max.
func (d *decoder) enum(max int) uint8 {
	v := d.u8()
	if d.err == nil && int(v) > max {
		d.fail(fmt.Errorf("%w: enum value %d > %d", ErrInvalidData, v, max))
	}
	return v
}

// decodeImageOptions reads image rendering options.
func decodeImageOptions(d *decoder) ImageOptions {
	interp := InterpolationMode(d.enum(int(InterpolationBilinear)))
	return ImageOptions{Interpolation: interp, Alpha: d.f64()}
}

// ensure gg is referenced for the path helpers used by the encoder's caller.
var _ = gg.NewPath
