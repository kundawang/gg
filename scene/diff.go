package scene

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// This file implements auditable frame-to-frame diffing of two Encodings.
//
// The hot-reload use case changes only a handful of primitives per frame, but
// previously callers had to re-compare the whole scene themselves. DiffEncodings
// parses both encodings into self-describing command records, aligns them with
// stable identity hashes (command type + geometry, excluding paint and
// resources), and reports exactly which commands were added, removed, or
// changed — including transform, clip, layer (filter/compositing), text, and
// image-resource changes. Reordering is detected via relative-order inversion,
// and same-tag substitution pairs geometry edits that lost their identity.
//
// The diff is purely read-only: neither input encoding nor any shared cache
// (LayerCache, glyph cache, pools) is mutated. All temporary state is local.

// Sentinel errors returned by DiffEncodings. All are wrapped with contextual
// information; test with errors.Is rather than string matching.
var (
	// ErrNilEncoding is returned when either encoding argument is nil.
	ErrNilEncoding = errors.New("scene: nil encoding")

	// ErrDuplicateID is returned when a resource registry maps one semantic
	// identifier to conflicting payloads (for example, one FontSourceID used
	// with two different font sizes in the same encoding).
	ErrDuplicateID = errors.New("scene: duplicate command or resource id")

	// ErrInvalidResource is returned when a command references a brush, image,
	// or font resource that does not exist in the encoding.
	ErrInvalidResource = errors.New("scene: invalid resource reference")

	// ErrSceneTooLarge is returned when an encoding exceeds the maximum number
	// of commands that can be diffed (see MaxDiffCommands).
	ErrSceneTooLarge = errors.New("scene: encoding exceeds maximum diff size")

	// ErrMalformedEncoding is returned when an encoding's data streams are
	// truncated or otherwise inconsistent with its tag stream.
	ErrMalformedEncoding = errors.New("scene: malformed encoding")
)

// MaxDiffCommands bounds the number of tag-stream commands a single encoding
// may contain when diffing. Move detection is O(k^2) in the number k of matched
// commands, so this guard keeps pathological inputs from consuming unbounded
// time and memory.
const MaxDiffCommands = 1_000_000

// ChangeKind categorizes a single command-level diff entry.
type ChangeKind string

// Change kind constants used in CommandChange.Kind.
const (
	// ChangeAdded: the command exists only in the new encoding.
	ChangeAdded ChangeKind = "added"
	// ChangeRemoved: the command exists only in the old encoding.
	ChangeRemoved ChangeKind = "removed"
	// ChangeChanged: the command exists in both encodings but its content,
	// transform, clip, layer parameters, or resources differ.
	ChangeChanged ChangeKind = "changed"
)

// ResourceKind identifies which resource pool a ResourceDiff describes.
type ResourceKind string

// Resource kind constants used in ResourceDiff.Kind.
const (
	ResourceBrush ResourceKind = "brush"
	ResourceImage ResourceKind = "image"
	ResourceFont  ResourceKind = "font"
)

// Aspect names identify the command aspect that changed. They appear in
// CommandChange.Aspects and are stable strings suitable for log filters and
// JSON consumers.
const (
	AspectGeometry  = "geometry"
	AspectPaint     = "paint"
	AspectStyle     = "style"
	AspectTransform = "transform"
	AspectClip      = "clip"
	AspectLayer     = "layer"
	AspectText      = "text"
	AspectImage     = "image"
	AspectResource  = "resource"
)

// ResourceDiff describes a single resource-level difference attached to a
// changed command. Resource indices are the local indices inside the encoding
// that owns the command.
type ResourceDiff struct {
	// Kind is the resource pool (brush, image, or font).
	Kind ResourceKind `json:"kind"`

	// OldIndex is the resource index referenced by the old command,
	// or -1 when the resource reference itself was added.
	OldIndex int `json:"oldIndex"`

	// NewIndex is the resource index referenced by the new command,
	// or -1 when the resource reference was removed.
	NewIndex int `json:"newIndex"`

	// OldValue is a compact, deterministic description of the old resource
	// (e.g. "rgba(1,0,0,1)", "image#2 64x64", "font#42 size=14").
	OldValue string `json:"oldValue,omitempty"`

	// NewValue is the analogous description of the new resource.
	NewValue string `json:"newValue,omitempty"`
}

// CommandChange is one auditable entry in a SceneDiff.
type CommandChange struct {
	// Kind is added, removed, or changed.
	Kind ChangeKind `json:"kind"`

	// ID is the command's stable identity hash (command type + geometry,
	// excluding paint and resources). It identifies the same primitive across
	// frames; added and removed entries carry the hash of their own command.
	ID uint64 `json:"id"`

	// Tag is the command tag (Fill, Stroke, Transform, BeginClip, ...).
	Tag Tag `json:"tag"`

	// TagName is the human-readable tag name, kept explicit so JSON output
	// stays readable without a client-side enum table.
	TagName string `json:"tagName"`

	// OldIndex is the command's zero-based position in the old encoding's
	// host (tag) order, or -1 for added commands.
	OldIndex int `json:"oldIndex"`

	// NewIndex is the command's zero-based position in the new encoding's
	// host (tag) order, or -1 for removed commands.
	NewIndex int `json:"newIndex"`

	// Moved is true for a changed command whose host order position shifted.
	Moved bool `json:"moved,omitempty"`

	// OldBounds is the command's bounds in the old encoding. It is the empty
	// Rect for added commands.
	OldBounds Rect `json:"oldBounds"`

	// NewBounds is the command's bounds in the new encoding. It is the empty
	// Rect for removed commands.
	NewBounds Rect `json:"newBounds"`

	// Aspects lists the changed aspects (geometry, paint, transform, clip,
	// layer, text, image, resource, style). Sorted and de-duplicated; empty
	// for added/removed entries.
	Aspects []string `json:"aspects,omitempty"`

	// Resources lists resource-level differences (brushes, images, fonts).
	// Empty when no resource changed.
	Resources []ResourceDiff `json:"resources,omitempty"`
}

// SceneDiff is the auditable result of comparing two encodings.
//
// All three slices are ordered by host draw order: removed entries by old
// index, added and changed entries by new index. Within the result slice the
// groups appear as Added, then Removed, then Changed; each group is itself
// index ordered, so serializing the same logical diff always yields the same
// bytes.
type SceneDiff struct {
	Added   []CommandChange `json:"added"`
	Removed []CommandChange `json:"removed"`
	Changed []CommandChange `json:"changed"`
}

// Empty reports whether the diff contains no changes.
func (d *SceneDiff) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// ChangedBounds returns the union of every affected command's bounds.
// For changed commands both old and new bounds are unioned so the rectangle
// covers both the area to repaint and the area vacated; added commands
// contribute new bounds, removed commands contribute old bounds.
func (d *SceneDiff) ChangedBounds() Rect {
	u := EmptyRect()
	for i := range d.Added {
		u = u.Union(d.Added[i].NewBounds)
	}
	for i := range d.Removed {
		u = u.Union(d.Removed[i].OldBounds)
	}
	for i := range d.Changed {
		u = u.Union(d.Changed[i].OldBounds)
		u = u.Union(d.Changed[i].NewBounds)
	}
	return u
}

// DiffOptions carries optional context for a frame diff. The zero value is
// valid and performs all checks that can be derived from the encodings alone.
type DiffOptions struct {
	// OldImageCount / NewImageCount are the sizes of the image registries
	// backing the two encodings (see Scene.Images). When non-zero, every
	// TagImage reference is range-checked against the matching count and an
	// out-of-range index returns ErrInvalidResource. Zero disables image
	// range validation because an encoding on its own does not carry the
	// registry.
	OldImageCount int
	NewImageCount int
}

// DiffEncodings computes the auditable frame-to-frame diff between two
// encodings with default options. Either argument may be an empty encoding
// (len(Tags) == 0); nil arguments return ErrNilEncoding.
//
// The function never mutates either encoding and touches no shared cache.
func DiffEncodings(oldEnc, newEnc *Encoding) (*SceneDiff, error) {
	return DiffEncodingsWithOptions(oldEnc, newEnc, DiffOptions{})
}

// DiffEncodingsWithOptions is DiffEncodings with explicit resource context.
func DiffEncodingsWithOptions(oldEnc, newEnc *Encoding, opts DiffOptions) (*SceneDiff, error) {
	if oldEnc == nil || newEnc == nil {
		which := "old"
		if newEnc == nil {
			which = "new"
		}
		return nil, fmt.Errorf("%w: %s encoding is nil", ErrNilEncoding, which)
	}

	oldCmds, err := parseCommands(oldEnc, "old")
	if err != nil {
		return nil, err
	}
	newCmds, err := parseCommands(newEnc, "new")
	if err != nil {
		return nil, err
	}

	if err := validateResources(oldCmds, "old", opts.OldImageCount); err != nil {
		return nil, err
	}
	if err := validateResources(newCmds, "new", opts.NewImageCount); err != nil {
		return nil, err
	}

	diff := alignAndDiff(oldCmds, newCmds)
	return diff, nil
}

// ---------------------------------------------------------------------------
// Parsed command records
// ---------------------------------------------------------------------------

// commandRecord is a self-contained, value-copied view of one logical command.
// All slices are copied out of the encoding so the record never aliases the
// input's backing arrays; comparisons therefore cannot be fooled by two
// encodings sharing a temporary slice that is later overwritten.
type commandRecord struct {
	tag        Tag
	index      int // byte offset of the command's first tag in the tag stream
	ordinal    int // zero-based host draw order among logical commands
	pathFloats []float32
	drawWords  []uint32
	textBytes  []byte
	transforms []Affine
	brushRefs  []int   // brush indices referenced by this command
	brushVals  []Brush // resolved brush values, parallel to brushRefs
	imageRefs  []int   // image indices referenced by this command
	fontRefs   []uint64
	bounds     Rect

	// identityID identifies the same primitive across frames: command tag plus
	// geometry (path coordinates, or the text payload's glyph placement and
	// string with font-resource header fields ignored). Paint, resource slots,
	// and transforms are excluded so color/resource-only edits keep the ID.
	identityID uint64
	// contentID hashes the full command including resolved resource values and
	// transforms. Two commands with equal contentID render identically.
	contentID uint64
}

// parseCommands walks an encoding's tag stream and produces one record per
// folded into the draw command that consumes the path, matching how a host
// thinks about draw order.
// commandParser walks an Encoding's streams once, emitting one commandRecord
// per logical command. Path construction verbs are folded into the draw or
// clip command that consumes them.
type commandParser struct {
	e                                   *Encoding
	label                               string
	pathIdx, drawIdx, transIdx, textIdx int
	cur                                 commandRecord
	records                             []commandRecord
}

// parseCommands walks an encoding's tag stream and produces one record per
// logical command.
func parseCommands(e *Encoding, label string) ([]commandRecord, error) {
	if len(e.tags) > MaxDiffCommands {
		return nil, fmt.Errorf("%w: %s encoding has %d tags (limit %d)",
			ErrSceneTooLarge, label, len(e.tags), MaxDiffCommands)
	}
	p := &commandParser{
		e:       e,
		label:   label,
		records: make([]commandRecord, 0, len(e.tags)),
		cur:     commandRecord{index: -1},
	}
	if err := p.run(); err != nil {
		return nil, err
	}
	return p.records, nil
}

func (p *commandParser) run() error {
	for tagIndex, tag := range p.e.tags {
		if err := p.checkStreams(tagIndex, tag); err != nil {
			return err
		}
		if err := p.dispatch(tagIndex, tag); err != nil {
			return err
		}
	}
	return p.checkFullyConsumed()
}

// streamKind selects which data stream a requirement applies to.
type streamKind uint8

const (
	streamPath streamKind = iota
	streamDraw
	streamTransform
	streamText
)

// streamRequirement is the number of entries a tag consumes from a stream.
type streamRequirement struct {
	kind streamKind
	need int
	msg  string
}

// streamRequirements maps each payload-bearing tag to its stream needs.
var streamRequirements = map[Tag]streamRequirement{
	TagMoveTo:        {streamPath, 2, "truncated path data"},
	TagLineTo:        {streamPath, 2, "truncated path data"},
	TagQuadTo:        {streamPath, 4, "truncated path data"},
	TagCubicTo:       {streamPath, 6, "truncated path data"},
	TagBrush:         {streamPath, 4, "truncated brush data"},
	TagFill:          {streamDraw, 2, "truncated draw data"},
	TagFillRoundRect: {streamDraw, 2, "truncated draw data"},
	TagSetAntiAlias:  {streamDraw, 1, "truncated draw data"},
	TagImage:         {streamDraw, 1, "truncated draw data"},
	TagStroke:        {streamDraw, 5, "truncated stroke data"},
	TagPushLayer:     {streamDraw, 2, "truncated layer data"},
	TagTransform:     {streamTransform, 1, "truncated transform stream"},
	TagText:          {streamText, glyphRunDataSize, "truncated text header"},
}

// checkStreams validates that the tag's expected payload is present in its
// stream before the payload is read.
func (p *commandParser) checkStreams(tagIndex int, tag Tag) error {
	req, ok := streamRequirements[tag]
	if !ok {
		return nil
	}
	if p.streamPosition(req.kind)+req.need > p.streamLength(req.kind) {
		return malformed(p.label, tagIndex, tag, req.msg)
	}
	return nil
}

func (p *commandParser) streamPosition(k streamKind) int {
	switch k {
	case streamPath:
		return p.pathIdx
	case streamDraw:
		return p.drawIdx
	case streamTransform:
		return p.transIdx
	default:
		return p.textIdx
	}
}

func (p *commandParser) streamLength(k streamKind) int {
	switch k {
	case streamPath:
		return len(p.e.pathData)
	case streamDraw:
		return len(p.e.drawData)
	case streamTransform:
		return len(p.e.transforms)
	default:
		return len(p.e.textData)
	}
}

// dispatch routes one tag to its handler.
func (p *commandParser) dispatch(tagIndex int, tag Tag) error {
	switch {
	case tag == TagBeginPath:
		if p.cur.index < 0 {
			p.cur.index = tagIndex
		}
		p.cur.tag = 0 // path is absorbed into the consuming command
	case tag == TagEndPath || tag == TagClosePath:
		// End markers carry no payload; geometry in cur is retained.
	case tag.IsPathCommand():
		p.handlePathVerb(tagIndex, tag)
	case tag == TagFill, tag == TagFillRoundRect, tag == TagStroke:
		return p.handleDraw(tagIndex, tag)
	case tag == TagImage:
		return p.handleImage(tagIndex, tag)
	case tag == TagText:
		return p.handleText(tagIndex)
	case tag == TagTransform:
		p.handleTransform(tagIndex)
	case tag == TagSetAntiAlias:
		p.handleAntiAlias(tagIndex)
	case tag == TagPushLayer, tag == TagPopLayer:
		p.handleLayer(tagIndex, tag)
	case tag == TagBeginClip, tag == TagEndClip:
		p.handleClip(tagIndex, tag)
	case tag == TagBrush:
		p.handleStandaloneBrush(tagIndex)
	default:
		return malformed(p.label, tagIndex, tag, "unknown tag")
	}
	return nil
}

func (p *commandParser) handlePathVerb(tagIndex int, tag Tag) {
	n := tag.DataSize()
	p.cur.pathFloats = append(p.cur.pathFloats, p.e.pathData[p.pathIdx:p.pathIdx+n]...)
	p.pathIdx += n
	if p.cur.index < 0 {
		p.cur.index = tagIndex
	}
}

func (p *commandParser) handleDraw(tagIndex int, tag Tag) error {
	if p.cur.index < 0 {
		p.cur.index = tagIndex
	}
	p.cur.tag = tag
	p.cur.drawWords = append(p.cur.drawWords, consumeDraw(p.e, tag, &p.drawIdx)...)
	if tag == TagFillRoundRect {
		p.cur.pathFloats = append(p.cur.pathFloats, p.e.pathData[p.pathIdx:p.pathIdx+6]...)
		p.pathIdx += 6
	}
	if err := attachBrushRefs(&p.cur, p.e, p.label, tagIndex); err != nil {
		return err
	}
	p.flush(p.cur)
	p.cur = commandRecord{index: -1}
	return nil
}

func (p *commandParser) handleImage(tagIndex int, tag Tag) error {
	rec := commandRecord{tag: tag, index: tagIndex}
	rec.drawWords = append(rec.drawWords, p.e.drawData[p.drawIdx])
	p.drawIdx++
	imgIdx := int(rec.drawWords[0])
	if imgIdx < 0 {
		return invalidResource(p.label, tagIndex, tag, fmt.Sprintf("image index %d", imgIdx))
	}
	rec.imageRefs = append(rec.imageRefs, imgIdx)
	if p.transIdx >= len(p.e.transforms) {
		return malformed(p.label, tagIndex, tag, "image missing transform")
	}
	rec.transforms = append(rec.transforms, p.e.transforms[p.transIdx])
	p.transIdx++
	rec.bounds = imageBounds(imgIdx, rec.transforms[0])
	p.flush(rec)
	return nil
}

func (p *commandParser) handleText(tagIndex int) error {
	rec, n, err := parseTextRecord(p.e, p.textIdx, tagIndex, p.label)
	if err != nil {
		return err
	}
	p.textIdx += n
	p.flush(rec)
	return nil
}

func (p *commandParser) handleTransform(tagIndex int) {
	rec := commandRecord{tag: TagTransform, index: tagIndex}
	rec.transforms = append(rec.transforms, p.e.transforms[p.transIdx])
	p.transIdx++
	p.flush(rec)
}

func (p *commandParser) handleAntiAlias(tagIndex int) {
	rec := commandRecord{tag: TagSetAntiAlias, index: tagIndex}
	rec.drawWords = append(rec.drawWords, p.e.drawData[p.drawIdx])
	p.drawIdx++
	p.flush(rec)
}

func (p *commandParser) handleLayer(tagIndex int, tag Tag) {
	rec := commandRecord{tag: tag, index: tagIndex}
	if tag == TagPushLayer {
		rec.drawWords = append(rec.drawWords, p.e.drawData[p.drawIdx], p.e.drawData[p.drawIdx+1])
		p.drawIdx += 2
	}
	p.flush(rec)
}

func (p *commandParser) handleClip(tagIndex int, tag Tag) {
	rec := commandRecord{tag: tag, index: tagIndex}
	// BeginClip inherits the geometry of the path that immediately precedes it.
	if tag == TagBeginClip && len(p.cur.pathFloats) > 0 {
		rec.pathFloats = append(rec.pathFloats, p.cur.pathFloats...)
		rec.index = p.cur.index
		p.cur = commandRecord{index: -1}
	}
	p.flush(rec)
}

func (p *commandParser) handleStandaloneBrush(tagIndex int) {
	rec := commandRecord{tag: TagBrush, index: tagIndex}
	rec.pathFloats = append(rec.pathFloats, p.e.pathData[p.pathIdx:p.pathIdx+4]...)
	p.pathIdx += 4
	p.flush(rec)
}

func (p *commandParser) flush(rec commandRecord) {
	rec.ordinal = len(p.records)
	finalizeRecord(&rec)
	p.records = append(p.records, rec)
}

func (p *commandParser) checkFullyConsumed() error {
	e := p.e
	if p.pathIdx != len(e.pathData) {
		return fmt.Errorf("%w: %s encoding has %d unconsumed path floats",
			ErrMalformedEncoding, p.label, len(e.pathData)-p.pathIdx)
	}
	if p.drawIdx != len(e.drawData) {
		return fmt.Errorf("%w: %s encoding has %d unconsumed draw words",
			ErrMalformedEncoding, p.label, len(e.drawData)-p.drawIdx)
	}
	if p.transIdx != len(e.transforms) {
		return fmt.Errorf("%w: %s encoding has %d unconsumed transforms",
			ErrMalformedEncoding, p.label, len(e.transforms)-p.transIdx)
	}
	if p.textIdx != len(e.textData) {
		return fmt.Errorf("%w: %s encoding has %d unconsumed text bytes",
			ErrMalformedEncoding, p.label, len(e.textData)-p.textIdx)
	}
	return nil
}

// consumeDraw copies the drawData words belonging to a draw tag.
func consumeDraw(e *Encoding, tag Tag, drawIdx *int) []uint32 {
	var n int
	switch tag {
	case TagFill, TagFillRoundRect:
		n = 2
	case TagStroke:
		n = 5
	default:
		n = 0
	}
	words := append([]uint32(nil), e.drawData[*drawIdx:*drawIdx+n]...)
	*drawIdx += n
	return words
}

// attachBrushRefs validates and records the brush reference of a draw command,
// capturing the resolved brush value so comparisons are independent of the
// brush's slot index (identical colors in different slots compare equal).
func attachBrushRefs(rec *commandRecord, e *Encoding, label string, tagIndex int) error {
	if len(rec.drawWords) == 0 {
		return nil
	}
	brushIdx := int(rec.drawWords[0])
	if brushIdx < 0 || brushIdx >= len(e.brushes) {
		return invalidResource(label, tagIndex, rec.tag,
			fmt.Sprintf("brush index %d (have %d brushes)", brushIdx, len(e.brushes)))
	}
	rec.brushRefs = append(rec.brushRefs, brushIdx)
	rec.brushVals = append(rec.brushVals, e.brushes[brushIdx])
	return nil
}

// parseTextRecord decodes one TagText payload into a record. It copies the
// raw text bytes so the record does not alias e.textData.
func parseTextRecord(e *Encoding, off, tagIndex int, label string) (commandRecord, int, error) {
	rec := commandRecord{tag: TagText, index: tagIndex}
	if off+glyphRunDataSize > len(e.textData) {
		return rec, 0, malformed(label, tagIndex, TagText, "truncated text header")
	}
	glyphCount := int(binary.LittleEndian.Uint16(e.textData[off+12:]))
	textLen := int(binary.LittleEndian.Uint16(e.textData[off+28:]))
	total := glyphRunDataSize + glyphCount*glyphEntrySize + textLen
	if off+total > len(e.textData) {
		return rec, 0, malformed(label, tagIndex, TagText, "truncated glyph or string data")
	}

	rec.textBytes = append(rec.textBytes, e.textData[off:off+total]...)
	rec.drawWords = append(rec.drawWords,
		binary.LittleEndian.Uint32(e.textData[off+24:])) // BrushIndex

	brushIdx := int(rec.drawWords[0])
	if brushIdx < 0 || brushIdx >= len(e.brushes) {
		return rec, 0, invalidResource(label, tagIndex, TagText,
			fmt.Sprintf("brush index %d (have %d brushes)", brushIdx, len(e.brushes)))
	}
	rec.brushRefs = append(rec.brushRefs, brushIdx)
	rec.brushVals = append(rec.brushVals, e.brushes[brushIdx])
	fontID := binary.LittleEndian.Uint64(e.textData[off:])
	rec.fontRefs = append(rec.fontRefs, fontID)

	// Bounds: approximate from glyph origins plus font size; the encoding does
	// not store a tighter text bounds, so use the origin-to-last-glyph extent.
	originX := math.Float32frombits(binary.LittleEndian.Uint32(e.textData[off+16:]))
	originY := math.Float32frombits(binary.LittleEndian.Uint32(e.textData[off+20:]))
	fontSize := math.Float32frombits(binary.LittleEndian.Uint32(e.textData[off+8:]))
	maxX := originX
	for i := range glyphCount {
		gx := math.Float32frombits(binary.LittleEndian.Uint32(
			e.textData[off+glyphRunDataSize+i*glyphEntrySize+2:]))
		if gx > maxX-originX {
			maxX = originX + gx
		}
	}
	rec.bounds = Rect{
		MinX: originX,
		MinY: originY - fontSize,
		MaxX: maxX + fontSize,
		MaxY: originY + fontSize*0.25,
	}
	return rec, total, nil
}

// imageBounds computes the bounds of an image command from its transform.
// Encodings are stream-only and do not store the image registry (that lives on
// Scene), so an image's intrinsic size is not available here; unit-box bounds
// transformed by the command's matrix give a stable, locatable rectangle.
// Scene-level callers can refine it via Scene.TaggedBounds.
func imageBounds(imgIdx int, t Affine) Rect {
	r := Rect{MinX: 0, MinY: 0, MaxX: 1, MaxY: 1}
	if !t.IsIdentity() {
		r = transformBounds(r, t)
	}
	_ = imgIdx
	return r
}

// finalizeRecord computes a record's bounds (when not already set) and its
// identity and content hashes.
func finalizeRecord(rec *commandRecord) {
	if rec.bounds.IsEmpty() {
		rec.bounds = boundsForRecord(rec)
	}
	rec.identityID = hashIdentity(rec)
	rec.contentID = hashContent(rec)
}

// textIdentityBytes returns the text payload with the font-resource header
// fields (FontSourceID, FontSize, Flags, BrushIndex) zeroed, so the identity
// key reflects glyph placement and string content, not the bound font.
func textIdentityBytes(rec *commandRecord) []byte {
	if rec.tag != TagText || len(rec.textBytes) < glyphRunDataSize {
		return rec.textBytes
	}
	b := append([]byte(nil), rec.textBytes...)
	// FontSourceID: bytes 0..7, FontSize: 8..11, Flags: 12..13,
	// OriginX/Y: 16..23, BrushIndex: 24..27. Zero the resource/style fields
	// while retaining glyph entries and the trailing UTF-8 string.
	for _, rng := range [][2]int{{0, 8}, {8, 12}, {12, 14}, {24, 28}} {
		for k := rng[0]; k < rng[1]; k++ {
			b[k] = 0
		}
	}
	return b
}

// boundsForRecord derives bounds from the record's own data.
func boundsForRecord(rec *commandRecord) Rect {
	switch rec.tag {
	case TagFillRoundRect:
		if len(rec.pathFloats) >= 4 {
			return Rect{
				MinX: rec.pathFloats[0],
				MinY: rec.pathFloats[1],
				MaxX: rec.pathFloats[2],
				MaxY: rec.pathFloats[3],
			}
		}
	case TagTransform, TagSetAntiAlias, TagPushLayer, TagPopLayer,
		TagEndClip, TagBrush:
		return EmptyRect()
	}
	// Fill/Stroke paths and BeginClip geometry: union every coordinate pair.
	r := EmptyRect()
	for i := 0; i+1 < len(rec.pathFloats); i += 2 {
		r = r.UnionPoint(rec.pathFloats[i], rec.pathFloats[i+1])
	}
	return r
}

// ---------------------------------------------------------------------------
// Stable hashing
// ---------------------------------------------------------------------------

const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

type fnvHasher struct{ h uint64 }

func newFnv() *fnvHasher { return &fnvHasher{h: fnvOffset64} }

func (x *fnvHasher) byte(v byte) {
	x.h ^= uint64(v)
	x.h *= fnvPrime64
}

func (x *fnvHasher) u32(v uint32) {
	x.h ^= uint64(v)
	x.h *= fnvPrime64
}

func (x *fnvHasher) f32(v float32) { x.u32(math.Float32bits(v)) }

func (x *fnvHasher) bytes(b []byte) {
	for _, c := range b {
		x.byte(c)
	}
}

func (x *fnvHasher) floats(f []float32) {
	for _, v := range f {
		x.f32(v)
	}
}

// hashIdentity is the LCS alignment key. It identifies the same command across
// frames by command type plus geometry (path coordinates, or the text payload's
// glyph placement and string with font-resource header fields ignored). Paint,
// brush slot, image index, font binding, and transform are excluded so a
// color-only or resource-only edit keeps its identity and aligns as changed.
//
// Geometry is part of the key, so two distinct primitives (a rect at (0,0) and
// a rect at (40,40)) never collapse onto one ID; reordering them is detected as
// a move. A command whose geometry itself changes (moved/resized) aligns by tag
// and ordinal in the tier-2 substitution pass.
//
// Resource *references* and paint slot indices are deliberately excluded:
// brush slots are positional, so inserting a draw earlier shifts every later
// slot without changing any rendering.
//
// Repeated identity IDs (two identical rectangles) are legal and aligned by
// occurrence in host order.
func hashIdentity(rec *commandRecord) uint64 {
	x := newFnv()
	x.byte(byte(rec.tag))
	if rec.tag == TagText {
		x.bytes(textIdentityBytes(rec))
	} else {
		x.floats(rec.pathFloats)
	}
	return x.h
}

// hashContent hashes every payload field plus the resolved resource *values*.
// Equal content IDs mean the commands render identically.
//
// The first draw word of fill/stroke commands is a brush slot index and is
// skipped (use styleWords for the remaining style words); the resolved brush
// value is hashed instead, so the same color in a different slot compares
// equal. Image indices name registry resources and are hashed directly.
func hashContent(rec *commandRecord) uint64 {
	x := newFnv()
	x.byte(byte(rec.tag))
	x.floats(rec.pathFloats)
	for _, w := range styleWords(*rec) {
		x.u32(w)
	}
	x.bytes(rec.textBytes)
	for _, t := range rec.transforms {
		x.f32(t.A)
		x.f32(t.B)
		x.f32(t.C)
		x.f32(t.D)
		x.f32(t.E)
		x.f32(t.F)
	}
	for _, idx := range rec.imageRefs {
		x.u32(uint32(idx))
	}
	for _, b := range rec.brushVals {
		x.u32(uint32(b.Kind))
		x.f32(float32(b.Color.R))
		x.f32(float32(b.Color.G))
		x.f32(float32(b.Color.B))
		x.f32(float32(b.Color.A))
	}
	return x.h
}

// styleWords returns a record's draw words with the leading brush-index word
// removed for draw commands that carry one.
func styleWords(rec commandRecord) []uint32 {
	if rec.tag == TagFill || rec.tag == TagFillRoundRect || rec.tag == TagStroke {
		return safeTail(rec.drawWords)
	}
	return rec.drawWords
}

// fontSignature captures the per-font payload that must be consistent for one
// FontSourceID across all text commands in an encoding.
type fontSignature struct {
	size  float32
	flags TextFlags
}

// validateResources enforces whole-encoding resource integrity:
//
//   - When imageCount > 0, every TagImage index must be within the registry.
//     (Brush and per-command font validity are already checked while parsing.)
//   - A font registry keyed by FontSourceID must map each id to one payload.
//     Two text runs sharing a font is normal and allowed, but the same id
//     carrying different font sizes/flags indicates a corrupted registry and
//     is reported as ErrDuplicateID.
//
// Duplicate command geometry (two identical rectangles) is explicitly legal:
// equal structural IDs are aligned by occurrence, so command IDs are not
// required to be unique.
func validateResources(cmds []commandRecord, label string, imageCount int) error {
	if imageCount > 0 {
		for k := range cmds {
			for _, imgIdx := range cmds[k].imageRefs {
				if imgIdx >= imageCount {
					return fmt.Errorf("%w: %s image index %d at tag %d exceeds registry size %d",
						ErrInvalidResource, label, imgIdx, cmds[k].index, imageCount)
				}
			}
		}
	}

	// fontSignature captures the per-font payload that must be consistent for
	// one FontSourceID across all text commands in an encoding.
	fontSigs := make(map[uint64]fontSignature)
	for k := range cmds {
		c := &cmds[k]
		if c.tag != TagText {
			continue
		}
		sig, fid := textSignature(*c)
		if prev, ok := fontSigs[fid]; ok && prev != sig {
			return fmt.Errorf("%w: %s font id %d maps to conflicting payloads at index %d",
				ErrDuplicateID, label, fid, c.index)
		}
		fontSigs[fid] = sig
	}
	return nil
}

// textSignature extracts the (size, flags) signature and font id of a text
// record, used to detect a duplicated font id with conflicting payloads.
func textSignature(c commandRecord) (fontSignature, uint64) {
	sig := fontSignature{}
	if len(c.textBytes) < glyphRunDataSize {
		return sig, 0
	}
	fid := binary.LittleEndian.Uint64(c.textBytes[0:])
	sig.size = math.Float32frombits(binary.LittleEndian.Uint32(c.textBytes[8:]))
	sig.flags = TextFlags(binary.LittleEndian.Uint16(c.textBytes[14:]))
	return sig, fid
}

// ---------------------------------------------------------------------------
// Alignment and change computation
// ---------------------------------------------------------------------------

// alignAndDiff aligns two command sequences and classifies every command as
// added, removed, or changed. Matching is keyed on identityID (command tag +
// geometry, excluding paint/resources/transform) and proceeds in three steps:
//
// Step 1 — identity matching. Each old command is paired with the earliest
// still-unpaired new command sharing its identityID, by host order. This pairs
// every primitive that exists in both frames regardless of reordering, and
// aligns repeated identical primitives by occurrence. Move is then derived
// from relative-order inversions among the matched pairs, so both halves of a
// swap are flagged while an insertion that merely shifts later ordinals is not.
//
// Step 2 — same-tag substitution. Commands whose geometry changed (and so lost
// their identity match) are grouped by tag and paired in host order; these are
// geometry/move changes. Genuine surplus commands remain unpaired.
//
// Step 3 — leftovers. Unpaired old commands are removed, unpaired new ones
// added.
//
// A matched pair whose content is byte-identical and which did not move is
// suppressed, yielding an empty diff for unchanged scenes.
// cmdPair couples an old and a new command record by their slice positions.
type cmdPair struct{ oi, ni int }

// alignAndDiff aligns two command sequences and classifies every command as
// added, removed, or changed. See matchIdentityPairs, pairSameTag, and
// collectLeftovers for the three matching steps.
func alignAndDiff(oldCmds, newCmds []commandRecord) *SceneDiff {
	n, m := len(oldCmds), len(newCmds)
	oldUsed := make([]bool, n)
	newUsed := make([]bool, m)

	diff := &SceneDiff{
		Added:   make([]CommandChange, 0),
		Removed: make([]CommandChange, 0),
		Changed: make([]CommandChange, 0),
	}

	// Step 1: identity matching plus move/inversion detection.
	identityPairs := matchIdentityPairs(oldCmds, newCmds, oldUsed, newUsed)
	movedSet := detectMoves(identityPairs)
	for _, p := range identityPairs {
		o, nw := oldCmds[p.oi], newCmds[p.ni]
		moved := movedSet[p.oi]
		if o.contentID == nw.contentID && !moved {
			continue
		}
		if ch := buildChangeWithMove(o, nw, moved); ch != nil {
			diff.Changed = append(diff.Changed, *ch)
		}
	}

	// Step 2: same-tag substitution for geometry edits that lost identity.
	diff.Changed = append(diff.Changed,
		pairSameTag(oldCmds, newCmds, oldUsed, newUsed)...)

	// Step 3: leftovers are removed / added.
	collectLeftovers(diff, oldCmds, newCmds, oldUsed, newUsed)

	// Stable host-order output within each group.
	sort.Slice(diff.Removed, func(a, b int) bool { return diff.Removed[a].OldIndex < diff.Removed[b].OldIndex })
	sort.Slice(diff.Added, func(a, b int) bool { return diff.Added[a].NewIndex < diff.Added[b].NewIndex })
	sort.Slice(diff.Changed, func(a, b int) bool { return diff.Changed[a].NewIndex < diff.Changed[b].NewIndex })
	return diff
}

// matchIdentityPairs pairs every old command with the earliest unpaired new
// command sharing its identityID, processing both sides in host order. Repeated
// identical primitives align by occurrence.
func matchIdentityPairs(oldCmds, newCmds []commandRecord, oldUsed, newUsed []bool) []cmdPair {
	newByID := make(map[uint64][]int)
	for k := range newCmds {
		id := newCmds[k].identityID
		newByID[id] = append(newByID[id], k)
	}
	var pairs []cmdPair
	for k := range oldCmds {
		id := oldCmds[k].identityID
		candidates := newByID[id]
		if len(candidates) == 0 {
			continue
		}
		ni := candidates[0]
		newByID[id] = candidates[1:]
		pairs = append(pairs, cmdPair{oi: k, ni: ni})
		oldUsed[k] = true
		newUsed[ni] = true
	}
	return pairs
}

// detectMoves returns the set of old-command indices whose relative order
// changed among the identity-matched pairs. Both halves of an inversion (a
// swap) are reported; a uniform shift from an earlier insertion is not.
func detectMoves(pairs []cmdPair) map[int]bool {
	oldOrder := append([]cmdPair(nil), pairs...)
	sort.Slice(oldOrder, func(a, b int) bool { return oldOrder[a].oi < oldOrder[b].oi })
	newOrder := append([]cmdPair(nil), pairs...)
	sort.Slice(newOrder, func(a, b int) bool { return newOrder[a].ni < newOrder[b].ni })
	newPosOf := make(map[int]int, len(newOrder))
	for pos, p := range newOrder {
		newPosOf[p.oi] = pos
	}
	moved := make(map[int]bool)
	for a := 0; a < len(oldOrder); a++ {
		for b := a + 1; b < len(oldOrder); b++ {
			if newPosOf[oldOrder[a].oi] > newPosOf[oldOrder[b].oi] {
				moved[oldOrder[a].oi] = true
				moved[oldOrder[b].oi] = true
			}
		}
	}
	return moved
}

// pairSameTag groups unmatched commands by tag and pairs them in host order,
// producing changed entries for geometry edits.
func pairSameTag(oldCmds, newCmds []commandRecord, oldUsed, newUsed []bool) []CommandChange {
	oldByTag := make(map[Tag][]int)
	newByTag := make(map[Tag][]int)
	for k := range oldCmds {
		if !oldUsed[k] {
			tg := oldCmds[k].tag
			oldByTag[tg] = append(oldByTag[tg], k)
		}
	}
	for k := range newCmds {
		if !newUsed[k] {
			tg := newCmds[k].tag
			newByTag[tg] = append(newByTag[tg], k)
		}
	}
	var changed []CommandChange
	for tg, ol := range oldByTag {
		nl := newByTag[tg]
		pairs := min(len(ol), len(nl))
		for k := range pairs {
			o, nw := oldCmds[ol[k]], newCmds[nl[k]]
			if ch := buildChange(o, nw); ch != nil {
				changed = append(changed, *ch)
			}
			oldUsed[ol[k]] = true
			newUsed[nl[k]] = true
		}
	}
	return changed
}

// collectLeftovers turns unpaired old commands into removed entries and
// unpaired new commands into added entries.
func collectLeftovers(diff *SceneDiff, oldCmds, newCmds []commandRecord, oldUsed, newUsed []bool) {
	for k := range oldCmds {
		if oldUsed[k] {
			continue
		}
		c := oldCmds[k]
		diff.Removed = append(diff.Removed, CommandChange{
			Kind: ChangeRemoved, ID: c.identityID, Tag: c.tag, TagName: c.tag.String(),
			OldIndex: c.index, NewIndex: -1, OldBounds: c.bounds,
		})
	}
	for k := range newCmds {
		if newUsed[k] {
			continue
		}
		c := newCmds[k]
		diff.Added = append(diff.Added, CommandChange{
			Kind: ChangeAdded, ID: c.identityID, Tag: c.tag, TagName: c.tag.String(),
			OldIndex: -1, NewIndex: c.index, NewBounds: c.bounds,
		})
	}
}

// buildChange compares two matched records and returns a CommandChange with
// aspect and resource details, or nil if they are content-identical and not
// moved. Move is derived from ordinal order and is appropriate for phase-2
// substitution pairs.
func buildChange(o, n commandRecord) *CommandChange {
	return buildChangeWithMove(o, n, o.ordinal != n.ordinal)
}

// buildChangeWithMove is buildChange with an explicit moved flag, used by the
// phase-1 alignment where move is decided by anchor rank rather than ordinal.
func buildChangeWithMove(o, n commandRecord, moved bool) *CommandChange {
	aspects := make(map[string]struct{})
	var resources []ResourceDiff

	if !floatSlicesEqual(o.pathFloats, n.pathFloats) {
		aspects[AspectGeometry] = struct{}{}
	}
	// Compare only the non-brush draw words so a pure color change is not
	// mislabeled as a style change. The brush word is handled by brushDiff.
	if styleWordsDiffer(o, n) {
		aspects[AspectStyle] = struct{}{}
	}
	if !bytes.Equal(o.textBytes, n.textBytes) {
		aspects[AspectText] = struct{}{}
	}
	if !affineSlicesEqual(o.transforms, n.transforms) {
		aspects[AspectTransform] = struct{}{}
	}
	if o.tag == TagBeginClip || n.tag == TagBeginClip {
		if !floatSlicesEqual(o.pathFloats, n.pathFloats) {
			aspects[AspectClip] = struct{}{}
		}
	}
	if o.tag == TagPushLayer || n.tag == TagPushLayer {
		if !wordSlicesEqual(o.drawWords, n.drawWords) {
			aspects[AspectLayer] = struct{}{}
		}
	}

	// Brush (paint) differences, resolved by value.
	if rd, changed := brushDiff(o, n); changed {
		resources = append(resources, rd)
		aspects[AspectPaint] = struct{}{}
		aspects[AspectResource] = struct{}{}
	}
	// Image reference differences.
	if rd, changed := imageRefDiff(o, n); changed {
		resources = append(resources, rd)
		aspects[AspectImage] = struct{}{}
		aspects[AspectResource] = struct{}{}
	}
	// Font (text resource) differences.
	if rd, changed := fontDiff(o, n); changed {
		resources = append(resources, rd)
		aspects[AspectText] = struct{}{}
		aspects[AspectResource] = struct{}{}
	}

	// moved is supplied by the caller based on alignment context (anchor rank
	// for phase 1, ordinal order for phase 2).
	if len(aspects) == 0 && !moved {
		return nil
	}

	aspectList := make([]string, 0, len(aspects))
	for a := range aspects {
		aspectList = append(aspectList, a)
	}
	sort.Strings(aspectList)

	return &CommandChange{
		Kind:      ChangeChanged,
		ID:        n.identityID,
		Tag:       n.tag,
		TagName:   n.tag.String(),
		OldIndex:  o.index,
		NewIndex:  n.index,
		Moved:     moved,
		OldBounds: o.bounds,
		NewBounds: n.bounds,
		Aspects:   aspectList,
		Resources: resources,
	}
}

// brushDiff compares the resolved brush values of two records. Only an actual
// paint-value change is reported; a brush slot index that shifts because an
// earlier command was inserted is ignored (the rendered color is unchanged).
func brushDiff(o, n commandRecord) (ResourceDiff, bool) {
	if len(o.brushRefs) == 0 && len(n.brushRefs) == 0 {
		return ResourceDiff{}, false
	}
	oi, ni := -1, -1
	var ov, nv string
	if len(o.brushRefs) > 0 {
		oi = o.brushRefs[0]
		ov = describeBrush(oi, o.brushVals[0])
	}
	if len(n.brushRefs) > 0 {
		ni = n.brushRefs[0]
		nv = describeBrush(ni, n.brushVals[0])
	}
	// Compare resolved values, not slot indices.
	if ov == nv {
		return ResourceDiff{}, false
	}
	return ResourceDiff{Kind: ResourceBrush, OldIndex: oi, NewIndex: ni, OldValue: ov, NewValue: nv}, true
}

// imageRefDiff compares image resource references. Image indices name registry
// resources, so a different index means a genuinely different image is drawn.
func imageRefDiff(o, n commandRecord) (ResourceDiff, bool) {
	if len(o.imageRefs) == 0 && len(n.imageRefs) == 0 {
		return ResourceDiff{}, false
	}
	oi, ni := -1, -1
	if len(o.imageRefs) > 0 {
		oi = o.imageRefs[0]
	}
	if len(n.imageRefs) > 0 {
		ni = n.imageRefs[0]
	}
	if oi == ni {
		return ResourceDiff{}, false
	}
	return ResourceDiff{
		Kind:     ResourceImage,
		OldIndex: oi,
		NewIndex: ni,
		OldValue: fmt.Sprintf("image#%d", oi),
		NewValue: fmt.Sprintf("image#%d", ni),
	}, true
}

// fontDiff compares font resource references of text commands. A different
// FontSourceID means a genuinely different font resource.
func fontDiff(o, n commandRecord) (ResourceDiff, bool) {
	if len(o.fontRefs) == 0 && len(n.fontRefs) == 0 {
		return ResourceDiff{}, false
	}
	var of, nf uint64
	if len(o.fontRefs) > 0 {
		of = o.fontRefs[0]
	}
	if len(n.fontRefs) > 0 {
		nf = n.fontRefs[0]
	}
	if of == nf {
		return ResourceDiff{}, false
	}
	return ResourceDiff{
		Kind:     ResourceFont,
		OldIndex: -1,
		NewIndex: -1,
		OldValue: fmt.Sprintf("font#%d", of),
		NewValue: fmt.Sprintf("font#%d", nf),
	}, true
}

// describeBrush renders a slot-independent description of a brush's value, so
// the same color in two different slots produces the same string. The slot
// index is carried separately on ResourceDiff.
func describeBrush(idx int, b Brush) string {
	_ = idx
	return fmt.Sprintf("kind=%d rgba(%g,%g,%g,%g)",
		b.Kind, b.Color.R, b.Color.G, b.Color.B, b.Color.A)
}

// ---------------------------------------------------------------------------
// Small value-comparison helpers (explicit, no reflect for determinism)
// ---------------------------------------------------------------------------

func floatSlicesEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func wordSlicesEqual(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// styleWordsDiffer compares the draw words that encode style, skipping the
// first word of fill/stroke/round-rect commands, which is the brush index
// (paint, handled separately).
func styleWordsDiffer(o, n commandRecord) bool {
	skipFirst := o.tag == TagFill || o.tag == TagFillRoundRect || o.tag == TagStroke
	lo, ln := o.drawWords, n.drawWords
	if skipFirst {
		lo = safeTail(lo)
		ln = safeTail(ln)
	}
	return !wordSlicesEqual(lo, ln)
}

func safeTail(w []uint32) []uint32 {
	if len(w) <= 1 {
		return nil
	}
	return w[1:]
}

func affineSlicesEqual(a, b []Affine) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func malformed(label string, tagIndex int, tag Tag, msg string) error {
	return fmt.Errorf("%w: %s encoding at tag %d (%s): %s",
		ErrMalformedEncoding, label, tagIndex, tag, msg)
}

func invalidResource(label string, tagIndex int, tag Tag, msg string) error {
	return fmt.Errorf("%w: %s encoding at tag %d (%s): %s",
		ErrInvalidResource, label, tagIndex, tag, msg)
}
