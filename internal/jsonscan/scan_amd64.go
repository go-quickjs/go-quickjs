package jsonscan

import (
	"unsafe"

	"golang.org/x/sys/cpu"
)

// fast says the machine has AVX2, and the system saves its registers.
var fast = cpu.X86.HasAVX2

func ascii(s string) int {
	return asciiAVX2(unsafe.StringData(s), len(s))
}

func wide(s string, other byte) int {
	return wideAVX2(unsafe.StringData(s), len(s), other)
}

// asciiAVX2 and wideAVX2 read 32 bytes of the n at p at a time, and return the
// index of the first byte they stop at, or how many bytes they read.
//
//go:noescape
func asciiAVX2(p *byte, n int) int

//go:noescape
func wideAVX2(p *byte, n int, other byte) int
