package mirror

import (
	"strings"
	"testing"
	"time"
)

func TestDisplayKeepsNewestKeyframe(t *testing.T) {
	batch := []viewPkt{
		{config: true, blob: []byte("sps")},
		{key: true, blob: []byte("k1")},
		{blob: []byte("p1")},
		{key: true, blob: []byte("k2")},
		{blob: []byte("p2")},
	}
	got := displayBlobs(batch)
	want := []string{"sps", "k2", "p2"}
	if len(got) != len(want) {
		t.Fatalf("got %d blobs", len(got))
	}
	for i, w := range want {
		if string(got[i]) != w {
			t.Fatalf("blob %d = %s", i, got[i])
		}
	}
}

func TestCodecOptionsAreLowDelayNotAllIntra(t *testing.T) {
	tiers := codecOptionTiers(60)
	if len(tiers) != 4 || tiers[3] != "" {
		t.Fatalf("tiers = %#v", tiers)
	}
	hw := tiers[0]
	for _, want := range []string{
		"max-bframes:int=0",
		"i-frame-interval:float=10",
		"low-latency:int=1",
		"priority:int=0",
		"operating-rate:int=60",
		"repeat-previous-frame-after:long=100000",
		"vendor.qti-ext-enc-low-latency.enable:int=1",
	} {
		if !strings.Contains(hw, want) {
			t.Fatalf("missing %s in %s", want, hw)
		}
	}
	if strings.Contains(hw, "latency:int=0") {
		t.Fatalf("latency 0 is not a real minimum: %s", hw)
	}
	if !strings.Contains(hw, ",latency:int=1,") {
		t.Fatalf("missing one-frame latency: %s", hw)
	}
	if strings.Contains(hw, "profile:int=65536") {
		t.Fatalf("forced baseline can leave the hardware encoder: %s", hw)
	}
	if strings.Contains(hw, "i-frame-interval:float=1,") || strings.Contains(hw, "i-frame-interval:float=0") {
		t.Fatalf("keyframe every second (or all-intra) hitches the picture: %s", hw)
	}
	if strings.Contains(tiers[1], "vendor.qti-ext-enc-low-latency") {
		t.Fatalf("std tier should drop the vendor flag: %s", tiers[1])
	}
	if !strings.Contains(tiers[2], "low-latency:int=1") {
		t.Fatalf("basic tier lost low-latency: %s", tiers[2])
	}
}

func TestChooseHardwareH264Encoder(t *testing.T) {
	log := `
[server] INFO: List of video encoders:
    --video-codec=h264 --video-encoder=c2.android.avc.encoder (sw)
    --video-codec=h264 --video-encoder=OMX.google.h264.encoder (sw)
    --video-codec=h264 --video-encoder=OMX.qcom.video.encoder.avc (hw) [vendor]
    --video-codec=h264 --video-encoder=c2.qti.avc.encoder (hw) [vendor]
    --video-codec=h264 --video-encoder=c2.qti.avc.encoder.secure (hw) [vendor] (alias for c2.qti.avc.encoder)
    --video-codec=h265 --video-encoder=c2.qti.hevc.encoder (hw) [vendor]
`
	if got := chooseH264Encoder(log); got != "c2.qti.avc.encoder" {
		t.Fatalf("got %q", got)
	}
	if got := chooseH264Encoder("    --video-codec=h264 --video-encoder=OMX.MTK.VIDEO.ENCODER.AVC\n"); got != "OMX.MTK.VIDEO.ENCODER.AVC" {
		t.Fatalf("untagged mtk %q", got)
	}
	if got := chooseH264Encoder("    --video-codec=h264 --video-encoder=c2.android.avc.encoder (sw)\n"); got != "" {
		t.Fatalf("software should not be forced: %q", got)
	}
	if shortEncoder("c2.qti.avc.encoder") != "qcom" || shortEncoder("OMX.MTK.VIDEO.ENCODER.AVC") != "mtk" {
		t.Fatal("short name")
	}
}

func TestPreviewFitsTheWindow(t *testing.T) {
	w, h := previewSize(1080, 1920)
	if w != 720 || h != 1280 {
		t.Fatalf("portrait %dx%d", w, h)
	}
	w, h = previewSize(1920, 1080)
	if w != 1280 || h != 720 {
		t.Fatalf("landscape %dx%d", w, h)
	}
	w, h = previewSize(640, 360)
	if w != 640 || h != 360 {
		t.Fatalf("small %dx%d", w, h)
	}
}

func TestDisplayAllIntraDropsOlder(t *testing.T) {
	batch := []viewPkt{
		{key: true, blob: []byte("a")},
		{key: true, blob: []byte("b")},
		{key: true, blob: []byte("c")},
	}
	got := displayBlobs(batch)
	if len(got) != 1 || string(got[0]) != "c" {
		t.Fatalf("%q", got)
	}
}

func TestPictureCountIgnoresConfigAndDropped(t *testing.T) {
	batch := []viewPkt{
		{config: true, blob: []byte("sps")},
		{key: true, blob: []byte("k1")},
		{blob: []byte("p1")},
		{key: true, blob: []byte("k2")},
		{blob: []byte("p2")},
	}
	show := displayBlobs(batch)
	if pictureCount(show, batch) != 2 {
		t.Fatalf("pics=%d show=%d", pictureCount(show, batch), len(show))
	}
	all := []viewPkt{
		{key: true, blob: []byte("a")},
		{key: true, blob: []byte("b")},
		{key: true, blob: []byte("c")},
	}
	show = displayBlobs(all)
	if pictureCount(show, all) != 1 {
		t.Fatalf("all-intra pics=%d", pictureCount(show, all))
	}
}

func TestDecodeTimingMatchesPictures(t *testing.T) {
	s := &Session{logAt: time.Now()}
	s.noteDecodeSend(2)
	s.noteDecodeDone(false)
	if s.decLast < 0 {
		t.Fatal("decode time")
	}
	s.noteDecodeDone(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.decQ) != 0 {
		t.Fatalf("queue %d", len(s.decQ))
	}
	if s.fpsN != 1 {
		t.Fatalf("fpsN %d", s.fpsN)
	}
	if s.sentN != 2 {
		t.Fatalf("sent %d", s.sentN)
	}
}
