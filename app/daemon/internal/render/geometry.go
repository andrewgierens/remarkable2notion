// Package render turns parsed scenes into SVG and PDF. Both formats consume
// the same path geometry — SVG path data and PDF content streams share their
// primitives (m/c/l + stroke) — so the curve fitting lives here and each
// backend only does serialisation.
package render

import (
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/rm"
)

// Device page size in document units (reMarkable 2 screen; Paper Pro pages
// use the same coordinate system with a larger canvas, which the computed
// bounds absorb).
const (
	PageWidth  = 1404.0
	PageHeight = 1872.0
)

// point is a 2D point in page coordinates (origin top-left, x already
// shifted from the device's centred coordinates).
type point struct{ X, Y float64 }

// segment is one cubic Bézier piece of a stroke path.
type segment struct{ C1, C2, To point }

// path is a renderable stroke: a start point, cubic segments, styling.
type path struct {
	Start   point
	Segs    []segment
	Width   float64
	Color   rgb
	Opacity float64 // 1 = opaque
}

type rgb struct{ R, G, B float64 }

// penColors maps device colour ids to render colours. Unknown ids fall back
// to black rather than failing the send.
var penColors = map[rm.Color]rgb{
	rm.ColorBlack:       {0, 0, 0},
	rm.ColorGray:        {0.494, 0.494, 0.494},
	rm.ColorWhite:       {1, 1, 1},
	rm.ColorYellow:      {1, 0.914, 0.29},
	rm.ColorGreen:       {0, 0.588, 0.278},
	rm.ColorPink:        {0.933, 0.394, 0.712},
	rm.ColorBlue:        {0.04, 0.31, 0.98},
	rm.ColorRed:         {0.82, 0.173, 0.173},
	rm.ColorGrayOverlap: {0.749, 0.749, 0.749},
	rm.ColorHLGreen:     {0.65, 0.97, 0.76},
	rm.ColorCyan:        {0.0, 0.75, 0.95},
	rm.ColorMagenta:     {0.85, 0.14, 0.85},
	rm.ColorHLYellow:    {1, 0.914, 0.29},
}

func colorFor(c rm.Color) rgb {
	if v, ok := penColors[c]; ok {
		return v
	}
	return rgb{0, 0, 0}
}

// strokePath converts one stroke to a smooth path, or nil when the stroke
// should not render (hidden tools, too few points).
func strokePath(line rm.Line) *path {
	if line.Tool == rm.PenEraseArea {
		return nil
	}
	pts := make([]point, 0, len(line.Points))
	for _, p := range line.Points {
		pts = append(pts, point{float64(p.X) + PageWidth/2, float64(p.Y)})
	}
	if len(pts) == 0 {
		return nil
	}

	pa := &path{Start: pts[0], Opacity: 1}

	// Width: the mean of the recorded per-point widths; strokes with no
	// width data fall back to the tool's thickness scale.
	var sum float64
	var n int
	for _, p := range line.Points {
		if p.Width > 0 {
			sum += float64(p.Width)
			n++
		}
	}
	switch {
	case n > 0:
		pa.Width = sum / float64(n)
	case line.ThicknessScale > 0:
		pa.Width = line.ThicknessScale * 2
	default:
		pa.Width = 2
	}

	pa.Color = colorFor(line.Color)
	if line.Tool == rm.PenEraser {
		pa.Color = rgb{1, 1, 1}
	}
	if line.Tool.IsHighlighter() {
		pa.Opacity = 0.45
		if line.Color == rm.ColorBlack { // stock highlighter reports black
			pa.Color = colorFor(rm.ColorHLYellow)
		}
	}

	if len(pts) == 1 {
		// A dot: emit a tiny segment so a stroked path with round caps
		// renders a point.
		pa.Segs = []segment{{pts[0], pts[0], point{pts[0].X + 0.1, pts[0].Y}}}
		return pa
	}

	// Catmull-Rom through the samples, emitted as cubic Béziers.
	for i := 0; i < len(pts)-1; i++ {
		p0 := pts[max(i-1, 0)]
		p1 := pts[i]
		p2 := pts[i+1]
		p3 := pts[min(i+2, len(pts)-1)]
		c1 := point{p1.X + (p2.X-p0.X)/6, p1.Y + (p2.Y-p0.Y)/6}
		c2 := point{p2.X - (p3.X-p1.X)/6, p2.Y - (p3.Y-p1.Y)/6}
		pa.Segs = append(pa.Segs, segment{c1, c2, p2})
	}
	return pa
}

// scenePaths flattens a scene into draw-ordered paths, skipping hidden
// layers.
func scenePaths(scene *rm.Scene) []*path {
	var out []*path
	for _, layer := range scene.Layers {
		if !layer.Visible {
			continue
		}
		for _, line := range layer.Lines {
			if p := strokePath(line); p != nil {
				out = append(out, p)
			}
		}
	}
	return out
}

// bounds returns the drawing area: the device page, expanded to cover any
// strokes and text outside it.
func bounds(scene *rm.Scene) (minX, minY, maxX, maxY float64) {
	minX, minY, maxX, maxY = 0, 0, PageWidth, PageHeight
	for _, p := range scenePaths(scene) {
		grow := func(pt point, w float64) {
			minX = min(minX, pt.X-w)
			minY = min(minY, pt.Y-w)
			maxX = max(maxX, pt.X+w)
			maxY = max(maxY, pt.Y+w)
		}
		grow(p.Start, p.Width)
		for _, s := range p.Segs {
			grow(s.C1, p.Width)
			grow(s.C2, p.Width)
			grow(s.To, p.Width)
		}
	}
	if scene.Text != nil {
		minX = min(minX, scene.Text.X+PageWidth/2)
		minY = min(minY, scene.Text.Y)
	}
	return
}

// Text layout constants for typed text. The device renders rich text with
// its own fonts; a plain sans layout keeps the content readable.
const (
	textFontSize   = 30.0
	textLineHeight = 44.0
)
