package rm

import (
	"math"
	"testing"
)

func TestParseRejectsNonV6(t *testing.T) {
	if _, err := Parse([]byte("not a lines file")); err == nil {
		t.Fatal("expected error for garbage input")
	}
	if _, err := Parse([]byte("reMarkable .lines file, version=5          ")); err == nil {
		t.Fatal("expected error for v5 file")
	}
}

func TestParseEmptyFile(t *testing.T) {
	f := newFile()
	scene, err := Parse(f.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(scene.Layers) != 0 || scene.Text != nil || scene.SkippedBlocks != 0 {
		t.Fatalf("empty file gave %+v", scene)
	}
}

func testLine(points ...Point) Line {
	return Line{
		Tool:           PenFineliner2,
		Color:          ColorBlack,
		ThicknessScale: 2.0,
		Points:         points,
	}
}

func TestParseSingleLayerWithStrokes(t *testing.T) {
	layerID := CrdtID{0, 11}
	f := newFile()
	f.sceneTree(layerID, RootID)
	f.treeNode(RootID, "", true)
	f.treeNode(layerID, "Layer 1", true)
	f.groupItem(RootID, CrdtID{1, 12}, layerID)
	f.lineItemV2(layerID, CrdtID{1, 13}, testLine(
		Point{X: 10, Y: 20, Width: 2.5, Speed: 4, Pressure: 128.0 / 255, Direction: float32(math.Pi)},
		Point{X: 30, Y: 40, Width: 3, Speed: 8, Pressure: 1},
	))

	scene, err := Parse(f.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(scene.Layers) != 1 {
		t.Fatalf("got %d layers, want 1", len(scene.Layers))
	}
	layer := scene.Layers[0]
	if layer.Label != "Layer 1" || !layer.Visible {
		t.Errorf("layer = %+v", layer)
	}
	if len(layer.Lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(layer.Lines))
	}
	line := layer.Lines[0]
	if line.Tool != PenFineliner2 || line.Color != ColorBlack || line.ThicknessScale != 2.0 {
		t.Errorf("line = %+v", line)
	}
	if len(line.Points) != 2 {
		t.Fatalf("got %d points, want 2", len(line.Points))
	}
	p := line.Points[0]
	if p.X != 10 || p.Y != 20 || p.Width != 2.5 {
		t.Errorf("point = %+v", p)
	}
	if math.Abs(float64(p.Pressure)-128.0/255) > 0.01 {
		t.Errorf("pressure = %v", p.Pressure)
	}
	if math.Abs(float64(p.Direction)-math.Pi) > 0.05 {
		t.Errorf("direction = %v", p.Direction)
	}
}

func TestParseV1Points(t *testing.T) {
	layerID := CrdtID{0, 11}
	f := newFile()
	f.sceneTree(layerID, RootID)
	f.treeNode(layerID, "L", true)
	f.groupItem(RootID, CrdtID{1, 12}, layerID)
	f.lineItemV1(layerID, CrdtID{1, 13}, testLine(
		Point{X: 1, Y: 2, Speed: 3, Direction: 4, Width: 5, Pressure: 0.5},
	))

	scene, err := Parse(f.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	p := scene.Layers[0].Lines[0].Points[0]
	if p != (Point{X: 1, Y: 2, Speed: 3, Direction: 4, Width: 5, Pressure: 0.5}) {
		t.Errorf("point = %+v", p)
	}
}

func TestParseMultipleLayersOrdered(t *testing.T) {
	l1, l2 := CrdtID{0, 11}, CrdtID{0, 21}
	f := newFile()
	f.sceneTree(l1, RootID)
	f.sceneTree(l2, RootID)
	f.treeNode(l1, "Bottom", true)
	f.treeNode(l2, "Top", false)
	f.groupItem(RootID, CrdtID{1, 12}, l1)
	f.groupItem(RootID, CrdtID{1, 22}, l2)
	f.lineItemV2(l2, CrdtID{1, 23}, testLine(Point{X: 1, Y: 1, Width: 2}))

	scene, err := Parse(f.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(scene.Layers) != 2 {
		t.Fatalf("got %d layers, want 2", len(scene.Layers))
	}
	if scene.Layers[0].Label != "Bottom" || scene.Layers[1].Label != "Top" {
		t.Errorf("layer order: %q, %q", scene.Layers[0].Label, scene.Layers[1].Label)
	}
	if scene.Layers[1].Visible {
		t.Error("Top layer should be hidden")
	}
	if len(scene.Layers[1].Lines) != 1 {
		t.Errorf("Top layer lines = %d", len(scene.Layers[1].Lines))
	}
}

func TestParseDeletedLineSkipped(t *testing.T) {
	layerID := CrdtID{0, 11}
	f := newFile()
	f.sceneTree(layerID, RootID)
	f.treeNode(layerID, "L", true)
	f.groupItem(RootID, CrdtID{1, 12}, layerID)
	// A tombstone: deleted_length > 0, no value subblock.
	f.block(blockSceneLineItem, 2, func(b *wbuf) {
		b.id(1, layerID)
		b.id(2, CrdtID{1, 13})
		b.id(3, CrdtID{0, 0})
		b.id(4, CrdtID{0, 0})
		b.intv(5, 3)
	})

	scene, err := Parse(f.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(scene.Layers[0].Lines); got != 0 {
		t.Fatalf("deleted line should not render, got %d lines", got)
	}
	if scene.SkippedBlocks != 0 {
		t.Errorf("tombstone is valid, not a skip: %d", scene.SkippedBlocks)
	}
}

func TestParseUnknownBlockSkippedGracefully(t *testing.T) {
	layerID := CrdtID{0, 11}
	f := newFile()
	f.sceneTree(layerID, RootID)
	f.treeNode(layerID, "L", true)
	f.groupItem(RootID, CrdtID{1, 12}, layerID)
	f.block(0x7F, 1, func(b *wbuf) { b.WriteString("future block format") })
	// A corrupt line block must not take the page down.
	f.block(blockSceneLineItem, 2, func(b *wbuf) { b.WriteString("garbage") })
	f.lineItemV2(layerID, CrdtID{1, 14}, testLine(Point{X: 1, Y: 2, Width: 2}))

	scene, err := Parse(f.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(scene.Layers) != 1 || len(scene.Layers[0].Lines) != 1 {
		t.Fatalf("good content must survive bad blocks: %+v", scene)
	}
	if scene.SkippedBlocks != 1 {
		t.Errorf("skipped = %d, want 1 (unknown types don't count, corrupt ones do)", scene.SkippedBlocks)
	}
}

func TestParseRootText(t *testing.T) {
	f := newFile()
	f.rootText(-100.5, 30.25, 900, []string{"Hello ", "world", "\nSecond line"})

	scene, err := Parse(f.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if scene.Text == nil {
		t.Fatal("text missing")
	}
	if scene.Text.Body != "Hello world\nSecond line" {
		t.Errorf("body = %q", scene.Text.Body)
	}
	if scene.Text.X != -100.5 || scene.Text.Y != 30.25 || scene.Text.Width != 900 {
		t.Errorf("position = %+v", scene.Text)
	}
}

func TestParseOrphanLinesGetALayer(t *testing.T) {
	// Lines attached directly to the root with no group items at all.
	f := newFile()
	f.lineItemV2(RootID, CrdtID{1, 13}, testLine(Point{X: 5, Y: 6, Width: 1}))

	scene, err := Parse(f.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(scene.Layers) != 1 || len(scene.Layers[0].Lines) != 1 {
		t.Fatalf("orphan lines must render: %+v", scene)
	}
}
