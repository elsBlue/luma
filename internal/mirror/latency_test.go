package mirror

import (
	"strings"
	"testing"
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
