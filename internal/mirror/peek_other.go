//go:build !windows

package mirror

import "os"

func pipePending(f *os.File) int { return 0 }
