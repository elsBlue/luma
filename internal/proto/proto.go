package proto

import (
	"encoding/binary"
	"fmt"
)

const (
	ServerVersion = "5.0.1"

	MsgKeycode       byte = 0
	MsgText          byte = 1
	MsgTouch         byte = 2
	MsgScroll        byte = 3
	MsgBackOrScreen  byte = 4
	MsgNotifPanel    byte = 5
	MsgSettingsPanel byte = 6
	MsgCollapse      byte = 7
	MsgSetClipboard  byte = 9
	MsgDisplayPower  byte = 10
	MsgRotate        byte = 11

	ActionDown byte = 0
	ActionUp   byte = 1
	ActionMove byte = 2

	ButtonPrimary uint32 = 1

	KeyHome       uint32 = 3
	KeyBack       uint32 = 4
	KeyDpadUp     uint32 = 19
	KeyDpadDown   uint32 = 20
	KeyDpadLeft   uint32 = 21
	KeyDpadRight  uint32 = 22
	KeyVolumeUp   uint32 = 24
	KeyVolumeDown uint32 = 25
	KeyPower      uint32 = 26
	KeyTab        uint32 = 61
	KeySpace      uint32 = 62
	KeyEnter      uint32 = 66
	KeyDel        uint32 = 67
	KeyEscape     uint32 = 111
	KeyForwardDel uint32 = 112
	KeyMoveHome   uint32 = 122
	KeyMoveEnd    uint32 = 123
	KeyPageUp     uint32 = 92
	KeyPageDown   uint32 = 93
	KeyAppSwitch  uint32 = 187

	CodecH264 uint32 = 0x68323634
	CodecH265 uint32 = 0x68323635

	flagConfig  uint64 = 1 << 62
	flagKey     uint64 = 1 << 61
	ptsMask     uint64 = (1 << 61) - 1
	flagSession byte   = 0x80
)

func putU16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }

func KeyMessage(action byte, code, repeat, meta uint32) []byte {
	b := make([]byte, 14)
	b[0] = MsgKeycode
	b[1] = action
	putU32(b[2:], code)
	putU32(b[6:], repeat)
	putU32(b[10:], meta)
	return b
}

func TextMessage(text string) []byte {
	raw := []byte(text)
	if len(raw) > 300 {
		raw = raw[:300]
	}
	b := make([]byte, 5+len(raw))
	b[0] = MsgText
	putU32(b[1:], uint32(len(raw)))
	copy(b[5:], raw)
	return b
}

func TouchMessage(action byte, x, y, w, h int, pressure float32, actionButton, buttons uint32) []byte {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	if pressure < 0 {
		pressure = 0
	}
	if pressure > 1 {
		pressure = 1
	}
	b := make([]byte, 32)
	b[0] = MsgTouch
	b[1] = action
	putU64(b[2:], ^uint64(0))
	putU32(b[10:], uint32(int32(x)))
	putU32(b[14:], uint32(int32(y)))
	putU16(b[18:], uint16(w))
	putU16(b[20:], uint16(h))
	putU16(b[22:], floatToU16(pressure))
	putU32(b[24:], actionButton)
	putU32(b[28:], buttons)
	return b
}

func ScrollMessage(x, y, w, h int, vscroll float32) []byte {
	b := make([]byte, 21)
	b[0] = MsgScroll
	putU32(b[1:], uint32(int32(x)))
	putU32(b[5:], uint32(int32(y)))
	putU16(b[9:], uint16(w))
	putU16(b[11:], uint16(h))
	putU16(b[13:], 0)
	putU16(b[15:], floatToI16(clamp(vscroll/16, -1, 1)))
	putU32(b[17:], 0)
	return b
}

func BackMessage(action byte) []byte { return []byte{MsgBackOrScreen, action} }

func Simple(msg byte) []byte { return []byte{msg} }

func DisplayPower(on bool) []byte {
	v := byte(0)
	if on {
		v = 1
	}
	return []byte{MsgDisplayPower, v}
}

func ClipboardMessage(seq uint64, text string, paste bool) []byte {
	raw := []byte(text)
	b := make([]byte, 14+len(raw))
	b[0] = MsgSetClipboard
	putU64(b[1:], seq)
	if paste {
		b[9] = 1
	}
	putU32(b[10:], uint32(len(raw)))
	copy(b[14:], raw)
	return b
}

func floatToU16(f float32) uint16 {
	u := uint32(f * 65536)
	if u > 0xffff {
		u = 0xffff
	}
	return uint16(u)
}

func floatToI16(f float32) uint16 {
	i := int32(f * 32768)
	if i > 0x7fff {
		i = 0x7fff
	}
	if i < -0x8000 {
		i = -0x8000
	}
	return uint16(int16(i))
}

func clamp(v, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type VideoHeader struct {
	Session bool
	Config  bool
	Key     bool
	PTS     uint64
	Width   int
	Height  int
	Size    int
}

func ParseVideoHeader(h []byte) (VideoHeader, error) {
	if len(h) != 12 {
		return VideoHeader{}, fmt.Errorf("header must be 12 bytes")
	}
	if h[0]&flagSession != 0 {
		return VideoHeader{
			Session: true,
			Width:   int(binary.BigEndian.Uint32(h[4:8])),
			Height:  int(binary.BigEndian.Uint32(h[8:12])),
		}, nil
	}
	pts := binary.BigEndian.Uint64(h[:8])
	return VideoHeader{
		Config: pts&flagConfig != 0,
		Key:    pts&flagKey != 0,
		PTS:    pts & ptsMask,
		Size:   int(binary.BigEndian.Uint32(h[8:12])),
	}, nil
}

func AnnexB(packet []byte) []byte {
	if isAnnexB(packet) {
		return append([]byte(nil), packet...)
	}
	if out, ok := avcCToAnnexB(packet); ok {
		return out
	}
	if out, ok := avccToAnnexB(packet); ok {
		return out
	}
	out := make([]byte, 0, len(packet)+4)
	out = append(out, 0, 0, 0, 1)
	return append(out, packet...)
}

func HasSPS(annex []byte) bool {
	i := 0
	for i+4 < len(annex) {
		if annex[i] == 0 && annex[i+1] == 0 && annex[i+2] == 1 {
			if nalType(annex[i+3]) == 7 {
				return true
			}
			i += 3
			continue
		}
		if i+5 < len(annex) && annex[i] == 0 && annex[i+1] == 0 && annex[i+2] == 0 && annex[i+3] == 1 {
			if nalType(annex[i+4]) == 7 {
				return true
			}
			i += 4
			continue
		}
		i++
	}
	return false
}

func nalType(b byte) int { return int(b & 0x1f) }

func isAnnexB(b []byte) bool {
	if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1 {
		return true
	}
	return len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1
}

func avcCToAnnexB(b []byte) ([]byte, bool) {
	if len(b) < 7 || b[0] != 1 {
		return nil, false
	}
	pos := 5
	var out []byte
	if pos >= len(b) {
		return nil, false
	}
	nSPS := int(b[pos] & 0x1f)
	pos++
	for i := 0; i < nSPS; i++ {
		nal, next, ok := take16(b, pos)
		if !ok {
			return nil, false
		}
		out = appendStart(out, nal)
		pos = next
	}
	if pos >= len(b) {
		return nil, false
	}
	nPPS := int(b[pos])
	pos++
	for i := 0; i < nPPS; i++ {
		nal, next, ok := take16(b, pos)
		if !ok {
			return nil, false
		}
		out = appendStart(out, nal)
		pos = next
	}
	return out, len(out) > 0
}

func take16(b []byte, pos int) ([]byte, int, bool) {
	if pos+2 > len(b) {
		return nil, 0, false
	}
	n := int(b[pos])<<8 | int(b[pos+1])
	pos += 2
	if n <= 0 || pos+n > len(b) {
		return nil, 0, false
	}
	return b[pos : pos+n], pos + n, true
}

func appendStart(dst, nal []byte) []byte {
	dst = append(dst, 0, 0, 0, 1)
	return append(dst, nal...)
}

func avccToAnnexB(b []byte) ([]byte, bool) {
	if len(b) < 5 {
		return nil, false
	}
	var out []byte
	i := 0
	for i+4 <= len(b) {
		n := int(binary.BigEndian.Uint32(b[i:]))
		i += 4
		if n <= 0 || n > len(b)-i {
			return nil, false
		}
		out = appendStart(out, b[i:i+n])
		i += n
	}
	if i != len(b) || len(out) == 0 {
		return nil, false
	}
	return out, true
}
