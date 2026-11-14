//go:build !windows

package adb

import "os/exec"

func hide(cmd *exec.Cmd) {}
