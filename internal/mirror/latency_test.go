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
	full := tiers[0]
	for _, want := range []string{
		"profile:int=65536",
		"max-bframes:int=0",
		"i-frame-interval:float=1",
		"low-latency:int=1",
		"priority:int=0",
		"operating-rate:int=60",
		"repeat-previous-frame-after:long=16666",
		"vendor.qti-ext-enc-low-latency.enable:int=1",
	} {
		if !strings.Contains(full, want) {
			t.Fatalf("missing %s in %s", want, full)
		}
	}
	if strings.Contains(full, "i-frame-interval:float=0,") || strings.HasSuffix(full, "i-frame-interval:float=0") {
		t.Fatalf("all-intra must not be requested: %s", full)
	}
	if !strings.Contains(tiers[2], "low-latency:int=1") {
		t.Fatalf("basic tier lost low-latency: %s", tiers[2])
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
