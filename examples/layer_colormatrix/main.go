// Package main demonstrates layer color matrices for a game HUD.
//
// A HUD is drawn once into a layer. On damage the same drawing commands are
// composited through a red tint matrix, and on pause through a grayscale
// matrix, so the whole group shifts hue without recoloring each icon.
//
// Run from this directory; it writes hud_normal.png, hud_damage.png and
// hud_paused.png.
package main

import (
	"log"

	"github.com/gogpu/gg"
)

const (
	width  = 480
	height = 240
)

func main() {
	// Untouched HUD.
	render("hud_normal.png", gg.IdentityColorMatrix(), 1.0)

	// Damage flash: tint the group toward red at 45% strength, keeping alpha.
	render("hud_damage.png", gg.TintColorMatrix(gg.RGBA{R: 1, G: 0, B: 0, A: 0.45}), 1.0)

	// Paused: desaturate the group, then fade it slightly.
	render("hud_paused.png", gg.SaturationColorMatrix(0), 0.7)
}

// render draws the HUD into a layer carrying the given color matrix.
func render(path string, matrix gg.ColorMatrix, opacity float64) {
	dc := gg.NewContext(width, height)
	defer func() { _ = dc.Close() }()

	dc.ClearWithColor(gg.RGBA{R: 0.08, G: 0.09, B: 0.12, A: 1})

	dc.PushLayerWithColorMatrix(gg.BlendNormal, opacity, matrix)
	drawHUD(dc)
	dc.PopLayer()

	if err := dc.SavePNG(path); err != nil {
		log.Fatalf("save %s: %v", path, err)
	}
	log.Printf("wrote %s", path)
}

// drawHUD is the group content. It uses its own colors; the layer matrix is
// what gives the whole group a consistent tone.
func drawHUD(dc *gg.Context) {
	// Health bar: green track, darker green fill, red low-health sliver.
	dc.SetRGBA(0.12, 0.2, 0.14, 1)
	dc.DrawRectangle(24, 24, 432, 22)
	_ = dc.Fill()

	dc.SetRGBA(0.25, 0.85, 0.4, 1)
	dc.DrawRectangle(26, 26, 300, 18)
	_ = dc.Fill()

	dc.SetRGBA(0.9, 0.25, 0.2, 1)
	dc.DrawRectangle(26, 26, 70, 18)
	_ = dc.Fill()

	// Mana bar: blue.
	dc.SetRGBA(0.2, 0.45, 0.95, 1)
	dc.DrawRectangle(24, 54, 432, 12)
	_ = dc.Fill()

	// Icon row: four simple colored badges with a light ring.
	colors := []gg.RGBA{
		{R: 0.95, G: 0.75, B: 0.2, A: 1},  // gold
		{R: 0.3, G: 0.8, B: 0.95, A: 1},   // cyan
		{R: 0.85, G: 0.35, B: 0.75, A: 1}, // magenta
		{R: 0.55, G: 0.9, B: 0.35, A: 1},  // lime
	}
	for i, c := range colors {
		cx := 60 + float64(i)*90
		cy := 150.0
		dc.SetRGBA(0.2, 0.22, 0.28, 1)
		dc.DrawCircle(cx, cy, 26)
		_ = dc.Fill()
		dc.SetRGBA(c.R, c.G, c.B, 1)
		dc.DrawCircle(cx, cy, 20)
		_ = dc.Fill()
	}

	// Crosshair / reticle accent.
	dc.SetRGBA(0.95, 0.95, 0.98, 1)
	dc.SetLineWidth(3)
	dc.DrawCircle(360, 150, 26)
	_ = dc.Stroke()
	dc.DrawLine(360, 118, 360, 182)
	_ = dc.Stroke()
	dc.DrawLine(328, 150, 392, 150)
	_ = dc.Stroke()
}
