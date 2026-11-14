package mirror

import "testing"

func TestQualitiesStayAt1080(t *testing.T) {
	if len(Qualities) != 4 {
		t.Fatalf("got %d presets", len(Qualities))
	}
	for _, q := range Qualities {
		if q.MaxSize <= 0 || q.MaxSize > 1920 {
			t.Fatalf("%s max size %d", q.Name, q.MaxSize)
		}
		if q.FPS > 60 || q.FPS < 30 {
			t.Fatalf("%s fps %d", q.Name, q.FPS)
		}
		if cappedSize(q.MaxSize) != q.MaxSize {
			t.Fatalf("%s was not capped", q.Name)
		}
	}
	if Qualities[2].Name != "1080p" || Qualities[3].MaxSize != 1920 {
		t.Fatalf("youtube preset %+v", Qualities[3])
	}
	if cappedSize(0) != 1920 || cappedSize(4000) != 1920 {
		t.Fatal("cap")
	}
}
