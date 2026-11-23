package scene

import (
	"encoding/binary"
	"math"

	"github.com/gogpu/gg"
)

// FilterSpec is a serializable description of a layer's filter chain.
// It is the public input to filter-aware signatures: two layers with equal
// specs produce equal FilterHash values, so changing any blur radius or
// shadow offset changes the signature and forces a rebuild.
//
// Type-specific parameters:
//   - Blur:        Params[0] = sigma (px)
//   - DropShadow:  Params = [offsetX, offsetY, blurSigma], RGBA = shadow color
//   - ColorMatrix: Matrix is a 20-float RGBA color matrix (row-major)
type FilterSpec struct {
	Type   FilterType
	Params [4]float32
	RGBA   gg.RGBA
	Matrix [20]float32
}

// Hash folds the spec into the running FNV-1a hash.
func (f FilterSpec) Hash(h uint64) uint64 {
	const prime = 1099511628211
	feed := func(v uint64) {
		h ^= v
		h *= prime
	}
	feed(uint64(f.Type))
	for _, p := range f.Params {
		feed(uint64(math.Float32bits(p)))
	}
	feed(math.Float64bits(f.RGBA.R))
	feed(math.Float64bits(f.RGBA.G))
	feed(math.Float64bits(f.RGBA.B))
	feed(math.Float64bits(f.RGBA.A))
	for _, m := range f.Matrix {
		feed(uint64(math.Float32bits(m)))
	}
	return h
}

// filterHashOf folds a filter chain description into a signature component.
// A nil/empty chain hashes to zero (unfiltered layer).
func filterHashOf(specs []FilterSpec) uint64 {
	if len(specs) == 0 {
		return 0
	}
	const offset = 14695981039346656037
	h := uint64(offset)
	for i := range specs {
		h = specs[i].Hash(h)
	}
	return h
}

// layerSegment describes one top-level PushLayer block found in a flattened
// encoding.
type layerSegment struct {
	// tagStart/tagEnd are the inclusive index range in enc.tags, from
	// TagPushLayer through the matching TagPopLayer.
	tagStart int
	tagEnd   int

	// blend/alpha carried by the PushLayer command.
	blend BlendMode
	alpha float32

	// hasClip is true when the block wraps content in BeginClip/EndClip.
	hasClip bool
}

// streamCursor records how many entries each parallel stream has consumed.
type streamCursor struct {
	path int
	draw int
	tr   int
	text int
}

// advanceStream moves the cursor past the data consumed by one tag.
// It mirrors the consumption contract of the Decoder.
func advanceStream(tag Tag, cur *streamCursor, textData []byte) {
	switch tag {
	case TagMoveTo, TagLineTo:
		cur.path += 2
	case TagQuadTo:
		cur.path += 4
	case TagCubicTo:
		cur.path += 6
	case TagBrush:
		cur.path += 4
	case TagFillRoundRect:
		cur.path += 6
		cur.draw += 2
	case TagFill:
		cur.draw += 2
	case TagStroke:
		cur.draw += 5
	case TagPushLayer:
		cur.draw += 2
	case TagSetAntiAlias:
		cur.draw++
	case TagImage:
		cur.draw++
		cur.tr++
	case TagText:
		if cur.text+glyphRunDataSize <= len(textData) {
			glyphCount := int(binary.LittleEndian.Uint16(textData[cur.text+12:]))
			textLen := int(binary.LittleEndian.Uint16(textData[cur.text+28:]))
			cur.text += glyphRunDataSize + glyphCount*glyphEntrySize + textLen
		}
	}
}

// segmentLayers walks a flattened encoding and returns the top-level layer
// blocks in stream order. Nested PushLayer commands inside a block are part
// of the block's content (balanced depth tracking).
func segmentLayers(enc *Encoding) []layerSegment {
	if enc == nil || len(enc.tags) == 0 {
		return nil
	}
	var segments []layerSegment
	cur := streamCursor{}
	depth := 0
	var seg layerSegment
	clipDepth := 0

	for i, tag := range enc.tags {
		switch tag {
		case TagPushLayer:
			if depth == 0 {
				seg = layerSegment{tagStart: i}
				if cur.draw+1 < len(enc.drawData) {
					seg.blend = BlendMode(enc.drawData[cur.draw])
					seg.alpha = math.Float32frombits(enc.drawData[cur.draw+1])
				}
			}
			depth++
		case TagBeginClip:
			if depth > 0 {
				clipDepth++
				seg.hasClip = true
			}
		case TagEndClip:
			if depth > 0 && clipDepth > 0 {
				clipDepth--
			}
		case TagPopLayer:
			depth--
			if depth == 0 {
				seg.tagEnd = i
				segments = append(segments, seg)
			}
		}
		advanceStream(tag, &cur, enc.textData)
	}
	return segments
}

// buildSubEncoding constructs a standalone Encoding from an inclusive tag
// range of enc. All four parallel streams are sliced with the same cursor
// contract as segmentLayers, and brush indices are remapped to local
// indices so the result is independently decodable.
func buildSubEncoding(enc *Encoding, start, end int) *Encoding {
	out := NewEncoding()
	if enc == nil || start < 0 || end >= len(enc.tags) || start > end {
		return out
	}

	// Stream offsets at the slice boundaries.
	cur := streamCursor{}
	for i := 0; i < start; i++ {
		advanceStream(enc.tags[i], &cur, enc.textData)
	}
	pathStart, drawStart, trStart, textStart := cur.path, cur.draw, cur.tr, cur.text

	cur = streamCursor{}
	for i := 0; i <= end; i++ {
		advanceStream(enc.tags[i], &cur, enc.textData)
	}
	pathEnd, drawEnd, trEnd, textEnd := cur.path, cur.draw, cur.tr, cur.text

	out.tags = append(out.tags, enc.tags[start:end+1]...)
	out.pathData = append(out.pathData, enc.pathData[pathStart:pathEnd]...)
	out.drawData = append(out.drawData, enc.drawData[drawStart:drawEnd]...)
	out.transforms = append(out.transforms, enc.transforms[trStart:trEnd]...)
	out.textData = append(out.textData, enc.textData[textStart:textEnd]...)

	remapBrushes(enc, out)
	out.bounds = computeEncodingBounds(out, nil)
	return out
}

// buildRootEncoding builds an encoding containing only the tags outside all
// top-level layer blocks (the root-level content), with remapped brushes.
func buildRootEncoding(enc *Encoding, segments []layerSegment) *Encoding {
	if len(segments) == 0 {
		return enc.Clone()
	}
	keep := make([]bool, len(enc.tags))
	for i := range keep {
		keep[i] = true
	}
	for _, seg := range segments {
		for i := seg.tagStart; i <= seg.tagEnd; i++ {
			keep[i] = false
		}
	}
	return buildEncodingWithMask(enc, keep)
}

// buildEncodingWithMask constructs an Encoding keeping only tags whose mask
// entry is true, slicing all parallel streams consistently and remapping
// brushes.
func buildEncodingWithMask(enc *Encoding, keep []bool) *Encoding {
	out := NewEncoding()
	cur := streamCursor{}
	brushMap := make(map[uint32]uint32)
	remap := func(old uint32) uint32 {
		if idx, ok := brushMap[old]; ok {
			return idx
		}
		idx := uint32(len(out.brushes))
		brushMap[old] = idx
		if int(old) < len(enc.brushes) {
			out.brushes = append(out.brushes, enc.brushes[old])
		} else {
			out.brushes = append(out.brushes, Brush{})
		}
		return idx
	}

	for i, tag := range enc.tags {
		p0, d0, t0, x0 := cur.path, cur.draw, cur.tr, cur.text
		advanceStream(tag, &cur, enc.textData)
		if !keep[i] {
			continue
		}

		out.tags = append(out.tags, tag)
		out.pathData = append(out.pathData, enc.pathData[p0:cur.path]...)
		out.drawData = append(out.drawData, enc.drawData[d0:cur.draw]...)
		out.transforms = append(out.transforms, enc.transforms[t0:cur.tr]...)
		out.textData = append(out.textData, enc.textData[x0:cur.text]...)

		switch tag {
		case TagFill, TagFillRoundRect, TagStroke:
			// First uint32 of this tag's draw tail is the brush index.
			tailBase := len(out.drawData) - (cur.draw - d0)
			out.drawData[tailBase] = remap(out.drawData[tailBase])
		case TagText:
			// BrushIndex is at byte offset 24 in the GlyphRunData header.
			tailBase := len(out.textData) - (cur.text - x0)
			off := tailBase + 24
			if off+4 <= len(out.textData) {
				binary.LittleEndian.PutUint32(out.textData[off:],
					remap(binary.LittleEndian.Uint32(out.textData[off:])))
			}
		}
	}
	out.bounds = computeEncodingBounds(out, nil)
	return out
}

// remapBrushes rewrites brush references in a sub-encoding produced by raw
// stream slicing, copying referenced brushes into a local brush table.
func remapBrushes(src, out *Encoding) {
	brushMap := make(map[uint32]uint32)
	remap := func(old uint32) uint32 {
		if idx, ok := brushMap[old]; ok {
			return idx
		}
		idx := uint32(len(out.brushes))
		brushMap[old] = idx
		if int(old) < len(src.brushes) {
			out.brushes = append(out.brushes, src.brushes[old])
		} else {
			out.brushes = append(out.brushes, Brush{})
		}
		return idx
	}

	cur := streamCursor{}
	for i, tag := range out.tags {
		switch tag {
		case TagFill, TagFillRoundRect, TagStroke:
			out.drawData[cur.draw] = remap(out.drawData[cur.draw])
		case TagText:
			off := cur.text + 24
			if off+4 <= len(out.textData) {
				binary.LittleEndian.PutUint32(out.textData[off:],
					remap(binary.LittleEndian.Uint32(out.textData[off:])))
			}
		}
		advanceStream(tag, &cur, out.textData)
		_ = i
	}
}

// computeEncodingBounds derives a conservative canvas-space bounding rect
// from command data. It covers paths, round rects, images (when the registry
// is available), and text glyph cells. Stroke width and a 1px AA margin are
// applied by the caller when sizing the offscreen buffer.
func computeEncodingBounds(enc *Encoding, images []*Image) Rect {
	b := EmptyRect()
	dec := NewDecoder(enc)
	currentTransform := IdentityAffine()
	var pathBounds Rect
	var havePath bool

	for dec.Next() {
		switch dec.Tag() {
		case TagTransform:
			currentTransform = dec.Transform()

		case TagBeginPath:
			pathBounds = EmptyRect()
			havePath = true

		case TagMoveTo:
			x, y := dec.MoveTo()
			tx, ty := currentTransform.TransformPoint(x, y)
			pathBounds = pathBounds.UnionPoint(tx, ty)

		case TagLineTo:
			x, y := dec.LineTo()
			tx, ty := currentTransform.TransformPoint(x, y)
			pathBounds = pathBounds.UnionPoint(tx, ty)

		case TagQuadTo:
			cx, cy, x, y := dec.QuadTo()
			tcx, tcy := currentTransform.TransformPoint(cx, cy)
			tx, ty := currentTransform.TransformPoint(x, y)
			pathBounds = pathBounds.UnionPoint(tcx, tcy).UnionPoint(tx, ty)

		case TagCubicTo:
			c1x, c1y, c2x, c2y, x, y := dec.CubicTo()
			p1x, p1y := currentTransform.TransformPoint(c1x, c1y)
			p2x, p2y := currentTransform.TransformPoint(c2x, c2y)
			px, py := currentTransform.TransformPoint(x, y)
			pathBounds = pathBounds.UnionPoint(p1x, p1y).UnionPoint(p2x, p2y).UnionPoint(px, py)

		case TagFill:
			_, _ = dec.Fill()
			if havePath {
				b = b.Union(pathBounds)
			}

		case TagStroke:
			_, style := dec.Stroke()
			if havePath {
				hw := style.Width / 2
				b = b.Union(Rect{
					MinX: pathBounds.MinX - hw,
					MinY: pathBounds.MinY - hw,
					MaxX: pathBounds.MaxX + hw,
					MaxY: pathBounds.MaxY + hw,
				})
			}

		case TagFillRoundRect:
			_, _, rect, _, _ := dec.FillRoundRect()
			b = b.Union(transformBounds(rect, currentTransform))

		case TagImage:
			idx, tr := dec.Image()
			iw, ih := float32(1), float32(1)
			if int(idx) < len(images) && images[idx] != nil {
				iw = float32(images[idx].Width)
				ih = float32(images[idx].Height)
			}
			corners := [4][2]float32{{0, 0}, {iw, 0}, {iw, ih}, {0, ih}}
			for _, c := range corners {
				x, y := tr.TransformPoint(c[0], c[1])
				b = b.UnionPoint(x, y)
			}

		case TagText:
			run, glyphs, _, _ := dec.Text()
			fs := run.FontSize
			for _, g := range glyphs {
				x, y := currentTransform.TransformPoint(g.X+run.OriginX, g.Y+run.OriginY)
				b = b.Union(Rect{
					MinX: x - fs*0.5,
					MinY: y - fs*0.9,
					MaxX: x + fs*0.9,
					MaxY: y + fs*0.3,
				})
			}
		}
	}
	return b
}

// renderEncodingToPixmap rasterizes a standalone encoding into a tightly
// bounded offscreen pixmap. It reuses the Renderer's pooled resources and
// the exact same tile command interpreter as the full-render path, so a
// cache miss produces pixels identical to full rendering.
//
// The returned pixmap's pixel (0,0) corresponds to canvas coordinate
// (originX, originY). At non-unit device scale the layer is first
// rasterized at scale 1 and then resampled; the result is stored under a
// scale-specific key so different device scales are never mixed.
func (r *Renderer) renderEncodingToPixmap(enc *Encoding, images []*Image, scale float32) (*gg.Pixmap, int, int, Rect) {
	b := computeEncodingBounds(enc, images)
	if b.IsEmpty() {
		b = enc.Bounds()
	}
	if b.IsEmpty() {
		return nil, 0, 0, EmptyRect()
	}

	// Integer pixel rect in canvas space, with 1px AA margin.
	ox := max(0, int(math.Floor(float64(b.MinX)))-1)
	oy := max(0, int(math.Floor(float64(b.MinY)))-1)
	ex := min(r.width, int(math.Ceil(float64(b.MaxX)))+1)
	ey := min(r.height, int(math.Ceil(float64(b.MaxY)))+1)
	w, h := ex-ox, ey-oy
	if w <= 0 || h <= 0 {
		return nil, 0, 0, EmptyRect()
	}

	sr := r.pool.getRenderer(w, h)
	defer r.pool.putRenderer(sr)

	if scale == 1 {
		pm := gg.NewPixmap(w, h)
		inner := r.pool.getDecoder(enc)
		r.executeEncodingAtOrigin(inner, pm, sr, images, ox, oy)
		r.pool.putDecoder(inner)
		return pm, ox, oy, boundsFromOrigin(ox, oy, w, h)
	}

	// Non-unit scale: rasterize at logical resolution, then resample.
	logical := gg.NewPixmap(w, h)
	inner := r.pool.getDecoder(enc)
	r.executeEncodingAtOrigin(inner, logical, sr, images, ox, oy)
	r.pool.putDecoder(inner)

	scaledW := max(1, int(math.Round(float64(w)*float64(scale))))
	scaledH := max(1, int(math.Round(float64(h)*float64(scale))))
	pm := gg.NewPixmap(scaledW, scaledH)
	resampleNearest(logical, pm)
	return pm, ox, oy, boundsFromOrigin(ox, oy, w, h)
}

func boundsFromOrigin(ox, oy, w, h int) Rect {
	return Rect{
		MinX: float32(ox), MinY: float32(oy),
		MaxX: float32(ox + w), MaxY: float32(oy + h),
	}
}

// resampleNearest scales src into dst using nearest-neighbor sampling in
// premultiplied space. Device-scale changes are rare; each scale keeps its
// own cache signature so this only runs on a true scale change.
func resampleNearest(src, dst *gg.Pixmap) {
	sw, sh := src.Width(), src.Height()
	dw, dh := dst.Width(), dst.Height()
	srcData, dstData := src.Data(), dst.Data()
	for dy := 0; dy < dh; dy++ {
		sy := dy * sh / dh
		for dx := 0; dx < dw; dx++ {
			sx := dx * sw / dw
			srcOff := (sy*sw + sx) * 4
			dstOff := (dy*dw + dx) * 4
			copy(dstData[dstOff:dstOff+4], srcData[srcOff:srcOff+4])
		}
	}
}

// layerContentHash computes the signature hash for a layer sub-encoding.
// The encoding's FNV hash is mixed with blend/alpha/clip flags and the
// modification versions of every referenced image, so in-place pixel
// mutation invalidates the entry even when the command stream is unchanged.
func layerContentHash(enc *Encoding, images []*Image, seg layerSegment) uint64 {
	const prime = 1099511628211
	h := enc.Hash()
	h ^= uint64(seg.blend)
	h *= prime
	h ^= uint64(math.Float32bits(seg.alpha))
	h *= prime
	if seg.hasClip {
		h ^= 0x51a7c11
		h *= prime
	}
	dec := NewDecoder(enc)
	for dec.Next() {
		if dec.Tag() != TagImage {
			continue
		}
		idx, _ := dec.Image()
		var ver uint64
		if int(idx) < len(images) && images[idx] != nil {
			ver = images[idx].Version()
		}
		h ^= ver + 1
		h *= prime
		h ^= uint64(idx)
		h *= prime
	}
	return h
}
