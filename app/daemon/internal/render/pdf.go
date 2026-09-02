package render

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"strings"

	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/rm"
)

// PDF writes scenes as a multi-page PDF, one page per scene. No PDF library:
// stroke paths are emitted directly as content-stream operators (m/c/l + S),
// the same primitives SVG paths use.
func PDF(scenes []*rm.Scene, w io.Writer) error {
	d := newPDFDoc()

	catalog := d.reserve() // object 1
	pages := d.reserve()   // object 2
	font := d.add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	hlState := d.add("<< /Type /ExtGState /CA 0.45 /ca 0.45 >>")

	var kids []string
	for _, scene := range scenes {
		pageRef := d.addScenePage(scene, pages, font, hlState)
		kids = append(kids, pageRef)
	}

	d.fill(catalog, fmt.Sprintf("<< /Type /Catalog /Pages %s >>", d.ref(pages)))
	d.fill(pages, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>",
		strings.Join(kids, " "), len(kids)))

	return d.write(w)
}

// pdfScale maps document units to PDF points: pages come out letter-width.
const pdfScale = 612.0 / PageWidth

type pdfObj struct {
	body   string
	stream []byte // non-nil for stream objects; body is the dict
}

type pdfDoc struct {
	objs []pdfObj // index i = object i+1
}

func newPDFDoc() *pdfDoc { return &pdfDoc{} }

func (d *pdfDoc) reserve() int {
	d.objs = append(d.objs, pdfObj{})
	return len(d.objs)
}

func (d *pdfDoc) add(body string) int {
	n := d.reserve()
	d.fill(n, body)
	return n
}

func (d *pdfDoc) addStream(dictExtra string, content []byte) int {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(content)
	zw.Close()
	n := d.reserve()
	d.objs[n-1] = pdfObj{
		body:   fmt.Sprintf("<< /Length %d /Filter /FlateDecode %s>>", buf.Len(), dictExtra),
		stream: buf.Bytes(),
	}
	return n
}

func (d *pdfDoc) fill(n int, body string) { d.objs[n-1].body = body }

func (d *pdfDoc) ref(n int) string { return fmt.Sprintf("%d 0 R", n) }

// addScenePage emits one page (content stream + page object) and returns the
// page object reference.
func (d *pdfDoc) addScenePage(scene *rm.Scene, pagesObj, fontObj, hlObj int) string {
	minX, minY, maxX, maxY := bounds(scene)
	wPt := (maxX - minX) * pdfScale
	hPt := (maxY - minY) * pdfScale

	// Transform document coordinates (origin top-left, y down) to PDF
	// (origin bottom-left, y up).
	tx := func(x float64) string { return f((x - minX) * pdfScale) }
	ty := func(y float64) string { return f((maxY - y) * pdfScale) }

	var cs bytes.Buffer
	fmt.Fprintf(&cs, "1 J 1 j\n") // round caps and joins

	if scene.Text != nil {
		writePDFText(&cs, scene.Text, tx, ty)
	}

	for _, p := range scenePaths(scene) {
		fmt.Fprintf(&cs, "q\n")
		if p.Opacity < 1 {
			fmt.Fprintf(&cs, "/GHL gs\n")
		}
		fmt.Fprintf(&cs, "%s %s %s RG\n", f(p.Color.R), f(p.Color.G), f(p.Color.B))
		fmt.Fprintf(&cs, "%s w\n", f(p.Width*pdfScale))
		fmt.Fprintf(&cs, "%s %s m\n", tx(p.Start.X), ty(p.Start.Y))
		for _, s := range p.Segs {
			fmt.Fprintf(&cs, "%s %s %s %s %s %s c\n",
				tx(s.C1.X), ty(s.C1.Y), tx(s.C2.X), ty(s.C2.Y), tx(s.To.X), ty(s.To.Y))
		}
		fmt.Fprintf(&cs, "S\nQ\n")
	}

	content := d.addStream("", cs.Bytes())
	page := d.add(fmt.Sprintf(
		"<< /Type /Page /Parent %s /MediaBox [0 0 %s %s] /Resources << /Font << /F1 %s >> /ExtGState << /GHL %s >> >> /Contents %s >>",
		d.ref(pagesObj), f(wPt), f(hPt), d.ref(fontObj), d.ref(hlObj), d.ref(content)))
	return d.ref(page)
}

func writePDFText(cs *bytes.Buffer, t *rm.Text, tx, ty func(float64) string) {
	size := textFontSize * pdfScale
	leading := textLineHeight * pdfScale
	x := tx(t.X + PageWidth/2)
	// Baseline of the first line sits one font-size below the anchor.
	y := ty(t.Y + textFontSize)
	fmt.Fprintf(cs, "BT\n/F1 %s Tf\n%s TL\n%s %s Td\n", f(size), f(leading), x, y)
	for i, line := range strings.Split(t.Body, "\n") {
		if i > 0 {
			fmt.Fprintf(cs, "T*\n")
		}
		if line != "" {
			fmt.Fprintf(cs, "(%s) Tj\n", pdfEscape(line))
		}
	}
	fmt.Fprintf(cs, "ET\n")
}

func pdfEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`, "\r", `\r`)
	return r.Replace(s)
}

// write serialises the document with a correct xref table.
func (d *pdfDoc) write(w io.Writer) error {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")

	offsets := make([]int, len(d.objs))
	for i, obj := range d.objs {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\n", i+1, obj.body)
		if obj.stream != nil {
			out.WriteString("stream\n")
			out.Write(obj.stream)
			out.WriteString("\nendstream\n")
		}
		out.WriteString("endobj\n")
	}

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", len(d.objs)+1)
	out.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(d.objs)+1, xref)

	_, err := w.Write(out.Bytes())
	return err
}
