//go:build windows

package mirror

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var peekNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

func pipePending(f *os.File) int {
	if f == nil {
		return 0
	}
	var avail uint32
	r, _, _ := peekNamedPipe.Call(
		uintptr(f.Fd()),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&avail)),
		0,
	)
	if r == 0 {
		return 0
	}
	return int(avail)
}
