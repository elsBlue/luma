//go:build !windows

package main

import "fmt"

func pickFile(string) (string, error) { return "", fmt.Errorf("file picker is Windows-only") }
func clipText() (string, error)       { return "", fmt.Errorf("clipboard is Windows-only") }
func alert(title, text string)        { fmt.Println(title+":", text) }
