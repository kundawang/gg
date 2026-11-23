package scene

import (
	"math"

	"github.com/gogpu/gg/internal/stroke"
	"github.com/gogpu/gg/text"
)

// Hit testing for retained scenes.
//
// A level editor needs to know which encoded draw command lies under a pointer.
// The renderer has no reusable geometry-to-point entry point: coverage is
// rasterized into pixels and the regions are never retained, while a query must
// not disturb the command stream in any way. HitTester walks an Encoding exactly
// the way the renderers do (same tag dispatch, same transform deltas, same
// nesting) but answers containment in floating point instead of producing pixels.
//
// # Coordinate spaces
//
// Recorded coordinates are "scene coordinates". A Scene may additionally carry
// an origin (see Scene.SetOrigin) describing where scene coordinates sit inside
// the parent/document space the host hit-tests in. Query points are interpreted
// in that same parent space; the origin is subtracted before a command's own
// transform is applied. Negative coordinates are ordinary coordinates.
//
// Each Hit reports LocalX/LocalY: the query point mapped through the inverse of
// the transform that was active for the command, i.e. the point in the space in
// which the command's geometry was authored. For a fill or stroke that is the
// path space; for an image it is image space ((0,0)-(W,H)); for text it is the
// glyph-run space (glyph positions plus the run origin). Editors use this to
// place handles or to turn a pick into an object-local coordinate (for example
// the caret position inside rotated text).
//
// # Semantics
//
//   - Fill uses the command's own fill rule (non-zero or even-odd).
//   - Stroke honors width, cap, join and miter limit. The outline is expanded
//     with the same expander the software renderer uses, so hit regions match
//     what is painted. Width is scaled by the transform's scale factor and
//     sub-pixel widths behave like the 1px hairline the renderer draws.
//   - Image maps the point into image space through the inverse of the image
//     transform and tests the image rectangle. When pixel data is present, a
//     sampled alpha of exactly zero is treated as a miss, so fully transparent
//     areas of an image do not swallow clicks. Images without pixel data fall
//     back to their rectangle.
//   - Text maps the point into the glyph run's space, offsets it by the run
//     origin and tests the glyph boxes derived from the font's glyph bounds.
//     The text string is never re-shaped. Without a font in the registry the
//     advance span is used so the run remains hittable.
//   - Clip regions constrain every command recorded inside them. Bounds are
//     tested first, then the clip path itself (non-zero, matching the mask the
//     renderers fill). Nested clips intersect.
//   - A layer is a compositor concept with no painted geometry of its own, so
//     its bounds never expand a hit. A layer that composites nothing visible — a
//     fully transparent layer, or one using BlendDestination — hides everything
//     drawn inside it. Other blend modes keep their content pickable.
//   - Filters that grow output (blur, drop shadow) attached to a layer expand
//     that layer's hit region while it is open, so e.g. a drop shadow is
//     clickable. Expansion is expressed in scene units and applied per side.
//   - Tangency counts as a hit. A point exactly on a fill edge, stroke edge,
//     image border or glyph box is contained. Boundary points are detected
//     exactly on straight segments and within BoundaryEpsilon on flattened
//     curves; the renderer's anti-aliased coverage is inclusive in the same
//     way.
//   - Empty paths never hit. MoveTo-only paths have zero winding and no
//     boundary, so they never hit either.
//
// # Robustness
//
// Non-finite query points, singular transforms, unbalanced tags, truncated
// streams and very deep nesting never panic. A query allocates only per-query
// scratch and never mutates the encoding, its caches, or the command order.
//
// # Concurrency
//
// A HitTester is safe for concurrent use: each query builds its own traversal
// state, so one tester can serve many goroutines. Resources supplied through
// the constructor (images, fonts) are read-only. An Encoding must not be
// mutated while queries run, which is already true of every read-only scene
// API.
type HitTester struct {
	enc    *Encoding
	images []*Image
	fonts  map[uint64]*text.FontSource
}

// HitKind classifies the draw command a hit landed on.
type HitKind uint8

const (
	// HitKindFill is a TagFill or TagFillRoundRect command.
	HitKindFill HitKind = iota
	// HitKindStroke is a TagStroke command.
	HitKindStroke
	// HitKindImage is a TagImage command.
	HitKindImage
	// HitKindText is a TagText command.
	HitKindText
)

// String returns a human-readable name for the hit kind.
func (k HitKind) String() string {
	switch k {
	case HitKindFill:
		return "Fill"
	case HitKindStroke:
		return "Stroke"
	case HitKindImage:
		return "Image"
	case HitKindText:
		return "Text"
	default:
		return unknownStr
	}
}

// Hit describes one draw command that contains a query point.
type Hit struct {
	// ID is the command's stable index: the position of its draw tag in the
	// encoding's tag stream. It is stable for a scene rebuilt in the same order
	// (the immediate-mode pattern) and lives in the same ID space as
	// TaggedBounds.
	ID uint64

	// Kind identifies the command that was hit.
	Kind HitKind

	// LocalX, LocalY are the query point in the command's local space, obtained
	// by inverting the transform that was active when the command was recorded.
	// For text this is glyph-run space (before the run origin offset); for an
	// image it is image space.
	LocalX, LocalY float32

	// Bounds is the command's hit-region bounding box in parent (query) space,
	// including stroke width, image extent, text extent and clip/filter
	// expansion. It is conservative, not the exact hit region.
	Bounds Rect
}

// HitOptions tunes a query.
type HitOptions struct {
	// MaxDepth bounds clip and layer nesting. Queries that exceed it ignore
	// deeper structure instead of recursing without limit. Zero selects
	// MaxHitDepth.
	MaxDepth int

	// IncludeTransparentImages keeps fully transparent image pixels hittable.
	// By default a sampled alpha of zero is a miss (see the semantics above).
	IncludeTransparentImages bool
}

// MaxHitDepth is the default nesting bound for clip and layer structures.
const MaxHitDepth = 256

// BoundaryEpsilon is the distance, in local scene units, within which a point
// is treated as lying on a flattened curve boundary. Straight segments are
// tested exactly. A negative or omitted value falls back to this constant.
const BoundaryEpsilon = 1e-4

// DefaultHitOptions returns the query options used by Hit and HitTest.
func DefaultHitOptions() HitOptions {
	return HitOptions{MaxDepth: MaxHitDepth}
}

// NewHitTester wraps an encoding for hit testing. A nil encoding yields a
// tester that reports no hits, never a panic.
//
// The encoding alone cannot resolve TagImage dimensions or TagText font
// metrics; use NewHitTesterWithResources to supply them. Without them images
// never hit and text falls back to its advance span.
func NewHitTester(enc *Encoding) *HitTester {
	return &HitTester{enc: enc}
}

// NewHitTesterWithResources wraps an encoding together with the image registry
// and font registry needed to resolve image and text commands. Either argument
// may be nil. The slices and maps are read-only after construction.
func NewHitTesterWithResources(enc *Encoding, images []*Image, fonts map[uint64]*text.FontSource) *HitTester {
	return &HitTester{enc: enc, images: images, fonts: fonts}
}

// Hit returns the topmost hit (last drawn command wins) or nil.
func (h *HitTester) Hit(x, y float32) *Hit {
	return h.HitWithOptions(x, y, DefaultHitOptions())
}

// HitAll returns every hit, topmost first (reverse draw order).
func (h *HitTester) HitAll(x, y float32) []Hit {
	return h.HitAllWithOptions(x, y, DefaultHitOptions())
}

// HitWithOptions is Hit with explicit options.
func (h *HitTester) HitWithOptions(x, y float32, opts HitOptions) *Hit {
	hits := h.HitAllWithOptions(x, y, opts)
	if len(hits) == 0 {
		return nil
	}
	return &hits[0]
}

// HitTest finds the topmost command of e containing (x, y). It is a
// convenience wrapper around NewHitTester.
func (e *Encoding) HitTest(x, y float32) *Hit {
	return NewHitTester(e).Hit(x, y)
}

// HitTestAll finds every command of e containing (x, y), topmost first.
func (e *Encoding) HitTestAll(x, y float32) []Hit {
	return NewHitTester(e).HitAll(x, y)
}

// HitTest finds the topmost command of s containing the query point. The point
// is expressed in the scene's parent space, so the scene origin is honored.
func (s *Scene) HitTest(x, y float32) *Hit {
	return s.HitTestWithOptions(x, y, DefaultHitOptions())
}

// HitTestAll finds every command of s containing the query point, topmost
// first. The point is expressed in the scene's parent space.
func (s *Scene) HitTestAll(x, y float32) []Hit {
	return s.HitTestAllWithOptions(x, y, DefaultHitOptions())
}

// HitTestWithOptions is Scene.HitTest with explicit options.
func (s *Scene) HitTestWithOptions(x, y float32, opts HitOptions) *Hit {
	hits := s.HitTestAllWithOptions(x, y, opts)
	if len(hits) == 0 {
		return nil
	}
	return &hits[0]
}

// HitTestAllWithOptions is Scene.HitTestAll with explicit options.
//
// Open layers are read in place; the scene, its encodings and its caches are
// not modified, and command order is preserved.
func (s *Scene) HitTestAllWithOptions(x, y float32, opts HitOptions) []Hit {
	if s == nil {
		return nil
	}
	originX, originY := s.Origin()
	enc := s.hitSnapshotEncoding()
	if enc == nil {
		return nil
	}
	tester := NewHitTesterWithResources(enc, s.imageRegistry, s.fontRegistry)
	return tester.HitAllWithOptions(x-originX, y-originY, opts)
}

// hitSnapshotEncoding returns a read-only view of the scene's command stream
// without flattening it.
//
// The stream order produced by Encoding() is: content already merged into the
// root encoding, then the root layer's pending content, then each open layer's
// content in stack order. The parent encoding of an open layer already ends
// with that layer's TagPushLayer, so appending the child's content yields
// exactly what PopLayer would produce. The scene's own encodings are only read.
func (s *Scene) hitSnapshotEncoding() *Encoding {
	layers := s.layerStack.All()
	if len(layers) == 0 {
		return nil
	}

	rootPending := false
	if root := s.layerStack.Root(); root != nil {
		rootPending = root.Encoding != nil && !root.Encoding.IsEmpty()
	}
	if len(layers) == 1 && !rootPending {
		return s.encoding
	}

	merged := NewEncoding()
	merged.AppendWithImages(s.encoding, 0)
	for _, layer := range layers {
		if layer.Encoding == nil || layer.Encoding.IsEmpty() {
			continue
		}
		merged.AppendWithImages(layer.Encoding, 0)
	}
	return merged
}

// --- Per-query traversal state ---------------------------------------------

// hitState is the per-query state. It is allocated per call so a HitTester
// stays safe for concurrent use.
type hitState struct {
	enc    *Encoding
	images []*Image
	fonts  map[uint64]*text.FontSource
	opts   HitOptions

	x, y float32 // query point in scene coordinates

	// Stream cursors mirroring Decoder.
	tagIdx   int
	pathIdx  int
	drawIdx  int
	transIdx int
	textIdx  int

	currentTransform Affine

	path       *Path // path under construction; reset on TagBeginPath
	pathActive bool

	clipStack  []hitClip
	layerStack []hitLayer

	// layerPushIdx counts TagPushLayer tags seen, to index enc.layerExpand.
	layerPushIdx int

	hits []Hit

	// scratch reused across commands within one query.
	pathPool     PathPool
	glyphScratch []GlyphEntry
	verbScratch  []stroke.PathVerb
	coordScratch []float64
}

// hitClip is one active clip region, expressed in scene space.
type hitClip struct {
	bounds Rect
	path   *Path // nil for a bounds-only clip
	empty  bool  // clip region contains nothing
}

// hitLayer is one active compositing layer.
type hitLayer struct {
	expand Rect // per-side filter growth in scene units
	hidden bool // layer contributes nothing visible
}

// resolveOptions applies defaults.
func resolveOptions(opts HitOptions) HitOptions {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = MaxHitDepth
	}
	return opts
}

func floatIsNaNOrInf(v float32) bool {
	f := float64(v)
	return math.IsNaN(f) || math.IsInf(f, 0)
}

func finitePoint(x, y float32) bool {
	return !floatIsNaNOrInf(x) && !floatIsNaNOrInf(y)
}

// HitAllWithOptions walks the encoding forward, collecting hits in recording
// order, then reverses them so the topmost (last painted) command is first.
func (h *HitTester) HitAllWithOptions(x, y float32, opts HitOptions) []Hit {
	if h == nil || h.enc == nil || !finitePoint(x, y) {
		return nil
	}
	st := &hitState{
		enc:              h.enc,
		images:           h.images,
		fonts:            h.fonts,
		opts:             resolveOptions(opts),
		x:                x,
		y:                y,
		currentTransform: IdentityAffine(),
	}
	st.run()

	hits := st.hits
	for i, j := 0, len(hits)-1; i < j; i, j = i+1, j-1 {
		hits[i], hits[j] = hits[j], hits[i]
	}
	return hits
}

// run is the tag interpreter. It consumes exactly the data words the renderer
// decoder consumes for every tag, so its cursors stay aligned.
//
//nolint:gocyclo,cyclop // tag dispatch mirrors the renderers
func (st *hitState) run() {
	tags := st.enc.tags
	for st.tagIdx < len(tags) {
		tag := tags[st.tagIdx]
		st.tagIdx++

		switch tag {
		case TagTransform:
			st.currentTransform = st.readTransform()

		case TagBeginPath:
			st.beginPath()

		case TagMoveTo:
			if px, py, ok := st.readPoint(); ok && st.pathActive {
				st.path.MoveTo(px, py)
			}

		case TagLineTo:
			if px, py, ok := st.readPoint(); ok && st.pathActive {
				st.path.LineTo(px, py)
			}

		case TagQuadTo:
			if cx, cy, px, py, ok := st.readQuad(); ok && st.pathActive {
				st.path.QuadTo(cx, cy, px, py)
			}

		case TagCubicTo:
			if c1x, c1y, c2x, c2y, px, py, ok := st.readCubic(); ok && st.pathActive {
				st.path.CubicTo(c1x, c1y, c2x, c2y, px, py)
			}

		case TagClosePath:
			if st.pathActive {
				st.path.Close()
			}

		case TagEndPath:
			// Path complete; the next draw or clip consumes it.

		case TagFill:
			_, style := st.readFill()
			st.hitFill(style)
			st.releasePath()

		case TagFillRoundRect:
			_, style, rect, rx, ry := st.readFillRoundRect()
			st.hitFillRoundRect(style, rect, rx, ry)
			st.releasePath()

		case TagStroke:
			_, style := st.readStroke()
			st.hitStroke(style)
			st.releasePath()

		case TagPushLayer:
			blend, alpha := st.readPushLayer()
			st.pushLayer(layerHidden(blend, alpha))

		case TagPopLayer:
			st.popLayer()

		case TagBeginClip:
			st.beginClip()

		case TagEndClip:
			st.endClip()

		case TagSetAntiAlias:
			st.skipDraw(1)

		case TagBrush:
			st.skipFloats(4)

		case TagImage:
			imageIdx, transform := st.readImage()
			st.hitImage(imageIdx, transform)

		case TagText:
			run, glyphs := st.readText()
			st.hitText(run, glyphs)

		default:
			// Unknown tag: stop rather than risk misaligned cursors.
			return
		}
	}
}

// --- Stream readers --------------------------------------------------------

func (st *hitState) readTransform() Affine {
	if st.transIdx < len(st.enc.transforms) {
		t := st.enc.transforms[st.transIdx]
		st.transIdx++
		return t
	}
	return IdentityAffine()
}

func (st *hitState) skipFloats(n int) {
	st.pathIdx = min(st.pathIdx+n, len(st.enc.pathData))
}

func (st *hitState) skipDraw(n int) {
	st.drawIdx = min(st.drawIdx+n, len(st.enc.drawData))
}

func (st *hitState) readPoint() (x, y float32, ok bool) {
	if st.pathIdx+2 > len(st.enc.pathData) {
		return 0, 0, false
	}
	x = st.enc.pathData[st.pathIdx]
	y = st.enc.pathData[st.pathIdx+1]
	st.pathIdx += 2
	return x, y, true
}

func (st *hitState) readQuad() (cx, cy, x, y float32, ok bool) {
	if st.pathIdx+4 > len(st.enc.pathData) {
		return 0, 0, 0, 0, false
	}
	cx = st.enc.pathData[st.pathIdx]
	cy = st.enc.pathData[st.pathIdx+1]
	x = st.enc.pathData[st.pathIdx+2]
	y = st.enc.pathData[st.pathIdx+3]
	st.pathIdx += 4
	return cx, cy, x, y, true
}

//nolint:gocritic // tooManyResultsChecker: cubic decoding naturally returns 6 coords + ok
func (st *hitState) readCubic() (c1x, c1y, c2x, c2y, x, y float32, ok bool) {
	if st.pathIdx+6 > len(st.enc.pathData) {
		return 0, 0, 0, 0, 0, 0, false
	}
	c1x = st.enc.pathData[st.pathIdx]
	c1y = st.enc.pathData[st.pathIdx+1]
	c2x = st.enc.pathData[st.pathIdx+2]
	c2y = st.enc.pathData[st.pathIdx+3]
	x = st.enc.pathData[st.pathIdx+4]
	y = st.enc.pathData[st.pathIdx+5]
	st.pathIdx += 6
	return c1x, c1y, c2x, c2y, x, y, true
}

func (st *hitState) readFill() (brushIdx uint32, style FillStyle) {
	if st.drawIdx+2 > len(st.enc.drawData) {
		return 0, FillNonZero
	}
	brushIdx = st.enc.drawData[st.drawIdx]
	style = FillStyle(st.enc.drawData[st.drawIdx+1])
	st.drawIdx += 2
	return brushIdx, style
}

func (st *hitState) readFillRoundRect() (brushIdx uint32, style FillStyle, rect Rect, rx, ry float32) {
	if st.drawIdx+2 > len(st.enc.drawData) {
		return 0, FillNonZero, Rect{}, 0, 0
	}
	brushIdx = st.enc.drawData[st.drawIdx]
	style = FillStyle(st.enc.drawData[st.drawIdx+1])
	st.drawIdx += 2

	if st.pathIdx+6 > len(st.enc.pathData) {
		return brushIdx, style, Rect{}, 0, 0
	}
	rect = Rect{
		MinX: st.enc.pathData[st.pathIdx],
		MinY: st.enc.pathData[st.pathIdx+1],
		MaxX: st.enc.pathData[st.pathIdx+2],
		MaxY: st.enc.pathData[st.pathIdx+3],
	}
	rx = st.enc.pathData[st.pathIdx+4]
	ry = st.enc.pathData[st.pathIdx+5]
	st.pathIdx += 6
	return brushIdx, style, rect, rx, ry
}

func (st *hitState) readStroke() (brushIdx uint32, style *StrokeStyle) {
	if st.drawIdx+5 > len(st.enc.drawData) {
		return 0, DefaultStrokeStyle()
	}
	brushIdx = st.enc.drawData[st.drawIdx]
	widthBits := st.enc.drawData[st.drawIdx+1]
	miterBits := st.enc.drawData[st.drawIdx+2]
	capVal := st.enc.drawData[st.drawIdx+3]
	joinVal := st.enc.drawData[st.drawIdx+4]
	st.drawIdx += 5

	return brushIdx, &StrokeStyle{
		Width:      math.Float32frombits(widthBits),
		MiterLimit: math.Float32frombits(miterBits),
		Cap:        LineCap(capVal),
		Join:       LineJoin(joinVal),
	}
}

func (st *hitState) readPushLayer() (blend BlendMode, alpha float32) {
	if st.drawIdx+2 > len(st.enc.drawData) {
		return BlendNormal, 1
	}
	blend = BlendMode(st.enc.drawData[st.drawIdx])
	alpha = math.Float32frombits(st.enc.drawData[st.drawIdx+1])
	st.drawIdx += 2
	return blend, alpha
}

// layerHidden reports whether a layer composites nothing visible. A fully
// transparent layer and BlendDestination (keep destination unchanged) both
// contribute no pixels, so their content is not pickable.
func layerHidden(blend BlendMode, alpha float32) bool {
	return alpha <= 0 || blend == BlendDestination
}

func (st *hitState) readImage() (imageIdx uint32, transform Affine) {
	if st.drawIdx+1 > len(st.enc.drawData) {
		return 0, IdentityAffine()
	}
	imageIdx = st.enc.drawData[st.drawIdx]
	st.drawIdx++

	if st.transIdx < len(st.enc.transforms) {
		transform = st.enc.transforms[st.transIdx]
		st.transIdx++
	} else {
		transform = IdentityAffine()
	}
	return imageIdx, transform
}

// readText decodes a TagText header and its glyph entries. It never re-shapes
// text; only the pre-shaped positions are read.
func (st *hitState) readText() (run GlyphRunData, glyphs []GlyphEntry) {
	td := st.enc.textData
	if st.textIdx+glyphRunDataSize > len(td) {
		st.textIdx = len(td)
		return GlyphRunData{}, nil
	}

	off := st.textIdx
	run.FontSourceID = readUint64LE(td[off:])
	off += 8
	run.FontSize = math.Float32frombits(readUint32LE(td[off:]))
	off += 4
	run.GlyphCount = uint16(readUint16LE(td[off:]))
	off += 2
	run.Flags = TextFlags(readUint16LE(td[off:]))
	off += 2
	run.OriginX = math.Float32frombits(readUint32LE(td[off:]))
	off += 4
	run.OriginY = math.Float32frombits(readUint32LE(td[off:]))
	off += 4
	run.BrushIndex = readUint32LE(td[off:])
	off += 4
	run.TextLen = uint16(readUint16LE(td[off:]))
	off += 2

	n := int(run.GlyphCount)
	if off+n*glyphEntrySize > len(td) {
		st.textIdx = off
		return run, nil
	}

	if cap(st.glyphScratch) < n {
		st.glyphScratch = make([]GlyphEntry, n)
	}
	glyphs = st.glyphScratch[:n]
	for i := range n {
		glyphs[i].GlyphID = text.GlyphID(readUint16LE(td[off:]))
		off += 2
		glyphs[i].X = math.Float32frombits(readUint32LE(td[off:]))
		off += 4
		glyphs[i].Y = math.Float32frombits(readUint32LE(td[off:]))
		off += 4
	}

	textLen := int(run.TextLen)
	if off+textLen <= len(td) {
		off += textLen
	}
	st.textIdx = off
	return run, glyphs
}

// --- Clip and layer stacks -------------------------------------------------

func (st *hitState) beginPath() {
	st.path = st.pathPool.Get()
	st.pathActive = true
}

// releasePath returns the consumed path to the pool. Called after a draw
// command eats the path; clip commands keep their transformed copy instead.
func (st *hitState) releasePath() {
	if st.path != nil {
		st.pathPool.Put(st.path)
		st.path = nil
	}
	st.pathActive = false
}

// pushLayer records a new layer. Past MaxDepth the layer is still pushed so its
// PopLayer pairs with this PushLayer and earlier layers stay balanced; only its
// filter contribution is dropped.
func (st *hitState) pushLayer(hidden bool) {
	var expand Rect
	if st.layerPushIdx < len(st.enc.layerExpand) {
		expand = st.enc.layerExpand[st.layerPushIdx]
	}
	st.layerPushIdx++

	if len(st.layerStack) >= st.opts.MaxDepth {
		// Keep push/pop paired; drop the deeper layer's own contribution.
		st.layerStack = append(st.layerStack, hitLayer{})
		return
	}
	st.layerStack = append(st.layerStack, hitLayer{expand: expand, hidden: hidden})
}

func (st *hitState) popLayer() {
	if len(st.layerStack) == 0 {
		return
	}
	st.layerStack = st.layerStack[:len(st.layerStack)-1]
}

// beginClip pushes a clip region. Past MaxDepth the clip is still pushed (as a
// permissive, bounds-only entry) so EndClip stays paired with BeginClip.
func (st *hitState) beginClip() {
	if !st.pathActive || st.path == nil || st.path.IsEmpty() {
		// An empty clip path clips everything away.
		st.clipStack = append(st.clipStack, hitClip{empty: true})
		st.pathActive = false
		return
	}
	if len(st.clipStack) >= st.opts.MaxDepth {
		// Too deep: keep pairing intact without constraining further.
		st.clipStack = append(st.clipStack, hitClip{})
		st.pathActive = false
		return
	}
	// Bake the active transform into the clip geometry so the clip test runs in
	// scene space, like the mask the renderers fill.
	clipPath := st.path.Transform(st.currentTransform)
	st.clipStack = append(st.clipStack, hitClip{
		bounds: clipPath.Bounds(),
		path:   clipPath,
	})
	st.pathActive = false
}

func (st *hitState) endClip() {
	if len(st.clipStack) == 0 {
		return
	}
	st.clipStack = st.clipStack[:len(st.clipStack)-1]
}

// clipContains reports whether the point survives every active clip and layer.
// A hidden layer (transparent or destination blend) suppresses everything drawn
// inside it.
func (st *hitState) clipContains() bool {
	for i := range st.layerStack {
		if st.layerStack[i].hidden {
			return false
		}
	}
	for i := range st.clipStack {
		c := &st.clipStack[i]
		if c.empty {
			return false
		}
		if !rectContains(c.bounds, st.x, st.y) {
			return false
		}
		if c.path != nil && !pathContainsWithBoundary(c.path, st.x, st.y, false) {
			return false
		}
	}
	return true
}

// layerExpand returns the accumulated per-side filter growth from open layers.
func (st *hitState) layerExpand() Rect {
	var total Rect
	for i := range st.layerStack {
		e := st.layerStack[i].expand
		total.MinX += e.MinX
		total.MinY += e.MinY
		total.MaxX += e.MaxX
		total.MaxY += e.MaxY
	}
	return total
}

// grow applies the open layers' filter expansion to a command's parent-space
// bounds. Expansion is stored as a per-side offset rect (MinX/MaxX are signed
// offsets, exactly as Filter.ExpandBounds(zero) returns them), so growth is a
// plain addition on every side.
func (st *hitState) grow(bounds Rect) Rect {
	if bounds.IsEmpty() {
		return bounds
	}
	e := st.layerExpand()
	if e.MinX == 0 && e.MinY == 0 && e.MaxX == 0 && e.MaxY == 0 {
		return bounds
	}
	return Rect{
		MinX: bounds.MinX + e.MinX,
		MinY: bounds.MinY + e.MinY,
		MaxX: bounds.MaxX + e.MaxX,
		MaxY: bounds.MaxY + e.MaxY,
	}
}

// expandedHit reports whether a filter-expanded layer makes an otherwise-missed
// command hittable. The filter only grows the painted footprint outward, so the
// extra region is the halo between the command's scene-space bounds and those
// bounds grown by the open layers' expansion. Points still inside the original
// bounds are handled by the geometry test, not here.
func (st *hitState) expandedHit(sceneBounds Rect) bool {
	e := st.layerExpand()
	if e.MinX == 0 && e.MinY == 0 && e.MaxX == 0 && e.MaxY == 0 || sceneBounds.IsEmpty() {
		return false
	}
	grown := st.grow(sceneBounds)
	if !rectContains(grown, st.x, st.y) {
		return false
	}
	return !rectContains(sceneBounds, st.x, st.y)
}

// --- Draw command handling -------------------------------------------------

func (st *hitState) addHit(kind HitKind, localX, localY float32, bounds Rect) {
	st.hits = append(st.hits, Hit{
		ID:     uint64(st.tagIdx - 1), //nolint:gosec // tag index is non-negative
		Kind:   kind,
		LocalX: localX,
		LocalY: localY,
		Bounds: bounds,
	})
}

// hitFill tests the pending path with the command's fill rule.
func (st *hitState) hitFill(style FillStyle) {
	if !st.pathActive || st.path == nil || st.path.IsEmpty() {
		return
	}
	if !st.clipContains() {
		return
	}
	inv, ok := invertAffine(st.currentTransform)
	if !ok {
		return
	}
	lx, ly := inv.TransformPoint(st.x, st.y)

	sceneBounds := transformBounds(st.path.Bounds(), st.currentTransform)
	evenOdd := style == FillEvenOdd
	if !pathContainsWithBoundary(st.path, lx, ly, evenOdd) && !st.expandedHit(sceneBounds) {
		return
	}
	st.addHit(HitKindFill, lx, ly, st.grow(sceneBounds))
}

// hitFillRoundRect tests the SDF-encoded rounded rectangle.
func (st *hitState) hitFillRoundRect(_ FillStyle, rect Rect, rx, ry float32) {
	if rect.IsEmpty() || !st.clipContains() {
		return
	}
	inv, ok := invertAffine(st.currentTransform)
	if !ok {
		return
	}
	lx, ly := inv.TransformPoint(st.x, st.y)

	shape := NewRoundRectShape(rect, rx, ry)
	sceneBounds := transformBounds(rect, st.currentTransform)
	if !shape.Contains(lx, ly) && !st.expandedHit(sceneBounds) {
		return
	}
	st.addHit(HitKindFill, lx, ly, st.grow(sceneBounds))
}

// hitStroke expands the pending path into a fill outline and tests it.
//
// The expansion matches SoftwareRenderer.Stroke: width is multiplied by the
// transform scale factor and a device width at or below one pixel is drawn as
// a 1px hairline.
func (st *hitState) hitStroke(style *StrokeStyle) {
	if !st.pathActive || st.path == nil || st.path.IsEmpty() || style == nil {
		return
	}
	if style.Width <= 0 || !st.clipContains() {
		return
	}
	inv, ok := invertAffine(st.currentTransform)
	if !ok {
		return
	}
	lx, ly := inv.TransformPoint(st.x, st.y)

	width := style.Width * affineScaleFactor(st.currentTransform)
	if width <= 1 {
		width = 1 // hairline parity with the rasterizer
	}

	local := st.path.Bounds()
	half := width / 2
	local.MinX -= half
	local.MinY -= half
	local.MaxX += half
	local.MaxY += half
	sceneBounds := transformBounds(local, st.currentTransform)

	if !st.expandedHit(sceneBounds) {
		verbs, coords := st.strokeOutline(style, width)
		if len(verbs) == 0 || !strokeOutlineContains(verbs, coords, lx, ly) {
			return
		}
	}
	st.addHit(HitKindStroke, lx, ly, st.grow(sceneBounds))
}

// strokeOutline expands the current path with the renderer's stroke expander.
func (st *hitState) strokeOutline(style *StrokeStyle, width float32) ([]stroke.PathVerb, []float64) {
	expander := stroke.NewStrokeExpander(stroke.Stroke{
		Width:      float64(width),
		Cap:        toStrokeCap(style.Cap),
		Join:       toStrokeJoin(style.Join),
		MiterLimit: float64(style.MiterLimit),
	})
	expander.SetTolerance(0.1)

	verbs := st.scenePathVerbs()
	st.coordScratch = st.coordScratch[:0]
	for _, v := range st.path.Points() {
		st.coordScratch = append(st.coordScratch, float64(v))
	}
	return expander.Expand(verbs, st.coordScratch)
}

// scenePathVerbs converts the path's verbs to the stroke package encoding
// (identical ordinal layout).
func (st *hitState) scenePathVerbs() []stroke.PathVerb {
	verbs := st.path.Verbs()
	if cap(st.verbScratch) < len(verbs) {
		st.verbScratch = make([]stroke.PathVerb, len(verbs))
	}
	st.verbScratch = st.verbScratch[:len(verbs)]
	for i, v := range verbs {
		st.verbScratch[i] = stroke.PathVerb(v)
	}
	return st.verbScratch
}

// hitImage tests a drawn image.
func (st *hitState) hitImage(imageIdx uint32, transform Affine) {
	img := st.imageAt(imageIdx)
	if img == nil || img.Width <= 0 || img.Height <= 0 {
		return
	}
	if !st.clipContains() {
		return
	}
	inv, ok := invertAffine(transform)
	if !ok {
		return
	}
	ux, uy := inv.TransformPoint(st.x, st.y)
	imgRect := Rect{MinX: 0, MinY: 0, MaxX: float32(img.Width), MaxY: float32(img.Height)}
	sceneBounds := transformBounds(imgRect, transform)

	if rectContains(imgRect, ux, uy) {
		// Inside the image quad: honor transparent-image semantics.
		if !st.opts.IncludeTransparentImages && !imagePixelOpaque(img, ux, uy) &&
			!st.expandedHit(sceneBounds) {
			return
		}
	} else if !st.expandedHit(sceneBounds) {
		return
	}
	st.addHit(HitKindImage, ux, uy, st.grow(sceneBounds))
}

func (st *hitState) imageAt(idx uint32) *Image {
	if st.images == nil || int(idx) >= len(st.images) {
		return nil
	}
	return st.images[idx]
}

// hitText tests a pre-shaped glyph run.
func (st *hitState) hitText(run GlyphRunData, glyphs []GlyphEntry) {
	if len(glyphs) == 0 || run.GlyphCount == 0 || !st.clipContains() {
		return
	}
	inv, ok := invertAffine(st.currentTransform)
	if !ok {
		return
	}
	// The run origin is applied after the scene transform in both renderers.
	lx, ly := inv.TransformPoint(st.x, st.y)
	gx := lx - run.OriginX
	gy := ly - run.OriginY

	parsed := st.fontAt(run.FontSourceID)
	bounds := glyphRunBounds(parsed, run, glyphs)
	if bounds.IsEmpty() {
		return
	}
	// Bounds are in run space; shift to scene space before transforming.
	bounds.MinX += run.OriginX
	bounds.MaxX += run.OriginX
	bounds.MinY += run.OriginY
	bounds.MaxY += run.OriginY
	sceneBounds := transformBounds(bounds, st.currentTransform)

	if !glyphRunContains(parsed, run, glyphs, gx, gy) && !st.expandedHit(sceneBounds) {
		return
	}
	st.addHit(HitKindText, lx, ly, st.grow(sceneBounds))
}

func (st *hitState) fontAt(id uint64) text.ParsedFont {
	if st.fonts == nil {
		return nil
	}
	source := st.fonts[id]
	if source == nil {
		return nil
	}
	return source.Parsed()
}

// --- Geometry helpers ------------------------------------------------------

// invertAffine returns the inverse of a, or false for a singular or non-finite
// matrix.
func invertAffine(a Affine) (Affine, bool) {
	det := a.A*a.E - a.B*a.D
	if det == 0 || floatIsNaNOrInf(det) {
		return Affine{}, false
	}
	invDet := 1 / det
	inv := Affine{
		A: a.E * invDet,
		B: -a.B * invDet,
		C: (a.B*a.F - a.E*a.C) * invDet,
		D: -a.D * invDet,
		E: a.A * invDet,
		F: (a.D*a.C - a.A*a.F) * invDet,
	}
	if !affineFinite(inv) {
		return Affine{}, false
	}
	return inv, true
}

func affineFinite(a Affine) bool {
	return !floatIsNaNOrInf(a.A) && !floatIsNaNOrInf(a.B) && !floatIsNaNOrInf(a.C) &&
		!floatIsNaNOrInf(a.D) && !floatIsNaNOrInf(a.E) && !floatIsNaNOrInf(a.F)
}

// affineScaleFactor returns the largest stretch the transform applies to a unit
// vector (mirrors gg.Matrix.ScaleFactor).
func affineScaleFactor(a Affine) float32 {
	sx := float32(math.Hypot(float64(a.A), float64(a.D)))
	sy := float32(math.Hypot(float64(a.B), float64(a.E)))
	if sx > sy {
		return sx
	}
	return sy
}

// rectContains reports whether the point is inside the closed rectangle;
// points on an edge count as inside.
func rectContains(r Rect, x, y float32) bool {
	if r.IsEmpty() {
		return false
	}
	return x >= r.MinX && x <= r.MaxX && y >= r.MinY && y <= r.MaxY
}

// pathContainsWithBoundary reports whether the point is inside the path under
// the requested fill rule, treating boundary points as inside.
func pathContainsWithBoundary(p *Path, px, py float32, evenOdd bool) bool {
	if p == nil || p.IsEmpty() {
		return false
	}
	b := p.Bounds()
	if !b.IsEmpty() && !rectContains(b, px, py) {
		return false
	}
	if evenOdd {
		if pathCrossingsEvenOdd(p, px, py)&1 != 0 {
			return true
		}
	} else if p.Contains(px, py) {
		return true
	}
	return pathOnBoundary(p, px, py)
}

// pathOnBoundary reports whether the point lies on the path outline. Straight
// segments are tested exactly; curves are flattened and tested within
// BoundaryEpsilon.
func pathOnBoundary(p *Path, px, py float32) bool {
	pts := p.Points()
	pidx := 0
	var curX, curY, startX, startY float32
	have := false

	for _, verb := range p.Verbs() {
		switch verb {
		case MoveTo:
			curX, curY = pts[pidx], pts[pidx+1]
			startX, startY = curX, curY
			pidx += 2
			have = true
		case LineTo:
			nx, ny := pts[pidx], pts[pidx+1]
			if have && pointOnSegment(px, py, curX, curY, nx, ny) {
				return true
			}
			curX, curY = nx, ny
			pidx += 2
		case QuadTo:
			cx, cy := pts[pidx], pts[pidx+1]
			nx, ny := pts[pidx+2], pts[pidx+3]
			if have && curveOnBoundary(func(t float32) (float32, float32) {
				mt := 1 - t
				return mt*mt*curX + 2*mt*t*cx + t*t*nx, mt*mt*curY + 2*mt*t*cy + t*t*ny
			}, px, py) {
				return true
			}
			curX, curY = nx, ny
			pidx += 4
		case CubicTo:
			c1x, c1y := pts[pidx], pts[pidx+1]
			c2x, c2y := pts[pidx+2], pts[pidx+3]
			nx, ny := pts[pidx+4], pts[pidx+5]
			if have && curveOnBoundary(func(t float32) (float32, float32) {
				mt := 1 - t
				a := mt * mt * mt
				b := 3 * mt * mt * t
				c := 3 * mt * t * t
				d := t * t * t
				return a*curX + b*c1x + c*c2x + d*nx, a*curY + b*c1y + c*c2y + d*ny
			}, px, py) {
				return true
			}
			curX, curY = nx, ny
			pidx += 6
		case Close:
			if have && pointOnSegment(px, py, curX, curY, startX, startY) {
				return true
			}
			curX, curY = startX, startY
		}
	}
	// An unclosed final subpath is still filled as if closed.
	if have && (curX != startX || curY != startY) &&
		pointOnSegment(px, py, curX, curY, startX, startY) {
		return true
	}
	return false
}

// curveOnBoundary samples a parametric curve and tests each chord.
func curveOnBoundary(point func(t float32) (float32, float32), px, py float32) bool {
	const steps = 32
	prevX, prevY := point(0)
	for i := 1; i <= steps; i++ {
		x, y := point(float32(i) / steps)
		if pointOnSegment(px, py, prevX, prevY, x, y) {
			return true
		}
		prevX, prevY = x, y
	}
	return false
}

// pointOnSegment reports whether the point lies on the closed line segment,
// within BoundaryEpsilon.
func pointOnSegment(px, py, x1, y1, x2, y2 float32) bool {
	dx := x2 - x1
	dy := y2 - y1
	lenSq := dx*dx + dy*dy
	if lenSq == 0 {
		return (px-x1)*(px-x1)+(py-y1)*(py-y1) <= BoundaryEpsilon*BoundaryEpsilon
	}
	t := ((px-x1)*dx + (py-y1)*dy) / lenSq
	if t < 0 || t > 1 {
		// Allow the epsilon to cover the caps.
		margin := BoundaryEpsilon / float32(math.Sqrt(float64(lenSq)))
		if t < -margin || t > 1+margin {
			return false
		}
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
	}
	cx := x1 + t*dx
	cy := y1 + t*dy
	return (px-cx)*(px-cx)+(py-cy)*(py-cy) <= BoundaryEpsilon*BoundaryEpsilon
}

// pathCrossingsEvenOdd counts ray crossings under the even-odd rule.
func pathCrossingsEvenOdd(p *Path, px, py float32) int {
	pts := p.Points()
	crossings := 0
	var curX, curY, startX, startY float32
	pidx := 0

	for _, verb := range p.Verbs() {
		switch verb {
		case MoveTo:
			curX, curY = pts[pidx], pts[pidx+1]
			startX, startY = curX, curY
			pidx += 2
		case LineTo:
			nx, ny := pts[pidx], pts[pidx+1]
			crossings += evenOddSegment(curX, curY, nx, ny, px, py)
			curX, curY = nx, ny
			pidx += 2
		case QuadTo:
			cx, cy := pts[pidx], pts[pidx+1]
			nx, ny := pts[pidx+2], pts[pidx+3]
			crossings += evenOddQuad(curX, curY, cx, cy, nx, ny, px, py)
			curX, curY = nx, ny
			pidx += 4
		case CubicTo:
			c1x, c1y := pts[pidx], pts[pidx+1]
			c2x, c2y := pts[pidx+2], pts[pidx+3]
			nx, ny := pts[pidx+4], pts[pidx+5]
			crossings += evenOddCubic(curX, curY, c1x, c1y, c2x, c2y, nx, ny, px, py)
			curX, curY = nx, ny
			pidx += 6
		case Close:
			crossings += evenOddSegment(curX, curY, startX, startY, px, py)
			curX, curY = startX, startY
		}
	}
	if curX != startX || curY != startY {
		crossings += evenOddSegment(curX, curY, startX, startY, px, py)
	}
	return crossings
}

// evenOddSegment counts ray crossings using a half-open rule so shared vertices
// are counted once.
func evenOddSegment(x1, y1, x2, y2, px, py float32) int {
	if (y1 > py) != (y2 > py) {
		xint := x1 + (py-y1)*(x2-x1)/(y2-y1)
		if px < xint {
			return 1
		}
	}
	return 0
}

func evenOddQuad(x0, y0, cx, cy, x1, y1, px, py float32) int {
	const steps = 16
	crossings := 0
	prevX, prevY := x0, y0
	for i := 1; i <= steps; i++ {
		t := float32(i) / steps
		mt := 1 - t
		x := mt*mt*x0 + 2*mt*t*cx + t*t*x1
		y := mt*mt*y0 + 2*mt*t*cy + t*t*y1
		crossings += evenOddSegment(prevX, prevY, x, y, px, py)
		prevX, prevY = x, y
	}
	return crossings
}

func evenOddCubic(x0, y0, c1x, c1y, c2x, c2y, x1, y1, px, py float32) int {
	const steps = 32
	crossings := 0
	prevX, prevY := x0, y0
	for i := 1; i <= steps; i++ {
		t := float32(i) / steps
		mt := 1 - t
		x := mt*mt*mt*x0 + 3*mt*mt*t*c1x + 3*mt*t*t*c2x + t*t*t*x1
		y := mt*mt*mt*y0 + 3*mt*mt*t*c1y + 3*mt*t*t*c2y + t*t*t*y1
		crossings += evenOddSegment(prevX, prevY, x, y, px, py)
		prevX, prevY = x, y
	}
	return crossings
}

// strokeOutlineContains tests an expanded stroke outline with the non-zero
// rule. The expander can emit overlapping contours at inner joins, which is why
// the renderer fills strokes non-zero.
func strokeOutlineContains(verbs []stroke.PathVerb, coords []float64, px, py float32) bool {
	winding := 0
	var curX, curY, startX, startY float64
	ci := 0
	have := false

	for _, v := range verbs {
		switch v {
		case stroke.VerbMoveTo:
			if ci+2 > len(coords) {
				return false
			}
			curX, curY = coords[ci], coords[ci+1]
			startX, startY = curX, curY
			ci += 2
			have = true
		case stroke.VerbLineTo:
			if ci+2 > len(coords) {
				return false
			}
			nx, ny := coords[ci], coords[ci+1]
			winding += windingSegment64(curX, curY, nx, ny, float64(px), float64(py))
			curX, curY = nx, ny
			ci += 2
		case stroke.VerbQuadTo:
			if ci+4 > len(coords) {
				return false
			}
			cx, cy := coords[ci], coords[ci+1]
			nx, ny := coords[ci+2], coords[ci+3]
			winding += windingQuad64(curX, curY, cx, cy, nx, ny, float64(px), float64(py))
			curX, curY = nx, ny
			ci += 4
		case stroke.VerbCubicTo:
			if ci+6 > len(coords) {
				return false
			}
			c1x, c1y := coords[ci], coords[ci+1]
			c2x, c2y := coords[ci+2], coords[ci+3]
			nx, ny := coords[ci+4], coords[ci+5]
			winding += windingCubic64(curX, curY, c1x, c1y, c2x, c2y, nx, ny, float64(px), float64(py))
			curX, curY = nx, ny
			ci += 6
		case stroke.VerbClose:
			winding += windingSegment64(curX, curY, startX, startY, float64(px), float64(py))
			curX, curY = startX, startY
		}
	}
	if have && (curX != startX || curY != startY) {
		winding += windingSegment64(curX, curY, startX, startY, float64(px), float64(py))
	}
	if winding != 0 {
		return true
	}
	return strokeOutlineOnBoundary(verbs, coords, float64(px), float64(py))
}

// strokeOutlineOnBoundary reports whether the point lies on the outline.
func strokeOutlineOnBoundary(verbs []stroke.PathVerb, coords []float64, px, py float64) bool {
	const eps = float64(BoundaryEpsilon)
	ci := 0
	var curX, curY, startX, startY float64
	have := false

	onLine := func(x1, y1, x2, y2 float64) bool {
		dx, dy := x2-x1, y2-y1
		lenSq := dx*dx + dy*dy
		if lenSq == 0 {
			return (px-x1)*(px-x1)+(py-y1)*(py-y1) <= eps*eps
		}
		t := ((px-x1)*dx + (py-y1)*dy) / lenSq
		if t < 0 || t > 1 {
			return false
		}
		cx, cy := x1+t*dx, y1+t*dy
		return (px-cx)*(px-cx)+(py-cy)*(py-cy) <= eps*eps
	}

	for _, v := range verbs {
		switch v {
		case stroke.VerbMoveTo:
			if ci+2 > len(coords) {
				return false
			}
			curX, curY = coords[ci], coords[ci+1]
			startX, startY = curX, curY
			ci += 2
			have = true
		case stroke.VerbLineTo:
			if ci+2 > len(coords) {
				return false
			}
			nx, ny := coords[ci], coords[ci+1]
			if have && onLine(curX, curY, nx, ny) {
				return true
			}
			curX, curY = nx, ny
			ci += 2
		case stroke.VerbQuadTo, stroke.VerbCubicTo:
			// Boundary tangency on curves is already covered by the fill-side
			// boundary test and by flattening; avoid duplicating sampling here.
			n := 4
			if v == stroke.VerbCubicTo {
				n = 6
			}
			if ci+n > len(coords) {
				return false
			}
			curX, curY = coords[ci+n-2], coords[ci+n-1]
			ci += n
		case stroke.VerbClose:
			if have && onLine(curX, curY, startX, startY) {
				return true
			}
			curX, curY = startX, startY
		}
	}
	return false
}

func windingSegment64(x1, y1, x2, y2, px, py float64) int {
	if y1 <= py && y2 > py {
		if cross64(x1, y1, x2, y2, px, py) > 0 {
			return 1
		}
		return 0
	}
	if y1 > py && y2 <= py {
		if cross64(x1, y1, x2, y2, px, py) < 0 {
			return -1
		}
	}
	return 0
}

func cross64(x1, y1, x2, y2, px, py float64) float64 {
	return (x2-x1)*(py-y1) - (px-x1)*(y2-y1)
}

func windingQuad64(x0, y0, cx, cy, x1, y1, px, py float64) int {
	const steps = 16
	winding := 0
	prevX, prevY := x0, y0
	for i := 1; i <= steps; i++ {
		t := float64(i) / steps
		mt := 1 - t
		x := mt*mt*x0 + 2*mt*t*cx + t*t*x1
		y := mt*mt*y0 + 2*mt*t*cy + t*t*y1
		winding += windingSegment64(prevX, prevY, x, y, px, py)
		prevX, prevY = x, y
	}
	return winding
}

func windingCubic64(x0, y0, c1x, c1y, c2x, c2y, x1, y1, px, py float64) int {
	const steps = 32
	winding := 0
	prevX, prevY := x0, y0
	for i := 1; i <= steps; i++ {
		t := float64(i) / steps
		mt := 1 - t
		x := mt*mt*mt*x0 + 3*mt*mt*t*c1x + 3*mt*t*t*c2x + t*t*t*x1
		y := mt*mt*mt*y0 + 3*mt*mt*t*c1y + 3*mt*t*t*c2y + t*t*t*y1
		winding += windingSegment64(prevX, prevY, x, y, px, py)
		prevX, prevY = x, y
	}
	return winding
}

// glyphRunContains tests the point against each glyph box in run space.
func glyphRunContains(parsed text.ParsedFont, run GlyphRunData, glyphs []GlyphEntry, gx, gy float32) bool {
	hasMetrics := parsed != nil && run.FontSize > 0
	for i := range glyphs {
		x := glyphs[i].X
		y := glyphs[i].Y
		if hasMetrics {
			gb := parsed.GlyphBounds(uint16(glyphs[i].GlyphID), float64(run.FontSize))
			box := Rect{
				MinX: x + float32(gb.MinX),
				MinY: y + float32(gb.MinY),
				MaxX: x + float32(gb.MaxX),
				MaxY: y + float32(gb.MaxY),
			}
			if !box.IsEmpty() {
				if rectContains(box, gx, gy) {
					return true
				}
				continue
			}
		}
		adv := glyphAdvanceFallback(parsed, glyphs[i].GlyphID, run.FontSize)
		if adv <= 0 {
			continue
		}
		if rectContains(Rect{MinX: x, MinY: y - run.FontSize, MaxX: x + adv, MaxY: y}, gx, gy) {
			return true
		}
	}
	return false
}

// glyphRunBounds unions the per-glyph boxes in run space.
func glyphRunBounds(parsed text.ParsedFont, run GlyphRunData, glyphs []GlyphEntry) Rect {
	hasMetrics := parsed != nil && run.FontSize > 0
	bounds := EmptyRect()
	for i := range glyphs {
		x := glyphs[i].X
		y := glyphs[i].Y
		if hasMetrics {
			gb := parsed.GlyphBounds(uint16(glyphs[i].GlyphID), float64(run.FontSize))
			box := Rect{
				MinX: x + float32(gb.MinX),
				MinY: y + float32(gb.MinY),
				MaxX: x + float32(gb.MaxX),
				MaxY: y + float32(gb.MaxY),
			}
			if !box.IsEmpty() {
				bounds = bounds.Union(box)
				continue
			}
		}
		adv := glyphAdvanceFallback(parsed, glyphs[i].GlyphID, run.FontSize)
		if adv <= 0 {
			continue
		}
		bounds = bounds.Union(Rect{MinX: x, MinY: y - run.FontSize, MaxX: x + adv, MaxY: y})
	}
	return bounds
}

func glyphAdvanceFallback(parsed text.ParsedFont, gid text.GlyphID, size float32) float32 {
	if parsed != nil && size > 0 {
		if adv := parsed.GlyphAdvance(uint16(gid), float64(size)); adv > 0 {
			return float32(adv)
		}
	}
	if size > 0 {
		return size / 2
	}
	return 0
}

func toStrokeCap(c LineCap) stroke.LineCap {
	switch c {
	case LineCapRound:
		return stroke.LineCapRound
	case LineCapSquare:
		return stroke.LineCapSquare
	default:
		return stroke.LineCapButt
	}
}

func toStrokeJoin(j LineJoin) stroke.LineJoin {
	switch j {
	case LineJoinRound:
		return stroke.LineJoinRound
	case LineJoinBevel:
		return stroke.LineJoinBevel
	default:
		return stroke.LineJoinMiter
	}
}

// imagePixelOpaque reports whether the pixel under (u, v) has non-zero alpha.
// Out-of-range coordinates are treated as opaque so rectangle testing governs
// edges. Images without pixel data are treated as opaque.
func imagePixelOpaque(img *Image, u, v float32) bool {
	if len(img.Data) < img.Width*img.Height*4 {
		return true
	}
	ix := int(math.Floor(float64(u)))
	iy := int(math.Floor(float64(v)))
	ix = min(max(ix, 0), img.Width-1)
	iy = min(max(iy, 0), img.Height-1)
	off := iy*img.Width*4 + ix*4
	if off+3 >= len(img.Data) {
		return true
	}
	return img.Data[off+3] != 0
}

// --- Little-endian readers -------------------------------------------------

func readUint16LE(b []byte) uint32 {
	if len(b) < 2 {
		return 0
	}
	return uint32(b[0]) | uint32(b[1])<<8
}

func readUint32LE(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func readUint64LE(b []byte) uint64 {
	if len(b) < 8 {
		return 0
	}
	return uint64(readUint32LE(b)) | uint64(readUint32LE(b[4:]))<<32
}
