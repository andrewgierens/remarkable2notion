package rm

// A minimal .rm v6 writer producing the same tagged block encoding the
// parser consumes. Round-trip tests lock the wire format, and integration
// tests elsewhere use BuildSimplePage to synthesise device pages.

import (
	"bytes"
	"encoding/binary"
	"math"
)

type wbuf struct{ bytes.Buffer }

func (w *wbuf) u8(v uint8)   { w.WriteByte(v) }
func (w *wbuf) u16(v uint16) { binary.Write(w, binary.LittleEndian, v) }
func (w *wbuf) u32(v uint32) { binary.Write(w, binary.LittleEndian, v) }
func (w *wbuf) f32(v float32) {
	w.u32(math.Float32bits(v))
}
func (w *wbuf) f64(v float64) {
	binary.Write(w, binary.LittleEndian, math.Float64bits(v))
}

func (w *wbuf) varuint(v uint64) {
	for v >= 0x80 {
		w.u8(byte(v) | 0x80)
		v >>= 7
	}
	w.u8(byte(v))
}

func (w *wbuf) tag(index uint64, typ tagType) { w.varuint(index<<4 | uint64(typ)) }

func (w *wbuf) id(index uint64, v CrdtID) {
	w.tag(index, tagID)
	w.u8(v.Part1)
	w.varuint(v.Part2)
}

func (w *wbuf) boolv(index uint64, v bool) {
	w.tag(index, tagByte1)
	if v {
		w.u8(1)
	} else {
		w.u8(0)
	}
}

func (w *wbuf) intv(index uint64, v uint32) {
	w.tag(index, tagByte4)
	w.u32(v)
}

func (w *wbuf) floatv(index uint64, v float32) {
	w.tag(index, tagByte4)
	w.f32(v)
}

func (w *wbuf) doublev(index uint64, v float64) {
	w.tag(index, tagByte8)
	w.f64(v)
}

func (w *wbuf) subblock(index uint64, fill func(*wbuf)) {
	var inner wbuf
	fill(&inner)
	w.tag(index, tagLength4)
	w.u32(uint32(inner.Len()))
	w.Write(inner.Bytes())
}

func (w *wbuf) stringv(index uint64, s string) {
	w.subblock(index, func(b *wbuf) {
		b.varuint(uint64(len(s)))
		b.u8(1) // ascii flag
		b.WriteString(s)
	})
}

func (w *wbuf) lwwString(index uint64, ts CrdtID, s string) {
	w.subblock(index, func(b *wbuf) {
		b.id(1, ts)
		b.stringv(2, s)
	})
}

func (w *wbuf) lwwBool(index uint64, ts CrdtID, v bool) {
	w.subblock(index, func(b *wbuf) {
		b.id(1, ts)
		b.boolv(2, v)
	})
}

// file assembles whole test files.
type file struct{ wbuf }

func newFile() *file {
	f := &file{}
	f.Write(headerV6)
	return f
}

func (f *file) block(blockType, version uint8, fill func(*wbuf)) {
	var payload wbuf
	fill(&payload)
	f.u32(uint32(payload.Len()))
	f.u8(0) // unknown
	f.u8(version)
	f.u8(version)
	f.u8(blockType)
	f.Write(payload.Bytes())
}

func (f *file) sceneTree(treeID, parentID CrdtID) {
	f.block(blockSceneTree, 1, func(b *wbuf) {
		b.id(1, treeID)
		b.id(2, CrdtID{0, 0})
		b.boolv(3, false)
		b.subblock(4, func(s *wbuf) { s.id(1, parentID) })
	})
}

func (f *file) treeNode(id CrdtID, label string, visible bool) {
	f.block(blockTreeNode, 1, func(b *wbuf) {
		b.id(1, id)
		b.lwwString(2, CrdtID{0, 10}, label)
		b.lwwBool(3, CrdtID{0, 11}, visible)
	})
}

func (f *file) groupItem(parentID, itemID, childID CrdtID) {
	f.block(blockSceneGroupItem, 1, func(b *wbuf) {
		b.id(1, parentID)
		b.id(2, itemID)
		b.id(3, CrdtID{0, 0})
		b.id(4, CrdtID{0, 0})
		b.intv(5, 0)
		b.subblock(6, func(s *wbuf) {
			s.u8(itemTypeGroup)
			s.id(2, childID)
		})
	})
}

// lineItemV2 writes a version-2 line block with 14-byte points.
func (f *file) lineItemV2(parentID, itemID CrdtID, line Line) {
	f.block(blockSceneLineItem, 2, func(b *wbuf) {
		b.id(1, parentID)
		b.id(2, itemID)
		b.id(3, CrdtID{0, 0})
		b.id(4, CrdtID{0, 0})
		b.intv(5, 0)
		b.subblock(6, func(s *wbuf) {
			s.u8(itemTypeLine)
			s.intv(1, uint32(line.Tool))
			s.intv(2, uint32(line.Color))
			s.doublev(3, line.ThicknessScale)
			s.floatv(4, line.StartingLength)
			s.subblock(5, func(pts *wbuf) {
				for _, p := range line.Points {
					pts.f32(p.X)
					pts.f32(p.Y)
					pts.u16(uint16(p.Speed * 4))
					pts.u16(uint16(p.Width * 4))
					pts.u8(uint8(p.Direction * 255 / (2 * math.Pi)))
					pts.u8(uint8(p.Pressure * 255))
				}
			})
			s.id(6, CrdtID{0, 99}) // timestamp
		})
	})
}

// lineItemV1 writes a version-1 line block with 24-byte float points.
func (f *file) lineItemV1(parentID, itemID CrdtID, line Line) {
	f.block(blockSceneLineItem, 1, func(b *wbuf) {
		b.id(1, parentID)
		b.id(2, itemID)
		b.id(3, CrdtID{0, 0})
		b.id(4, CrdtID{0, 0})
		b.intv(5, 0)
		b.subblock(6, func(s *wbuf) {
			s.u8(itemTypeLine)
			s.intv(1, uint32(line.Tool))
			s.intv(2, uint32(line.Color))
			s.doublev(3, line.ThicknessScale)
			s.floatv(4, line.StartingLength)
			s.subblock(5, func(pts *wbuf) {
				for _, p := range line.Points {
					pts.f32(p.X)
					pts.f32(p.Y)
					pts.f32(p.Speed)
					pts.f32(p.Direction)
					pts.f32(p.Width)
					pts.f32(p.Pressure)
				}
			})
			s.id(6, CrdtID{0, 99})
		})
	})
}

func (f *file) rootText(x, y float64, width float32, spans []string) {
	f.block(blockRootText, 1, func(b *wbuf) {
		b.id(1, CrdtID{0, 0})
		b.subblock(2, func(outer *wbuf) {
			outer.subblock(1, func(items *wbuf) {
				items.subblock(1, func(inner *wbuf) {
					inner.varuint(uint64(len(spans)))
					for i, span := range spans {
						inner.subblock(0, func(item *wbuf) {
							item.id(2, CrdtID{1, uint64(20 + i)})
							item.id(3, CrdtID{0, 0})
							item.id(4, CrdtID{0, 0})
							item.intv(5, 0)
							item.subblock(6, func(v *wbuf) {
								v.stringv(2, span)
							})
						})
					}
				})
			})
			outer.subblock(2, func(*wbuf) {}) // empty formatting map
			outer.subblock(3, func(pos *wbuf) {
				pos.f64(x)
				pos.f64(y)
			})
			outer.floatv(4, width)
		})
	})
}

// BuildSimplePage synthesises a complete one-layer v6 page containing the
// given lines and optional text — the shape xochitl writes for a simple
// notebook page. Used by integration tests across packages.
func BuildSimplePage(lines []Line, text *Text) []byte {
	f := newFile()
	layerID := CrdtID{0, 11}
	f.sceneTree(layerID, RootID)
	f.treeNode(layerID, "Layer 1", true)
	f.groupItem(RootID, CrdtID{1, 12}, layerID)
	for i, line := range lines {
		f.lineItemV2(layerID, CrdtID{1, uint64(20 + i)}, line)
	}
	if text != nil {
		f.rootText(text.X, text.Y, text.Width, []string{text.Body})
	}
	return f.Bytes()
}
