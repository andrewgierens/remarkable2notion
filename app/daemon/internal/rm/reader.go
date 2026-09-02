package rm

import (
	"encoding/binary"
	"fmt"
	"math"
)

// tagType is the low nibble of a tag, naming the wire type that follows.
type tagType uint64

const (
	tagByte1   tagType = 0x1
	tagByte2   tagType = 0x2
	tagByte4   tagType = 0x4
	tagByte8   tagType = 0x8
	tagLength4 tagType = 0xC // length-prefixed data: subblocks, strings
	tagID      tagType = 0xF
)

// reader is a bounds-checked cursor over one block's payload, implementing
// the tagged-value encoding used throughout .rm v6 files.
type reader struct {
	buf []byte
	pos int
}

func newReader(buf []byte) *reader { return &reader{buf: buf} }

func (r *reader) remaining() int { return len(r.buf) - r.pos }

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || r.remaining() < n {
		return nil, fmt.Errorf("rm: truncated data at offset %d (want %d bytes, have %d)", r.pos, n, r.remaining())
	}
	b := r.buf[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

func (r *reader) readUint8() (uint8, error) {
	b, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *reader) readUint16() (uint16, error) {
	b, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (r *reader) readUint32() (uint32, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (r *reader) readFloat32() (float32, error) {
	v, err := r.readUint32()
	return math.Float32frombits(v), err
}

func (r *reader) readFloat64() (float64, error) {
	b, err := r.take(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
}

// readVaruint reads a LEB128-style unsigned varint.
func (r *reader) readVaruint() (uint64, error) {
	var v uint64
	var shift uint
	for {
		b, err := r.readUint8()
		if err != nil {
			return 0, err
		}
		if shift >= 64 {
			return 0, fmt.Errorf("rm: varuint overflow at offset %d", r.pos)
		}
		v |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			return v, nil
		}
		shift += 7
	}
}

// peekTag decodes the next tag without consuming it. ok is false at
// end-of-buffer or if the bytes do not decode as a tag.
func (r *reader) peekTag() (index uint64, typ tagType, ok bool) {
	save := r.pos
	defer func() { r.pos = save }()
	tag, err := r.readVaruint()
	if err != nil {
		return 0, 0, false
	}
	return tag >> 4, tagType(tag & 0xF), true
}

// expectTag consumes the next tag and checks it names (index, typ).
func (r *reader) expectTag(index uint64, typ tagType) error {
	tag, err := r.readVaruint()
	if err != nil {
		return err
	}
	if tag>>4 != index || tagType(tag&0xF) != typ {
		return fmt.Errorf("rm: expected tag %d/%x, got %d/%x", index, typ, tag>>4, tag&0xF)
	}
	return nil
}

// hasTag reports whether the next value carries (index, typ).
func (r *reader) hasTag(index uint64, typ tagType) bool {
	i, t, ok := r.peekTag()
	return ok && i == index && t == typ
}

func (r *reader) readID(index uint64) (CrdtID, error) {
	if err := r.expectTag(index, tagID); err != nil {
		return CrdtID{}, err
	}
	part1, err := r.readUint8()
	if err != nil {
		return CrdtID{}, err
	}
	part2, err := r.readVaruint()
	if err != nil {
		return CrdtID{}, err
	}
	return CrdtID{part1, part2}, nil
}

func (r *reader) readBool(index uint64) (bool, error) {
	if err := r.expectTag(index, tagByte1); err != nil {
		return false, err
	}
	b, err := r.readUint8()
	return b != 0, err
}

func (r *reader) readByte(index uint64) (uint8, error) {
	if err := r.expectTag(index, tagByte1); err != nil {
		return 0, err
	}
	return r.readUint8()
}

func (r *reader) readInt(index uint64) (uint32, error) {
	if err := r.expectTag(index, tagByte4); err != nil {
		return 0, err
	}
	return r.readUint32()
}

func (r *reader) readFloat(index uint64) (float32, error) {
	if err := r.expectTag(index, tagByte4); err != nil {
		return 0, err
	}
	return r.readFloat32()
}

func (r *reader) readDouble(index uint64) (float64, error) {
	if err := r.expectTag(index, tagByte8); err != nil {
		return 0, err
	}
	return r.readFloat64()
}

// readSubblock consumes a Length4-tagged subblock and returns a reader over
// its contents.
func (r *reader) readSubblock(index uint64) (*reader, error) {
	if err := r.expectTag(index, tagLength4); err != nil {
		return nil, err
	}
	n, err := r.readUint32()
	if err != nil {
		return nil, err
	}
	b, err := r.take(int(n))
	if err != nil {
		return nil, err
	}
	return newReader(b), nil
}

// readString reads a string subblock: varuint length, ascii flag, bytes.
func (r *reader) readString(index uint64) (string, error) {
	sub, err := r.readSubblock(index)
	if err != nil {
		return "", err
	}
	n, err := sub.readVaruint()
	if err != nil {
		return "", err
	}
	if _, err := sub.readUint8(); err != nil { // is_ascii flag
		return "", err
	}
	b, err := sub.take(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readLwwID reads a last-writer-wins wrapped CRDT id.
func (r *reader) readLwwID(index uint64) (CrdtID, error) {
	sub, err := r.readSubblock(index)
	if err != nil {
		return CrdtID{}, err
	}
	if _, err := sub.readID(1); err != nil { // timestamp
		return CrdtID{}, err
	}
	return sub.readID(2)
}

// readLwwBool reads a last-writer-wins wrapped bool.
func (r *reader) readLwwBool(index uint64) (bool, error) {
	sub, err := r.readSubblock(index)
	if err != nil {
		return false, err
	}
	if _, err := sub.readID(1); err != nil {
		return false, err
	}
	return sub.readBool(2)
}

// readLwwString reads a last-writer-wins wrapped string.
func (r *reader) readLwwString(index uint64) (string, error) {
	sub, err := r.readSubblock(index)
	if err != nil {
		return "", err
	}
	if _, err := sub.readID(1); err != nil {
		return "", err
	}
	return sub.readString(2)
}
