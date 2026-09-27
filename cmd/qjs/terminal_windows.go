package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableEscapes asks a Windows console to interpret the escape sequences the
// line editor draws with, reporting whether it can.
func enableEscapes(out *os.File) bool {
	h := windows.Handle(out.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
