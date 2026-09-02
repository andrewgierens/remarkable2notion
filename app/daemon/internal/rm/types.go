// Package rm parses reMarkable .rm v6 files into a scene tree, scoped to
// what rendering needs: layers, strokes with pen/colour/width, and typed
// text. The block structure is ported from ricklupton/rmscene, the reference
// implementation. Unknown blocks and undocumented pen types degrade
// gracefully instead of failing the parse.
package rm

import "fmt"

// CrdtID identifies items in the scene's CRDT structures.
type CrdtID struct {
	Part1 uint8
	Part2 uint64
}

// RootID is the id of the scene's root group.
var RootID = CrdtID{0, 1}

func (id CrdtID) String() string { return fmt.Sprintf("%d:%d", id.Part1, id.Part2) }

// IsZero reports whether the id is the null id.
func (id CrdtID) IsZero() bool { return id.Part1 == 0 && id.Part2 == 0 }

// Pen tool ids as stored in line items.
type Pen int

const (
	PenPaintbrush1       Pen = 0
	PenPencil1           Pen = 1
	PenBallpoint1        Pen = 2
	PenMarker1           Pen = 3
	PenFineliner1        Pen = 4
	PenHighlighter1      Pen = 5
	PenEraser            Pen = 6
	PenSharpPencil1      Pen = 7
	PenEraseArea         Pen = 8
	PenPaintbrush2       Pen = 12
	PenMechanicalPencil2 Pen = 13
	PenPencil2           Pen = 14
	PenBallpoint2        Pen = 15
	PenMarker2           Pen = 16
	PenFineliner2        Pen = 17
	PenHighlighter2      Pen = 18
	PenCalligraphy       Pen = 21
	PenShader            Pen = 23
)

// IsEraser reports whether strokes from this tool erase rather than draw.
func (p Pen) IsEraser() bool { return p == PenEraser || p == PenEraseArea }

// IsHighlighter reports whether the tool paints translucent ink.
func (p Pen) IsHighlighter() bool {
	return p == PenHighlighter1 || p == PenHighlighter2 || p == PenShader
}

// Color ids as stored in line items.
type Color int

const (
	ColorBlack       Color = 0
	ColorGray        Color = 1
	ColorWhite       Color = 2
	ColorYellow      Color = 3
	ColorGreen       Color = 4
	ColorPink        Color = 5
	ColorBlue        Color = 6
	ColorRed         Color = 7
	ColorGrayOverlap Color = 8
	ColorHLGreen     Color = 9
	ColorCyan        Color = 10
	ColorMagenta     Color = 11
	ColorHLYellow    Color = 12
)

// Point is one sample of a stroke.
type Point struct {
	X, Y      float32
	Speed     float32
	Direction float32 // radians
	Width     float32 // rendered width at this point, document units
	Pressure  float32 // 0..1
}

// Line is one stroke.
type Line struct {
	Tool           Pen
	Color          Color
	ThicknessScale float64
	StartingLength float32
	Points         []Point
}

// Text is the page's typed text block, positioned in document coordinates.
type Text struct {
	X, Y  float64
	Width float32
	Body  string // spans joined in document order, '\n' between paragraphs
}

// Layer is one drawing layer: a group directly under the scene root.
type Layer struct {
	ID      CrdtID
	Label   string
	Visible bool
	Lines   []Line
}

// Scene is a parsed page.
type Scene struct {
	Layers []*Layer
	Text   *Text // nil when the page has no typed text
	// SkippedBlocks counts blocks that could not be parsed and were
	// ignored; non-zero means the render may be missing content.
	SkippedBlocks int
}
