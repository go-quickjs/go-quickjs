//go:build !windows

package main

import "os"

// enableEscapes reports whether a terminal interprets escape sequences, which
// every terminal outside Windows does.
func enableEscapes(*os.File) bool { return true }
