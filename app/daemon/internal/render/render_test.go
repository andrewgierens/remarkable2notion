package render

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/rm"
)

func testScene() *rm.Scene {
	return &rm.Scene{
		Layers: []*rm.Layer{
			{
				Label:   "Layer 1",
				Visible: true,
				Lines: []rm.Line{
					{
						Tool: rm.PenFineliner2, Color: rm.ColorBlack, ThicknessScale: 2,
						Points: []rm.Point{
							{X: -100, Y: 100, Width: 2},
							{X: 0, Y: 150, Width: 2},
							{X: 100, Y: 100, Width: 2},
						},
					},
					{
						Tool: rm.PenHighlighter2, Color: rm.ColorYellow, ThicknessScale: 4,
						Points: []rm.Point{
							{X: -50, Y: 300, Width: 20},
							{X: 50, Y: 300, Width: 20},
						},
					},
				},
			},
			{
				Label:   "Hidden",
				Visible: false,
				Lines: []rm.Line{
					{Tool: rm.PenFineliner2, Points: []rm.Point{{X: 0, Y: 0, Width: 2}}},
				},
			},
		},
		Text: &rm.Text{X: -650, Y: 50, Width: 1300, Body: "Hello <world>\nsecond line"},
	}
}

func TestSVG(t *testing.T) {
	var buf bytes.Buffer
	if err := SVG(testScene(), &buf); err != nil {
		t.Fatal(err)
	}
	svg := buf.String()

	for _, want := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg"`,
		`<g id="Layer 1">`,
		`stroke="#000000"`,
		`stroke-linecap="round"`,
		` C `, // cubic segments
		`stroke-opacity="0.45"`,
		`Hello &lt;world&gt;`,
		`second line`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("SVG missing %q", want)
		}
	}
	if strings.Contains(svg, `id="Hidden"`) {
		t.Error("hidden layer must not render")
	}
	// Stroke x=-100 maps to 602 (centred coordinates shifted by width/2).
	if !strings.Contains(svg, "M 602 100") {
		t.Errorf("coordinate transform wrong:\n%s", svg[:min(len(svg), 2000)])
	}
}

func TestSVGEmptyScene(t *testing.T) {
	var buf bytes.Buffer
	if err := SVG(&rm.Scene{}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `viewBox="0 0 1404 1872"`) {
		t.Errorf("empty scene should render a blank page: %s", buf.String())
	}
}

func TestPDFStructure(t *testing.T) {
	var buf bytes.Buffer
	if err := PDF([]*rm.Scene{testScene(), {}}, &buf); err != nil {
		t.Fatal(err)
	}
	pdf := buf.Bytes()

	if !bytes.HasPrefix(pdf, []byte("%PDF-1.4")) {
		t.Fatal("missing PDF header")
	}
	if !bytes.HasSuffix(bytes.TrimSpace(pdf), []byte("%%EOF")) {
		t.Fatal("missing EOF marker")
	}
	if !bytes.Contains(pdf, []byte("/Count 2")) {
		t.Error("expected a 2-page document")
	}

	// Every xref entry must point at the object it claims.
	xrefRe := regexp.MustCompile(`(?m)^(\d{10}) 00000 n `)
	matches := xrefRe.FindAllSubmatch(pdf, -1)
	if len(matches) == 0 {
		t.Fatal("no xref entries")
	}
	for i, m := range matches {
		off, _ := strconv.Atoi(string(m[1]))
		want := fmt.Sprintf("%d 0 obj", i+1)
		if !bytes.HasPrefix(pdf[off:], []byte(want)) {
			t.Errorf("xref entry %d points at %q, want %q", i+1, pdf[off:off+min(12, len(pdf)-off)], want)
		}
	}

	// startxref must point at the xref table.
	sxRe := regexp.MustCompile(`startxref\n(\d+)\n`)
	sx := sxRe.FindSubmatch(pdf)
	if sx == nil {
		t.Fatal("missing startxref")
	}
	off, _ := strconv.Atoi(string(sx[1]))
	if !bytes.HasPrefix(pdf[off:], []byte("xref")) {
		t.Errorf("startxref points at %q", pdf[off:off+8])
	}
}

func TestPDFContentStream(t *testing.T) {
	var buf bytes.Buffer
	if err := PDF([]*rm.Scene{testScene()}, &buf); err != nil {
		t.Fatal(err)
	}
	pdf := buf.Bytes()

	// Decompress every stream and check the drawing operators.
	var content []byte
	rest := pdf
	for {
		i := bytes.Index(rest, []byte("stream\n"))
		if i < 0 {
			break
		}
		rest = rest[i+len("stream\n"):]
		j := bytes.Index(rest, []byte("\nendstream"))
		if j < 0 {
			t.Fatal("unterminated stream")
		}
		zr, err := zlib.NewReader(bytes.NewReader(rest[:j]))
		if err != nil {
			t.Fatalf("stream not flate-compressed: %v", err)
		}
		dec, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("decompress: %v", err)
		}
		content = append(content, dec...)
		rest = rest[j+len("\nendstream"):]
	}

	for _, want := range []string{" m\n", " c\n", "S\n", " RG\n", "/GHL gs", "BT\n", "(Hello <world>) Tj"} {
		if !bytes.Contains(content, []byte(want)) {
			t.Errorf("content stream missing %q in:\n%s", want, content)
		}
	}
}

func TestEraserAndUnknownColor(t *testing.T) {
	scene := &rm.Scene{Layers: []*rm.Layer{{
		Visible: true,
		Lines: []rm.Line{
			{Tool: rm.PenEraser, Color: rm.ColorBlack, Points: []rm.Point{{X: 0, Y: 0, Width: 10}, {X: 5, Y: 5, Width: 10}}},
			{Tool: rm.PenEraseArea, Points: []rm.Point{{X: 0, Y: 0}}},
			{Tool: rm.PenFineliner2, Color: rm.Color(999), Points: []rm.Point{{X: 0, Y: 0, Width: 2}, {X: 5, Y: 5, Width: 2}}},
		},
	}}}
	var buf bytes.Buffer
	if err := SVG(scene, &buf); err != nil {
		t.Fatal(err)
	}
	svg := buf.String()
	if !strings.Contains(svg, `stroke="#ffffff"`) {
		t.Error("eraser should render white")
	}
	if !strings.Contains(svg, `stroke="#000000"`) {
		t.Error("unknown colour should fall back to black")
	}
	if got := strings.Count(svg, "<path"); got != 2 {
		t.Errorf("erase-area must not render, got %d paths", got)
	}
}
