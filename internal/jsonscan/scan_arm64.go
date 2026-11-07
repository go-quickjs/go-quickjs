//go:build !purego

package jsonscan

import "unsafe"

// fast says the machine has a way to read many bytes at a time: every arm64
// has NEON.
const fast = true

func ascii(s string) int {
	return asciiNEON(unsafe.StringData(s), len(s))
}

func wide(s string, other byte) int {
	return wideNEON(unsafe.StringData(s), len(s), other)
}

// asciiNEON and wideNEON read 16 bytes of the n at p at a time, and return the
// index of the first byte they stop at, or how many bytes they read.
//
//go:noescape
func asciiNEON(p *byte, n int) int

//go:noescape
func wideNEON(p *byte, n int, other byte) int
