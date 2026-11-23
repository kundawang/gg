package scene

import "github.com/gogpu/gg"

// blurFilter applies a separable box approximation of a Gaussian blur.
// It implements Filter for filter-aware layer caching.
type blurFilter struct {
	sigma float32
}

// dropShadowFilter renders an offset, blurred copy of the source alpha in a
// solid color beneath the source content.
type dropShadowFilter struct {
	offsetX float32
	offsetY float32
	sigma   float32
	color   gg.RGBA
}

// colorMatrixFilter applies a 4x5 RGBA color matrix in straight-alpha space.
type colorMatrixFilter struct {
	matrix [20]float32
}

// newFilterFromSpec constructs a concrete Filter from a serializable spec.
// Returns nil for FilterNone or unknown types.
func newFilterFromSpec(spec FilterSpec) Filter {
	switch spec.Type {
	case FilterBlur:
		return &blurFilter{sigma: spec.Params[0]}
	case FilterDropShadow:
		return &dropShadowFilter{
			offsetX: spec.Params[0],
			offsetY: spec.Params[1],
			sigma:   spec.Params[2],
			color:   spec.RGBA,
		}
	case FilterColorMatrix:
		return &colorMatrixFilter{matrix: spec.Matrix}
	default:
		return nil
	}
}

func (f *blurFilter) ExpandBounds(input Rect) Rect {
	r := f.sigma
	return Rect{
		MinX: input.MinX - r,
		MinY: input.MinY - r,
		MaxX: input.MaxX + r,
		MaxY: input.MaxY + r,
	}
}

// Apply performs a separable box blur (3 passes approximate a Gaussian for
// typical UI radii). Operates on premultiplied RGBA.
func (f *blurFilter) Apply(src, dst *gg.Pixmap, _ Rect) {
	if f.sigma <= 0 {
		copyPixmap(src, dst, Rect{MaxX: float32(src.Width()), MaxY: float32(src.Height())})
		return
	}
	radius := int(f.sigma + 0.5)
	if radius < 1 {
		radius = 1
	}
	boxBlur(src, dst, radius)
}

func (f *dropShadowFilter) ExpandBounds(input Rect) Rect {
	r := f.sigma
	return Rect{
		MinX: input.MinX - r + min32(0, f.offsetX),
		MinY: input.MinY - r + min32(0, f.offsetY),
		MaxX: input.MaxX + r + max32(0, f.offsetX),
		MaxY: input.MaxY + r + max32(0, f.offsetY),
	}
}

func (f *dropShadowFilter) Apply(src, dst *gg.Pixmap, bounds Rect) {
	w, h := src.Width(), src.Height()
	shadow := gg.NewPixmap(w, h)
	ox := int(f.offsetX + 0.5)
	oy := int(f.offsetY + 0.5)
	// Paint a solid-color shape masked by source alpha.
	srcData := src.Data()
	shData := shadow.Data()
	cr := uint8(f.color.R * 255)
	cg := uint8(f.color.G * 255)
	cb := uint8(f.color.B * 255)
	ca := uint8(f.color.A * 255)
	for i := 0; i < w*h; i++ {
		a := srcData[i*4+3]
		if a == 0 {
			continue
		}
		// Premultiplied shadow pixel scaled by coverage.
		scale := uint32(a) * uint32(ca) / 255
		shData[i*4] = uint8(uint32(cr) * scale / 255)
		shData[i*4+1] = uint8(uint32(cg) * scale / 255)
		shData[i*4+2] = uint8(uint32(cb) * scale / 255)
		shData[i*4+3] = uint8(scale)
	}
	if f.sigma > 0 {
		radius := int(f.sigma + 0.5)
		blurred := gg.NewPixmap(w, h)
		boxBlur(shadow, blurred, max(1, radius))
		shadow = blurred
	}
	// Composite the offset shadow, then the source content on top.
	shifted := gg.NewPixmap(w, h)
	blitSnapshot(shifted, shadow, ox, oy)
	compositePixmaps(dst, shifted)
	compositePixmaps(dst, src)
	_ = bounds
}

func (f *colorMatrixFilter) ExpandBounds(input Rect) Rect { return input }

func (f *colorMatrixFilter) Apply(src, dst *gg.Pixmap, _ Rect) {
	w, h := src.Width(), src.Height()
	srcData, dstData := src.Data(), dst.Data()
	m := f.matrix
	for i := 0; i < w*h; i++ {
		off := i * 4
		// Un-premultiply for the matrix, then re-premultiply.
		a := srcData[off+3]
		var sr, sg, sb float32
		if a > 0 {
			sr = float32(srcData[off]) / float32(a)
			sg = float32(srcData[off+1]) / float32(a)
			sb = float32(srcData[off+2]) / float32(a)
		}
		sa := float32(a) / 255
		rr := m[0]*sr + m[1]*sg + m[2]*sb + m[3]*sa + m[4]
		rg := m[5]*sr + m[6]*sg + m[7]*sb + m[8]*sa + m[9]
		rb := m[10]*sr + m[11]*sg + m[12]*sb + m[13]*sa + m[14]
		ra := m[15]*sr + m[16]*sg + m[17]*sb + m[18]*sa + m[19]
		rr = clamp01(rr)
		rg = clamp01(rg)
		rb = clamp01(rb)
		ra = clamp01(ra)
		dstData[off] = clampByte(rr * ra * 255)
		dstData[off+1] = clampByte(rg * ra * 255)
		dstData[off+2] = clampByte(rb * ra * 255)
		dstData[off+3] = clampByte(ra * 255)
	}
}

func clamp01(v float32) float32 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// boxBlur applies a horizontal+vertical box blur with edge clamping, using
// a rolling sum for O(n) per-pass cost.
func boxBlur(src, dst *gg.Pixmap, radius int) {
	w, h := src.Width(), src.Height()
	tmp := gg.NewPixmap(w, h)
	blurPass(src.Data(), tmp.Data(), w, h, radius, true)
	blurPass(tmp.Data(), dst.Data(), w, h, radius, false)
}

// blurPass performs one separable box-blur pass. When horizontal is true it
// scans rows (src -> a row-major buffer), otherwise it scans columns. The
// rolling window is tracked by addWindowPixel/shiftWindow helpers so the
// main loop stays linear and branch-light.
func blurPass(in, out []byte, w, h, radius int, horizontal bool) {
	span := w
	if !horizontal {
		span = h
	}
	for line := 0; line < blurLineCount(w, h, horizontal); line++ {
		var sum [4]int
		// Prime the window with the clamped leading edge.
		for k := 0; k <= radius && k < span; k++ {
			blurAccumulate(in, &sum, blurOffset(line, k, w, horizontal))
		}
		for k := 0; k < span; k++ {
			window := min(radius, span-1-k) + min(radius, k) + 1
			off := blurOffset(line, k, w, horizontal)
			writeAveragedPixel(out, off, &sum, window)
			// Slide the window: drop the pixel leaving the left edge.
			if leaving := k - radius; leaving-1 >= 0 {
				blurSubtract(in, &sum, blurOffset(line, leaving-1, w, horizontal))
			}
			// Add the pixel entering the right edge.
			if entering := k + radius + 1; entering < span {
				blurAccumulate(in, &sum, blurOffset(line, entering, w, horizontal))
			}
		}
	}
}

// blurLineCount returns how many scan lines a pass covers.
func blurLineCount(w, h int, horizontal bool) int {
	if horizontal {
		return h
	}
	return w
}

// blurOffset maps (scan line, position within line) to a byte offset.
func blurOffset(line, pos, w int, horizontal bool) int {
	if horizontal {
		return (line*w + pos) * 4
	}
	return (pos*w + line) * 4
}

func blurAccumulate(data []byte, sum *[4]int, off int) {
	for c := 0; c < 4; c++ {
		sum[c] += int(data[off+c])
	}
}

func blurSubtract(data []byte, sum *[4]int, off int) {
	for c := 0; c < 4; c++ {
		sum[c] -= int(data[off+c])
	}
}

func writeAveragedPixel(out []byte, off int, sum *[4]int, window int) {
	for c := 0; c < 4; c++ {
		out[off+c] = uint8(sum[c] / window)
	}
}
