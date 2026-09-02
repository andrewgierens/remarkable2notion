package rm

import (
	"bytes"
	"fmt"
	"math"
)

// headerV6 opens every .rm v6 file; 43 bytes, space padded.
var headerV6 = []byte("reMarkable .lines file, version=6          ")

// Block type ids.
const (
	blockMigrationInfo  = 0x00
	blockSceneTree      = 0x01
	blockTreeNode       = 0x02
	blockSceneGlyphItem = 0x03
	blockSceneGroupItem = 0x04
	blockSceneLineItem  = 0x05
	blockSceneTextItem  = 0x06
	blockRootText       = 0x07
	blockAuthorIds      = 0x09
	blockPageInfo       = 0x0A
	blockSceneInfo      = 0x0D
)

// Scene item types, the first byte inside a scene item's value subblock.
const (
	itemTypeGroup = 0x02
	itemTypeLine  = 0x03
	itemTypeText  = 0x05
)

// node is a group in the scene tree while it is being assembled.
type node struct {
	id       CrdtID
	label    string
	visible  bool
	parent   CrdtID
	children []CrdtID // ordered child group ids
	lines    []Line
}

type parser struct {
	nodes   map[CrdtID]*node
	order   []CrdtID // node ids in file order
	text    *Text
	skipped int
}

// Parse reads a .rm v6 page into a Scene. Blocks that cannot be parsed are
// skipped and counted rather than failing the page.
func Parse(data []byte) (*Scene, error) {
	if len(data) < len(headerV6) || !bytes.Equal(data[:33], headerV6[:33]) {
		return nil, fmt.Errorf("rm: not a reMarkable .lines v6 file")
	}
	r := newReader(data[len(headerV6):])

	p := &parser{nodes: map[CrdtID]*node{}}
	p.getNode(RootID) // the root always exists

	for r.remaining() > 0 {
		length, err := r.readUint32()
		if err != nil {
			return nil, err
		}
		hdr, err := r.take(4) // unknown, minVersion, currentVersion, blockType
		if err != nil {
			return nil, err
		}
		payload, err := r.take(int(length))
		if err != nil {
			return nil, err
		}
		version := hdr[2]
		blockType := hdr[3]
		if err := p.parseBlock(blockType, version, newReader(payload)); err != nil {
			p.skipped++
		}
	}
	return p.assemble(), nil
}

func (p *parser) getNode(id CrdtID) *node {
	if n, ok := p.nodes[id]; ok {
		return n
	}
	n := &node{id: id, visible: true}
	p.nodes[id] = n
	p.order = append(p.order, id)
	return n
}

func (p *parser) parseBlock(blockType, version uint8, r *reader) (err error) {
	// A malformed block must never take the page down; recover and let the
	// caller count it as skipped.
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("rm: panic parsing block %#x: %v", blockType, rec)
		}
	}()

	switch blockType {
	case blockSceneTree:
		return p.parseSceneTree(r)
	case blockTreeNode:
		return p.parseTreeNode(r)
	case blockSceneGroupItem:
		return p.parseSceneItem(r, version, itemTypeGroup)
	case blockSceneLineItem:
		return p.parseSceneItem(r, version, itemTypeLine)
	case blockRootText:
		return p.parseRootText(r)
	case blockMigrationInfo, blockAuthorIds, blockPageInfo, blockSceneInfo,
		blockSceneGlyphItem, blockSceneTextItem:
		return nil // recognised, nothing to render from
	default:
		return nil // unknown block: skip silently, forward compatible
	}
}

// parseSceneTree links a node into the tree.
func (p *parser) parseSceneTree(r *reader) error {
	treeID, err := r.readID(1)
	if err != nil {
		return err
	}
	if _, err := r.readID(2); err != nil { // node_id (an item id, unused here)
		return err
	}
	if _, err := r.readBool(3); err != nil { // is_update
		return err
	}
	sub, err := r.readSubblock(4)
	if err != nil {
		return err
	}
	parentID, err := sub.readID(1)
	if err != nil {
		return err
	}
	p.getNode(treeID).parent = parentID
	return nil
}

// parseTreeNode fills in a node's label and visibility.
func (p *parser) parseTreeNode(r *reader) error {
	id, err := r.readID(1)
	if err != nil {
		return err
	}
	n := p.getNode(id)
	if label, err := r.readLwwString(2); err == nil {
		n.label = label
	} else {
		return err
	}
	if r.hasTag(3, tagLength4) {
		if visible, err := r.readLwwBool(3); err == nil {
			n.visible = visible
		}
	}
	// Optional anchor fields (7..10) are not needed for rendering.
	return nil
}

// parseSceneItem handles group and line item blocks, which share a layout:
// CRDT ids, a deleted length, and an optional typed value subblock.
func (p *parser) parseSceneItem(r *reader, version uint8, wantType uint8) error {
	parentID, err := r.readID(1)
	if err != nil {
		return err
	}
	if _, err := r.readID(2); err != nil { // item_id
		return err
	}
	if _, err := r.readID(3); err != nil { // left_id
		return err
	}
	if _, err := r.readID(4); err != nil { // right_id
		return err
	}
	deletedLength, err := r.readInt(5)
	if err != nil {
		return err
	}
	if !r.hasTag(6, tagLength4) {
		return nil // tombstone: deleted item with no value
	}
	sub, err := r.readSubblock(6)
	if err != nil {
		return err
	}
	itemType, err := sub.readUint8()
	if err != nil {
		return err
	}
	if itemType != wantType || deletedLength > 0 {
		return nil
	}

	switch itemType {
	case itemTypeGroup:
		childID, err := sub.readID(2)
		if err != nil {
			return err
		}
		parent := p.getNode(parentID)
		parent.children = append(parent.children, childID)
		p.getNode(childID) // ensure it exists even if TreeNodeBlock is missing
	case itemTypeLine:
		line, err := parseLine(sub, version)
		if err != nil {
			return err
		}
		parent := p.getNode(parentID)
		parent.lines = append(parent.lines, *line)
	}
	return nil
}

// pointSizeV1 and pointSizeV2 are the serialized point sizes for line block
// versions 1 and 2.
const (
	pointSizeV1 = 24
	pointSizeV2 = 14
)

func parseLine(r *reader, version uint8) (*Line, error) {
	tool, err := r.readInt(1)
	if err != nil {
		return nil, err
	}
	color, err := r.readInt(2)
	if err != nil {
		return nil, err
	}
	thickness, err := r.readDouble(3)
	if err != nil {
		return nil, err
	}
	startLen, err := r.readFloat(4)
	if err != nil {
		return nil, err
	}
	points, err := r.readSubblock(5)
	if err != nil {
		return nil, err
	}

	line := &Line{
		Tool:           Pen(tool),
		Color:          Color(color),
		ThicknessScale: thickness,
		StartingLength: startLen,
	}

	// Infer the point encoding from the data length rather than trusting the
	// version alone — undocumented versions appear in real notebooks and we
	// degrade gracefully rather than failing the send.
	n := points.remaining()
	var size int
	switch {
	case version >= 2 && n%pointSizeV2 == 0:
		size = pointSizeV2
	case n%pointSizeV1 == 0:
		size = pointSizeV1
	case n%pointSizeV2 == 0:
		size = pointSizeV2
	default:
		return nil, fmt.Errorf("rm: point data length %d fits no known encoding", n)
	}

	line.Points = make([]Point, 0, n/size)
	for points.remaining() >= size {
		var pt Point
		if pt.X, err = points.readFloat32(); err != nil {
			return nil, err
		}
		if pt.Y, err = points.readFloat32(); err != nil {
			return nil, err
		}
		if size == pointSizeV1 {
			if pt.Speed, err = points.readFloat32(); err != nil {
				return nil, err
			}
			if pt.Direction, err = points.readFloat32(); err != nil {
				return nil, err
			}
			if pt.Width, err = points.readFloat32(); err != nil {
				return nil, err
			}
			if pt.Pressure, err = points.readFloat32(); err != nil {
				return nil, err
			}
		} else {
			speed, err := points.readUint16()
			if err != nil {
				return nil, err
			}
			width, err := points.readUint16()
			if err != nil {
				return nil, err
			}
			dir, err := points.readUint8()
			if err != nil {
				return nil, err
			}
			pressure, err := points.readUint8()
			if err != nil {
				return nil, err
			}
			pt.Speed = float32(speed) / 4
			pt.Width = float32(width) / 4
			pt.Direction = float32(dir) * (2 * math.Pi) / 255
			pt.Pressure = float32(pressure) / 255
		}
		line.Points = append(line.Points, pt)
	}
	// Trailing values (timestamp id, move id on newer firmware) are not
	// needed for rendering; the block reader discards them with the payload.
	return line, nil
}

// parseRootText extracts the page's typed text. The CRDT sequence is read in
// file order, which matches document order for text produced by the device.
func (p *parser) parseRootText(r *reader) error {
	if _, err := r.readID(1); err != nil { // block id
		return err
	}
	outer, err := r.readSubblock(2)
	if err != nil {
		return err
	}

	text := &Text{}

	// Text items.
	items, err := outer.readSubblock(1)
	if err != nil {
		return err
	}
	itemsInner, err := items.readSubblock(1)
	if err != nil {
		return err
	}
	count, err := itemsInner.readVaruint()
	if err != nil {
		return err
	}
	var body bytes.Buffer
	for i := uint64(0); i < count; i++ {
		item, err := itemsInner.readSubblock(0)
		if err != nil {
			return err
		}
		if _, err := item.readID(2); err != nil { // item id
			return err
		}
		if _, err := item.readID(3); err != nil { // left
			return err
		}
		if _, err := item.readID(4); err != nil { // right
			return err
		}
		deleted, err := item.readInt(5)
		if err != nil {
			return err
		}
		if deleted > 0 || !item.hasTag(6, tagLength4) {
			continue
		}
		val, err := item.readSubblock(6)
		if err != nil {
			return err
		}
		// The value is either a string span (with an optional format code
		// after it) or a bare format code.
		if val.hasTag(2, tagLength4) {
			s, err := val.readString(2)
			if err != nil {
				return err
			}
			body.WriteString(s)
		}
	}
	text.Body = body.String()

	// Formatting map (subblock 2) is skipped: not needed to render plain text.
	if outer.hasTag(2, tagLength4) {
		if _, err := outer.readSubblock(2); err != nil {
			return err
		}
	}

	// Position.
	if outer.hasTag(3, tagLength4) {
		pos, err := outer.readSubblock(3)
		if err != nil {
			return err
		}
		if text.X, err = pos.readFloat64(); err != nil {
			return err
		}
		if text.Y, err = pos.readFloat64(); err != nil {
			return err
		}
	}

	// Width.
	if outer.hasTag(4, tagByte4) {
		if text.Width, err = outer.readFloat(4); err != nil {
			return err
		}
	}

	if text.Body != "" {
		p.text = text
	}
	return nil
}

// assemble flattens the node tree into ordered layers. Layers are the
// children of the root; nested groups fold into their owning layer.
func (p *parser) assemble() *Scene {
	scene := &Scene{Text: p.text, SkippedBlocks: p.skipped}

	root := p.nodes[RootID]
	layerIDs := root.children
	if len(layerIDs) == 0 {
		// No explicit group items: fall back to every non-root node that
		// claims root as parent, in file order.
		for _, id := range p.order {
			n := p.nodes[id]
			if id != RootID && n.parent == RootID {
				layerIDs = append(layerIDs, id)
			}
		}
	}

	seen := map[CrdtID]bool{RootID: true}
	for _, id := range layerIDs {
		n, ok := p.nodes[id]
		if !ok || seen[id] {
			continue
		}
		layer := &Layer{ID: id, Label: n.label, Visible: n.visible}
		p.collect(n, layer, seen)
		scene.Layers = append(scene.Layers, layer)
	}

	// Lines attached to the root or to orphaned nodes still deserve to be
	// rendered: put them in a synthetic layer at the bottom of the stack.
	orphan := &Layer{ID: RootID, Label: "", Visible: true, Lines: root.lines}
	for _, id := range p.order {
		if !seen[id] {
			p.collect(p.nodes[id], orphan, seen)
		}
	}
	if len(orphan.Lines) > 0 {
		scene.Layers = append([]*Layer{orphan}, scene.Layers...)
	}
	return scene
}

// collect gathers a node's lines and those of its nested groups into layer.
func (p *parser) collect(n *node, layer *Layer, seen map[CrdtID]bool) {
	if seen[n.id] && n.id != RootID {
		return
	}
	seen[n.id] = true
	layer.Lines = append(layer.Lines, n.lines...)
	for _, childID := range n.children {
		if child, ok := p.nodes[childID]; ok && !seen[childID] {
			p.collect(child, layer, seen)
		}
	}
}
