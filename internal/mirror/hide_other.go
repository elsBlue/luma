//go:build !windows

package mirror

import "os/exec"

func hide(cmd *exec.Cmd) {}
