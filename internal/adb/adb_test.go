package adb

import "testing"

func TestParseDevices(t *testing.T) {
	text := "List of devices attached\nR58N123 device product:husky model:Pixel_8 device:husky\nABC unauthorized\n\n"
	list := ParseDevices(text)
	if len(list) != 2 {
		t.Fatalf("%+v", list)
	}
	if list[0].Model != "Pixel_8" || list[0].Label() != "Pixel 8" {
		t.Fatalf("%+v", list[0])
	}
	if list[1].State != "unauthorized" {
		t.Fatalf("%+v", list[1])
	}
}
