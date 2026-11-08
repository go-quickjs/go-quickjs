package main

import (
	"encoding/binary"
	"testing"
)

// TestA64SameLayout pins when two arm64 codes' descriptions are compared:
// where their items are as long one by one. Loads of addresses of
// different lengths can add up to the same code length and still put
// every later instruction elsewhere, and so every offset a description
// has.
func TestA64SameLayout(t *testing.T) {
	code := func(words ...uint32) []byte {
		b := make([]byte, 4*len(words))
		for i, w := range words {
			binary.LittleEndian.PutUint32(b[4*i:], w)
		}
		return b
	}
	const (
		movz0 = 0xD2800000 | 1<<5 // movz x0, #1
		movk0 = 0xF2A00000 | 2<<5 // movk x0, #2, lsl 16
		movz1 = 0xD2800001 | 3<<5 // movz x1, #3
		movk1 = 0xF2A00001 | 4<<5 // movk x1, #4, lsl 16
	)
	short := code(movz0, movz1, movk1) // a load of one move, one of two
	long := code(movz0, movk0, movz1)
	if !a64SameLayout(short, short, len(short), len(short)) {
		t.Error("a code's layout differs from its own")
	}
	if a64SameLayout(short, long, len(short), len(long)) {
		t.Error("loads of one and two moves, and of two and one, are one layout")
	}
}
