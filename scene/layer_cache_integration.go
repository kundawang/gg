package scene

import (
	"context"
	"time"

	"github.com/gogpu/gg"
)

// cachedLayerPlan is the per-frame decomposition of a scene into root
// content and cacheable static layers.
type cachedLayerPlan struct {
	root     *Encoding
	layers   []plannedLayer
	eligible bool
}

// plannedLayer is one layer slot with its standalone sub-encoding and
// signature.
type plannedLayer struct {
	id    uint32
	seg   layerSegment
	enc   *Encoding
	key   LayerKey
	specs []FilterSpec
}

// planCachedLayers decomposes enc into root content and cacheable layers.
// It returns eligible=false when any layer cannot be served through the
// cache path, so the caller deterministically falls back to full rendering.
func (r *Renderer) planCachedLayers(enc *Encoding, images []*Image) cachedLayerPlan {
	segments := segmentLayers(enc)
	plan := cachedLayerPlan{eligible: len(segments) > 0}
	if !plan.eligible {
		return plan
	}

	scale := r.deviceScale
	if scale <= 0 {
		scale = 1
	}
	scale = QuantizeScale(scale)

	for i, seg := range segments {
		// Only isolated, source-over-composited layers are bit-exact
		// equivalent when blitted as an offscreen buffer. Other blend
		// modes or partial alpha require full-pipeline compositing.
		if seg.blend != BlendNormal || seg.alpha < 1 {
			plan.eligible = false
			return plan
		}
		sub := buildSubEncoding(enc, seg.tagStart, seg.tagEnd)
		if sub == nil || sub.IsEmpty() {
			plan.eligible = false
			return plan
		}
		specs := r.layerFilters[uint32(i)]
		key := LayerKey{
			ContentHash: layerContentHash(sub, images, seg),
			DeviceScale: scale,
			FilterHash:  filterHashOf(specs),
			Blend:       seg.blend,
			Alpha:       seg.alpha,
			LayerID:     uint32(i),
		}
		plan.layers = append(plan.layers, plannedLayer{
			id:    uint32(i),
			seg:   seg,
			enc:   sub,
			key:   key,
			specs: specs,
		})
	}
	plan.root = buildRootEncoding(enc, segments)

	// Deterministic fallback: if any layer's rasterization would exceed the
	// byte budget, the scene is marked ineligible and the caller uses the
	// full tile pipeline. This keeps low-budget output byte-identical to
	// uncached rendering instead of silently bypassing the cache per layer.
	if r.pixelCache != nil && r.pixelCache.Budget() > 0 {
		budget := r.pixelCache.Budget()
		for _, layer := range plan.layers {
			b := computeEncodingBounds(layer.enc, images)
			if b.IsEmpty() {
				continue
			}
			ox := max(0, int(b.MinX)-1)
			oy := max(0, int(b.MinY)-1)
			ex := min(r.width, int(b.MaxX)+1)
			ey := min(r.height, int(b.MaxY)+1)
			if int64((ex-ox)*(ey-oy))*bytesPerPixel > budget {
				plan.eligible = false
				return plan
			}
		}
	}
	return plan
}

// renderWithLayerCache runs the layer-cached CPU render path. It returns
// rendered=false whenever the scene is not eligible, signaling the caller to
// use the full tile pipeline so output is always identical.
func (r *Renderer) renderWithLayerCache(ctx context.Context, target *gg.Pixmap, scene *Scene) (bool, error) {
	enc := scene.Encoding()
	images := scene.Images()
	r.fontRegistry = scene.FontRegistry()

	plan := r.planCachedLayers(enc, images)
	if !plan.eligible {
		return false, nil
	}

	// Cancellation check before any work.
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	default:
	}

	startTotal := time.Now()
	r.statsMu.Lock()
	r.stats = RenderStats{}
	r.statsMu.Unlock()

	// 1. Root content through the unchanged tile pipeline.
	startRaster := time.Now()
	r.dirty.MarkAll()
	tiles := r.tileGrid.AllTiles()
	if err := r.renderTilesWithContext(ctx, tiles, plan.root, target, images); err != nil {
		return true, err
	}
	r.compositeTiles(tiles, target)
	r.dirty.Clear()
	r.tileGrid.ClearDirty()

	// 2. Layers: lookup, build on miss, blit.
	for _, layer := range plan.layers {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		default:
		}
		if !r.renderOneCachedLayer(layer, images, target) {
			continue
		}
	}

	rasterTime := time.Since(startRaster)
	totalTime := time.Since(startTotal)
	r.updateStats(len(tiles), len(tiles), len(tiles), 0, rasterTime, 0, totalTime)
	return true, nil
}

// renderOneCachedLayer serves one layer from the cache, rebuilding and
// storing it on miss, then blits it onto target. Returns false when the
// layer produced nothing and was skipped.
func (r *Renderer) renderOneCachedLayer(layer plannedLayer, images []*Image, target *gg.Pixmap) bool {
	snap := r.pixelCache.Lookup(layer.key)
	if snap != nil {
		r.stats.LayersCached++
		snap.Blit(target)
		return true
	}

	pm, ox, oy, b := r.renderEncodingToPixmap(layer.enc, images, layer.key.DeviceScale)
	if pm == nil {
		return false
	}
	// Filter expansion: apply the chain before storing. The filter hash is
	// already part of the key, so expanded buffers for different chains
	// never collide.
	if len(layer.specs) > 0 {
		pm = r.applyLayerFilters(pm, layer.specs, b, ox, oy)
		if pm == nil {
			return false
		}
	}
	snap = r.pixelCache.Store(layer.key, pm, ox, oy, b)
	if snap == nil {
		return false
	}
	r.stats.LayersRendered++
	snap.Blit(target)
	return true
}

// WarmupLayers pre-rasterizes every cacheable static layer of scene into the
// pixel cache under the renderer's device scale and configured filters.
//
// It is safe to call before the first frame and concurrently from multiple
// goroutines: per-key single-flight guarantees each layer is built at most
// once for a given signature. Context cancellation aborts the wait without
// corrupting the cache; a canceled warmup simply leaves a cold entry and the
// next render rebuilds it.
//
// Returns the number of layers warmed. Non-cacheable scenes return 0 without
// error (the render path itself will use the full tile fallback).
func (r *Renderer) WarmupLayers(ctx context.Context, scene *Scene) (int, error) {
	if r.pixelCache == nil {
		return 0, nil
	}
	enc := scene.Encoding()
	images := scene.Images()
	r.fontRegistry = scene.FontRegistry()

	plan := r.planCachedLayers(enc, images)
	if !plan.eligible {
		return 0, nil
	}

	warmed := 0
	for _, layer := range plan.layers {
		layer := layer // capture for closure
		_, err := r.pixelCache.Warm(ctx, layer.key, func() (*gg.Pixmap, int, int, Rect) {
			pm, ox, oy, b := r.renderEncodingToPixmap(layer.enc, images, layer.key.DeviceScale)
			if pm != nil && len(layer.specs) > 0 {
				pm = r.applyLayerFilters(pm, layer.specs, b, ox, oy)
			}
			if pm == nil {
				return nil, 0, 0, EmptyRect()
			}
			return pm, ox, oy, b
		})
		if err != nil {
			return warmed, err
		}
		warmed++
	}
	return warmed, nil
}

// InvalidateLayerCache precisely invalidates one layer slot across all
// scales and filters. Use when a single static layer's geometry changes.
func (r *Renderer) InvalidateLayerCache(layerID uint32) {
	if r.pixelCache != nil {
		r.pixelCache.InvalidateLayer(layerID)
	}
}

// InvalidateLayerCacheAll explicitly clears every cached layer entry.
func (r *Renderer) InvalidateLayerCacheAll() {
	if r.pixelCache != nil {
		r.pixelCache.InvalidateAll()
	}
}

// PixelCacheStats returns the observable counters of the layer pixel cache.
// Returns the zero value when layer caching is not enabled.
func (r *Renderer) PixelCacheStats() PixelCacheStats {
	if r.pixelCache == nil {
		return PixelCacheStats{}
	}
	return r.pixelCache.Stats()
}

// SetLayerCacheBudget changes the layer cache byte budget at runtime.
// A zero budget disables caching and clears all entries deterministically.
func (r *Renderer) SetLayerCacheBudget(maxBytes int64) {
	if r.pixelCache == nil {
		if maxBytes > 0 {
			r.pixelCache = NewLayerPixelCache(maxBytes)
		}
		return
	}
	r.pixelCache.SetBudget(maxBytes)
}

// DeviceScale returns the quantized device scale used for layer signatures.
func (r *Renderer) DeviceScale() float32 {
	return r.deviceScale
}

// applyLayerFilters runs a filter chain over a freshly rasterized layer
// pixmap. Filters that expand bounds (blur, shadow) produce a larger buffer;
// the origin is shifted accordingly so Blit places pixels at the right
// canvas position. The input pixmap is consumed and must not be reused.
func (r *Renderer) applyLayerFilters(pm *gg.Pixmap, specs []FilterSpec, bounds Rect, ox, oy int) *gg.Pixmap {
	if len(specs) == 0 || pm == nil {
		return pm
	}
	// Build concrete filters from the serializable specs.
	filters := make([]Filter, 0, len(specs))
	for i := range specs {
		if f := newFilterFromSpec(specs[i]); f != nil {
			filters = append(filters, f)
		}
	}
	if len(filters) == 0 {
		return pm
	}
	chain := NewFilterChain(filters...)
	expanded := chain.ExpandBounds(bounds)

	newOX := int(expanded.MinX)
	newOY := int(expanded.MinY)
	w := int(expanded.MaxX) - newOX + 1
	h := int(expanded.MaxY) - newOY + 1
	if w < pm.Width() {
		w = pm.Width()
	}
	if h < pm.Height() {
		h = pm.Height()
	}
	dst := gg.NewPixmap(w, h)
	// Shift source into the expanded buffer at its original origin.
	src := gg.NewPixmap(w, h)
	blitSnapshot(src, pm, ox-newOX, oy-newOY)
	chain.Apply(src, dst, bounds)
	return dst
}
