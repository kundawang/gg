package gg

import (
	intImage "github.com/gogpu/gg/internal/image"
)

// Layer represents a drawing layer with blend mode and opacity.
// Layers allow isolating drawing operations and compositing them with
// different blend modes and opacity values, similar to layers in Photoshop
// or SVG group opacity.
type Layer struct {
	pixmap      *Pixmap
	blendMode   BlendMode
	opacity     float64
	mask        *Mask        // optional alpha mask, applied on PopLayer (nil = no mask)
	colorMatrix *ColorMatrix // optional color transform, applied on PopLayer (nil = identity)
}

// layerStack manages the layer hierarchy for the context.
type layerStack struct {
	layers []*Layer
	pool   *intImage.Pool
}

// newLayerStack creates a new layer stack with a pool for memory reuse.
func newLayerStack() *layerStack {
	return &layerStack{
		layers: make([]*Layer, 0, 4),
		pool:   intImage.NewPool(8),
	}
}

// PushLayer creates a new layer and makes it the active drawing target.
// All subsequent drawing operations will render to this layer until PopLayer is called.
//
// The layer will be composited onto the parent layer/canvas when PopLayer is called,
// using the specified blend mode and opacity.
//
// Parameters:
//   - blendMode: How to composite this layer onto the parent (e.g., BlendMultiply, BlendScreen)
//   - opacity: Layer opacity in range [0.0, 1.0] where 0 is fully transparent and 1 is fully opaque
//
// Example:
//
//	dc.PushLayer(gg.BlendMultiply, 0.5)
//	dc.SetRGB(1, 0, 0)
//	dc.DrawCircle(100, 100, 50)
//	dc.Fill()
//	dc.PopLayer() // Composite circle onto canvas with multiply blend at 50% opacity
func (c *Context) PushLayer(blendMode BlendMode, opacity float64) {
	c.pushLayer(blendMode, opacity, nil, nil)
}

// PushLayerWithColorMatrix is PushLayer with an optional 4x5 color matrix
// applied to the layer's contents on PopLayer, before opacity and blending.
//
// This is the group-level color transform: a HUD can push one layer, draw all
// its icons and text normally, and have the whole group shifted toward red
// (or desaturated, tinted, and so on) with exactly the same transform, without
// recoloring each draw call.
//
// The matrix is applied to straight-alpha color in [0, 1]; see [ColorMatrix].
// Fully transparent pixels remain transparent. The result is clamped to [0, 1].
//
// Edge cases, all deterministic:
//   - A non-finite matrix (NaN or ±Inf) is ignored and the layer composites
//     unchanged, so a bad coefficient cannot corrupt the parent.
//   - The identity matrix is a no-op and costs nothing extra.
//   - Matrix application runs on the CPU after pending GPU work is flushed;
//     software and GPU backends therefore produce the same pixels.
//   - An empty or fully transparent layer stays a no-op.
//
// Example:
//
//	dc.PushLayerWithColorMatrix(gg.BlendNormal, 1.0,
//		gg.SaturationColorMatrix(1.4))
//	drawHUD(dc) // every icon keeps its own colors
//	dc.PopLayer()
//
// For the damage flash that tints the group red while preserving alpha:
//
//	flash := gg.TintColorMatrix(gg.RGBA{R: 1, G: 0, B: 0, A: 0.45})
//	dc.PushLayerWithColorMatrix(gg.BlendNormal, 1.0, flash)
//	drawHUD(dc)
//	dc.PopLayer()
func (c *Context) PushLayerWithColorMatrix(blendMode BlendMode, opacity float64, matrix ColorMatrix) {
	if !matrix.IsFinite() {
		Logger().Warn("gg: ignoring non-finite layer color matrix")
		c.pushLayer(blendMode, opacity, nil, nil)
		return
	}
	if matrix.IsIdentity() {
		c.pushLayer(blendMode, opacity, nil, nil)
		return
	}
	m := matrix
	c.pushLayer(blendMode, opacity, nil, &m)
}

// pushLayer is the shared implementation for PushLayer, PushLayerWithColorMatrix
// and PushMaskLayer.
func (c *Context) pushLayer(blendMode BlendMode, opacity float64, mask *Mask, matrix *ColorMatrix) {
	// Clamp opacity to valid range
	if opacity < 0 {
		opacity = 0
	}
	if opacity > 1 {
		opacity = 1
	}

	// Initialize layer stack if needed
	if c.layerStack == nil {
		c.layerStack = newLayerStack()
	}

	// Resolve any GPU work queued for the parent target before switching to
	// the layer pixmap. The GPU queue is associated with the target that was
	// last drawn to; flushing here guarantees that PopLayer only ever has to
	// resolve this layer's own draws, and that switching to a fresh pixmap
	// never leaves the parent's output stranded in the queue.
	if c.layerStackHasPendingGPU() {
		c.flushGPUAccelerator()
	}

	// Save base pixmap on first push
	if len(c.layerStack.layers) == 0 && c.basePixmap == nil {
		c.basePixmap = c.pixmap
	}

	// Create the layer at the physical resolution used by the renderer. The
	// context dimensions are logical when deviceScale != 1, but drawing paths
	// are transformed into device coordinates before rasterization.
	layerPixmap := NewPixmap(c.PixelWidth(), c.PixelHeight())
	layerPixmap.Clear(Transparent)

	// Create layer
	layer := &Layer{
		pixmap:      layerPixmap,
		blendMode:   blendMode,
		opacity:     opacity,
		mask:        mask,
		colorMatrix: matrix,
	}

	// Save current pixmap and switch to layer pixmap
	c.layerStack.layers = append(c.layerStack.layers, layer)
	c.pixmap = layerPixmap
}

// PopLayer composites the current layer onto the parent layer/canvas.
// Uses the blend mode and opacity specified in the corresponding PushLayer call.
//
// The layer is composited using the specified blend mode and opacity.
// After compositing, the layer's memory is returned to the pool for reuse.
//
// If there are no layers to pop, this function does nothing.
//
// Example:
//
//	dc.PushLayer(gg.BlendScreen, 1.0)
//	// ... draw operations ...
//	dc.PopLayer() // Composite layer onto parent
func (c *Context) PopLayer() {
	if c.layerStack == nil || len(c.layerStack.layers) == 0 {
		return
	}

	// Make sure GPU-queued draws in this layer's pixmap have landed before
	// the CPU compositor reads it. Done before popping the stack so a GPU
	// context is only created/looked up when this layer has pending work.
	// The CPU compositor below is the single source of truth for layer
	// blending, so software and GPU backends composite identically. If an
	// accelerator cannot resolve to this pixmap it returns ErrFallbackToCPU
	// and the flusher falls back to the global accelerator, which does.
	if c.layerStackHasPendingGPU() {
		c.flushGPUAccelerator()
	}

	// Pop the current layer
	layers := c.layerStack.layers
	layer := layers[len(layers)-1]
	c.layerStack.layers = layers[:len(layers)-1]

	// Get parent pixmap (either previous layer or base)
	var parentPixmap *Pixmap
	if len(c.layerStack.layers) > 0 {
		parentPixmap = c.layerStack.layers[len(c.layerStack.layers)-1].pixmap
	} else {
		// Restore base pixmap
		parentPixmap = c.basePixmap
		c.basePixmap = nil
	}

	// Apply the optional group color transform before opacity and blending.
	// Fully transparent pixels are left untouched (see transformColorMatrix).
	if layer.colorMatrix != nil {
		applyColorMatrixToPixmap(layer.pixmap, layer.colorMatrix)
	}

	// Apply mask to layer content before compositing (PushMaskLayer).
	if layer.mask != nil {
		c.applyMaskToPixmap(layer.pixmap, layer.mask)
	}

	// Composite layer onto parent
	c.compositeLayer(layer, parentPixmap)

	// Restore parent pixmap as current drawing target
	c.pixmap = parentPixmap
}

// PushMaskLayer creates an isolated layer with an associated alpha mask.
// All subsequent drawing operations render to this layer normally (without masking).
// When PopLayer is called, the ENTIRE layer is masked by the mask and then
// composited onto the parent using source-over blending with full opacity.
//
// This produces different results from SetMask: PushMaskLayer masks the
// composited group, while SetMask masks each shape individually.
//
// Matches Vello push_mask_layer() semantics (research §4).
//
// Example:
//
//	mask := gg.NewMaskFromAlpha(maskImage)
//	dc.PushMaskLayer(mask)
//	dc.DrawCircle(100, 100, 50)
//	dc.Fill()
//	dc.DrawRect(80, 80, 40, 40)
//	dc.Fill()
//	dc.PopLayer() // entire layer content masked, then composited
func (c *Context) PushMaskLayer(mask *Mask) {
	// Clamp: nil mask means no masking (equivalent to PushLayer).
	if mask == nil {
		c.PushLayer(BlendNormal, 1.0)
		return
	}
	c.pushLayer(BlendNormal, 1.0, mask, nil)
}

// applyMaskToPixmap applies a DestinationIn mask to a pixmap's pixel data.
// For each pixel: all channels are scaled by mask.At(x,y) / 255.
func (c *Context) applyMaskToPixmap(pm *Pixmap, mask *Mask) {
	applyMaskToPixmapData(pm, mask)
}

// SetBlendMode sets the blend mode for subsequent fill and stroke operations.
// The blend mode controls how source pixels are composited onto the destination.
//
// All 29 blend modes from the W3C Compositing and Blending specification are
// supported: 14 Porter-Duff operators, 11 separable modes (Multiply, Screen,
// Overlay, etc.), and 4 non-separable HSL modes (Hue, Saturation, Color, Luminosity).
//
// The default mode is BlendNormal (source-over alpha compositing).
// For SourceOver (the default), rendering uses the existing optimized float64
// inline path with zero additional overhead.
//
// Example:
//
//	dc.SetBlendMode(gg.BlendMultiply)
//	dc.SetRGB(1, 0, 0)
//	dc.DrawRectangle(0, 0, 100, 100)
//	dc.Fill()
//	dc.SetBlendMode(gg.BlendNormal) // restore default
func (c *Context) SetBlendMode(mode BlendMode) {
	c.paint.blendMode = mapPublicBlendMode(mode)
}

// GetBlendMode returns the current blend mode for fill and stroke operations.
func (c *Context) GetBlendMode() BlendMode {
	return BlendMode(c.paint.blendMode)
}

// layerStackHasPendingGPU reports whether GPU work may be queued and still
// needs to be resolved into the current pixmap before layer compositing.
//
// A per-context GPU render context is authoritative: its PendingCount reports
// exactly whether draws are waiting. Without one, a registered (legacy or
// global) accelerator may still batch work in its own queue, so this reports
// true and leaves it to Flush to decide; flushing an empty queue is a no-op.
func (c *Context) layerStackHasPendingGPU() bool {
	if c.gpuCtx != nil {
		type pendingCounter interface{ PendingCount() int }
		if pc, ok := c.gpuCtx.(pendingCounter); ok {
			return pc.PendingCount() > 0
		}
		// Unknown per-context backend: flush conservatively.
		return true
	}
	a := Accelerator()
	if a == nil {
		return false
	}
	// Legacy/global accelerators that batch report their queue length; flush
	// only when there is something to resolve.
	if pc, ok := a.(interface{ PendingCount() int }); ok {
		return pc.PendingCount() > 0
	}
	// A registered accelerator with no way to inspect its queue: flush
	// conservatively so layer output is never composed from stale pixels.
	return true
}

// compositeLayer composites a layer onto a parent pixmap using the layer's
// blend mode and opacity.
//
// Both buffers carry premultiplied RGBA. The compositor works in premultiplied
// space with the W3C Compositing and Blending formula
//
//	Cr = (1 - αs)·Cd + (1 - αd)·Cs + αs·αd·B(Cs/αs, Cd/αd)
//
// so a translucent source pixel keeps its color instead of being darkened by a
// second alpha multiply. (The previous path fed premultiplied bytes to
// internal/image.DrawImage as if they were straight-alpha, so a 50% red layer
// over white composited to pink instead of the same red a direct fill yields.)
func (c *Context) compositeLayer(layer *Layer, parent *Pixmap) {
	if layer == nil || layer.pixmap == nil || parent == nil || layer.opacity <= 0 {
		return
	}
	if layer.pixmap.Width() != parent.Width() {
		// Layers are allocated at the context's physical size, so a mismatch
		// means the parent was replaced mid-stack. Clip to the smaller width
		// rather than reading past either buffer.
		Logger().Warn("gg: layer/parent width mismatch; compositing to the common width")
	}
	compositeLayerPixels(parent.Data(), layer.pixmap.Data(), min(layer.pixmap.Width(), parent.Width()),
		layer.blendMode, layer.opacity)
}

// compositeLayerPixels blends src onto dst in premultiplied RGBA byte space.
// src and dst are row-major 4-byte pixels of width w (same stride). Pixels
// outside either buffer are never read or written.
func compositeLayerPixels(dst, src []byte, width int, mode BlendMode, opacity float64) {
	if width <= 0 {
		return
	}
	n := min(len(src), len(dst))
	n -= n % 4 // whole pixels only
	if n <= 0 || opacity <= 0 {
		return
	}
	if opacity > 1 {
		opacity = 1
	}

	for i := 0; i < n; i += 4 {
		// Scale the layer by its opacity in premultiplied space: the color
		// channels and alpha shrink together.
		sa := float64(src[i+3]) / 255.0 * opacity
		if sa <= 0 {
			// Fully transparent (or fully faded) source: keep dst exactly,
			// including its raw bytes. Empty layers are therefore a no-op.
			continue
		}
		sr := float64(src[i+0]) / 255.0 * opacity
		sg := float64(src[i+1]) / 255.0 * opacity
		sb := float64(src[i+2]) / 255.0 * opacity

		dr := float64(dst[i+0]) / 255.0
		dg := float64(dst[i+1]) / 255.0
		db := float64(dst[i+2]) / 255.0
		da := float64(dst[i+3]) / 255.0

		invSa := 1 - sa
		invDa := 1 - da
		outA := sa + da*invSa

		// Un-premultiplied colors are only needed for the blend function.
		// When either alpha is zero its blend term is zero as well, so the
		// placeholder stays unused and division by zero is avoided.
		var csr, csg, csb, cdr, cdg, cdb float64
		if sa > 0 {
			invS := 1 / sa
			csr, csg, csb = sr*invS, sg*invS, sb*invS
		}
		if da > 0 {
			invD := 1 / da
			cdr, cdg, cdb = dr*invD, dg*invD, db*invD
		}

		blendR := layerBlendChannel(mode, csr, cdr)
		blendG := layerBlendChannel(mode, csg, cdg)
		blendB := layerBlendChannel(mode, csb, cdb)

		// Premultiplied output of the W3C formula.
		dst[i+0] = unitToByte(invSa*dr + invDa*sr + sa*da*blendR)
		dst[i+1] = unitToByte(invSa*dg + invDa*sg + sa*da*blendG)
		dst[i+2] = unitToByte(invSa*db + invDa*sb + sa*da*blendB)
		dst[i+3] = unitToByte(outA)
	}
}

// layerBlendChannel evaluates the separable blend function B(Cs, Cd) for one
// channel. Inputs and output are straight-alpha [0, 1] values.
//
// Layer blending supports the four modes of [BlendMode] (normal, multiply,
// screen, overlay); other values use source-over semantics, matching the
// legacy layer compositor.
func layerBlendChannel(mode BlendMode, cs, cd float64) float64 {
	switch mode {
	case BlendMultiply:
		return cs * cd
	case BlendScreen:
		return cs + cd - cs*cd
	case BlendOverlay:
		if cd <= 0.5 {
			return 2 * cs * cd
		}
		return 1 - 2*(1-cs)*(1-cd)
	default: // BlendNormal and any unsupported mode
		return cs
	}
}

// applyColorMatrixToPixmap transforms every pixel of pm in place using m.
// Fully transparent pixels are left byte-for-byte untouched so the transform
// cannot paint color into empty regions of a layer.
func applyColorMatrixToPixmap(pm *Pixmap, m *ColorMatrix) {
	if pm == nil || m == nil || !m.IsFinite() {
		return
	}
	data := pm.Data()
	for i := 0; i+3 < len(data); i += 4 {
		if data[i+3] == 0 {
			// Fully transparent: leave the pixel untouched, including any
			// stray biased color bytes, so the transform cannot paint into
			// empty regions of a layer.
			continue
		}
		r, g, b, a := m.transformColorMatrix(data[i], data[i+1], data[i+2], data[i+3])
		data[i+0] = r
		data[i+1] = g
		data[i+2] = b
		data[i+3] = a
	}
}
