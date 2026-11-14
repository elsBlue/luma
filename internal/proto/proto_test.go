package proto

import (
	"bytes"
	"testing"
)

func TestTouchLayout(t *testing.T) {
	b := TouchMessage(ActionDown, 10, 20, 100, 200, 1, ButtonPrimary, ButtonPrimary)
	if len(b) != 32 {
		t.Fatalf("len %d", len(b))
	}
	if b[0] != MsgTouch || b[1] != ActionDown {
		t.Fatalf("head %v", b[:2])
	}
	for i := 2; i < 10; i++ {
		if b[i] != 0xff {
			t.Fatalf("pointer byte %d = %x", i, b[i])
		}
	}
	if b[13] != 10 || b[17] != 20 || b[18] != 0 || b[19] != 100 || b[21] != 200 {
		t.Fatalf("pos %v", b[10:22])
	}
	if b[22] != 0xff || b[23] != 0xff {
		t.Fatalf("pressure %x %x", b[22], b[23])
	}
}

func TestVideoHeader(t *testing.T) {
	h := []byte{0x80, 0, 0, 0, 0, 0, 0x04, 0x38, 0, 0, 0x09, 0x60}
	v, err := ParseVideoHeader(h)
	if err != nil || !v.Session || v.Width != 1080 || v.Height != 2400 {
		t.Fatalf("%+v %v", v, err)
	}
	media := make([]byte, 12)
	media[0] = 0x60 // config + key
	media[11] = 4
	v, err = ParseVideoHeader(media)
	if err != nil || v.Session || !v.Config || !v.Key || v.Size != 4 {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestAnnexB(t *testing.T) {
	avcc := []byte{0, 0, 0, 3, 0x67, 0x64, 0x00}
	got := AnnexB(avcc)
	want := []byte{0, 0, 0, 1, 0x67, 0x64, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("%x", got)
	}
	avcC := []byte{1, 0x64, 0, 0x1e, 0xff, 0xe1, 0x00, 0x03, 0x67, 0x64, 0x00, 0x01, 0x00, 0x02, 0x68, 0xee}
	got = AnnexB(avcC)
	if !HasSPS(got) {
		t.Fatalf("missing sps %x", got)
	}
}
