package mirror

import "testing"

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
