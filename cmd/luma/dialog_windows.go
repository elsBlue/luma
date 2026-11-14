//go:build windows

package main

import (
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

func pickFile(filter string) (string, error) {
	script := `
Add-Type -AssemblyName System.Windows.Forms
$f = New-Object System.Windows.Forms.OpenFileDialog
$f.Filter = '` + filter + `'
$f.Multiselect = $false
if ($f.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { $f.FileName }
`
	cmd := exec.Command("powershell", "-STA", "-NoProfile", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func clipText() (string, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "Get-Clipboard -Raw")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

func alert(title, text string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	t, _ := syscall.UTF16PtrFromString(title)
	b, _ := syscall.UTF16PtrFromString(text)
	_, _, _ = proc.Call(0, uintptr(unsafe.Pointer(b)), uintptr(unsafe.Pointer(t)), 0)
}
