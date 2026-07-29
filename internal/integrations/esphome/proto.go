package esphome

import (
	"encoding/binary"
	"fmt"
)

// protoField is a decoded protobuf field.
// val is uint64 for wire type 0 (varint), uint32 for wire type 5 (fixed32),
// and []byte for wire type 2 (length-delimited).
type protoField struct {
	num  int
	wire int
	val  any
}

func decodeProto(b []byte) ([]protoField, error) {
	var fields []protoField
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, fmt.Errorf("bad varint tag")
		}
		b = b[n:]
		fieldNum := int(tag >> 3)
		wireType := int(tag & 7)
		switch wireType {
		case 0: // varint
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, fmt.Errorf("bad varint field %d", fieldNum)
			}
			b = b[n:]
			fields = append(fields, protoField{fieldNum, 0, v})
		case 2: // length-delimited (string, bytes, embedded)
			l, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, fmt.Errorf("bad length for field %d", fieldNum)
			}
			b = b[n:]
			if int(l) > len(b) {
				return nil, fmt.Errorf("truncated field %d", fieldNum)
			}
			data := make([]byte, l)
			copy(data, b[:l])
			b = b[l:]
			fields = append(fields, protoField{fieldNum, 2, data})
		case 5: // 32-bit (fixed32, float)
			if len(b) < 4 {
				return nil, fmt.Errorf("truncated fixed32 field %d", fieldNum)
			}
			v := binary.LittleEndian.Uint32(b[:4])
			b = b[4:]
			fields = append(fields, protoField{fieldNum, 5, v})
		default:
			return nil, fmt.Errorf("unsupported wire type %d field %d", wireType, fieldNum)
		}
	}
	return fields, nil
}

func encodeVarint(v uint64) []byte {
	var buf [10]byte
	n := binary.PutUvarint(buf[:], v)
	return buf[:n]
}

func protoString(fieldNum int, s string) []byte {
	tag := encodeVarint(uint64(fieldNum<<3 | 2))
	l := encodeVarint(uint64(len(s)))
	b := make([]byte, 0, len(tag)+len(l)+len(s))
	b = append(b, tag...)
	b = append(b, l...)
	return append(b, s...)
}

func protoVarint(fieldNum int, v uint64) []byte {
	tag := encodeVarint(uint64(fieldNum << 3))
	return append(tag, encodeVarint(v)...)
}

func protoFixed32(fieldNum int, v uint32) []byte {
	tag := encodeVarint(uint64(fieldNum<<3 | 5))
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	return append(tag, buf[:]...)
}
