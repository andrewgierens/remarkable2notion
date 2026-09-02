package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/rm"
)

// SVG writes one scene as a standalone SVG document. Layers become <g>
// elements in draw order; strokes are <path> elements with cubic segments.
func SVG(scene *rm.Scene, w io.Writer) error {
	minX, minY, maxX, maxY := bounds(scene)
	width, height := maxX-minX, maxY-minY

	b := &strings.Builder{}
	fmt.Fprintf(b, `<?xml version="1.0" encoding="UTF-8"?>`+"\n")
	fmt.Fprintf(b,
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="%s %s %s %s" width="%s" height="%s">`+"\n",
		f(minX), f(minY), f(width), f(height), f(width), f(height))
	fmt.Fprintf(b, `<rect x="%s" y="%s" width="%s" height="%s" fill="white"/>`+"\n",
		f(minX), f(minY), f(width), f(height))

	if scene.Text != nil {
		writeSVGText(b, scene.Text)
	}

	for i, layer := range scene.Layers {
		if !layer.Visible {
			continue
		}
		label := layer.Label
		if label == "" {
			label = fmt.Sprintf("layer-%d", i+1)
		}
		fmt.Fprintf(b, `<g id="%s">`+"\n", xmlEscape(label))
		for _, line := range layer.Lines {
			p := strokePath(line)
			if p == nil {
				continue
			}
			var d strings.Builder
			fmt.Fprintf(&d, "M %s %s", f(p.Start.X), f(p.Start.Y))
			for _, s := range p.Segs {
				fmt.Fprintf(&d, " C %s %s, %s %s, %s %s",
					f(s.C1.X), f(s.C1.Y), f(s.C2.X), f(s.C2.Y), f(s.To.X), f(s.To.Y))
			}
			opacity := ""
			if p.Opacity < 1 {
				opacity = fmt.Sprintf(` stroke-opacity="%s"`, f(p.Opacity))
			}
			fmt.Fprintf(b,
				`<path d="%s" fill="none" stroke="%s" stroke-width="%s" stroke-linecap="round" stroke-linejoin="round"%s/>`+"\n",
				d.String(), hex(p.Color), f(p.Width), opacity)
		}
		fmt.Fprint(b, "</g>\n")
	}

	fmt.Fprint(b, "</svg>\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func writeSVGText(b *strings.Builder, t *rm.Text) {
	x := t.X + PageWidth/2
	fmt.Fprintf(b, `<g id="typed-text" font-family="sans-serif" font-size="%s" fill="black">`+"\n", f(textFontSize))
	y := t.Y + textFontSize
	for _, line := range strings.Split(t.Body, "\n") {
		if line != "" {
			fmt.Fprintf(b, `<text x="%s" y="%s">%s</text>`+"\n", f(x), f(y), xmlEscape(line))
		}
		y += textLineHeight
	}
	fmt.Fprint(b, "</g>\n")
}

// f formats a coordinate compactly.
func f(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func hex(c rgb) string {
	to := func(v float64) int { return int(v*255 + 0.5) }
	return fmt.Sprintf("#%02x%02x%02x", to(c.R), to(c.G), to(c.B))
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
