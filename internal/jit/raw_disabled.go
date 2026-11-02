//go:build !quickjs_jit || android || ios || !((linux && amd64) || (windows && amd64) || (darwin && arm64))

package jit

import "unsafe"

// RawCode is machine code another compiler made; a build without a backend
// places none (see raw.go).
type RawCode struct{}

// PlaceRaw reports that this build cannot run native code.
func PlaceRaw(*Arena, []byte) (*RawCode, error) { return nil, ErrUnavailable }

// Base is 0: there is no code.
func (c *RawCode) Base() uintptr { return 0 }

// Size is 0: there is no code.
func (c *RawCode) Size() int { return 0 }

// Enter is never called: PlaceRaw places nothing.
func (c *RawCode) Enter(unsafe.Pointer) { panic("jit: no native code in this build") }

// Close does nothing.
func (c *RawCode) Close() error { return nil }
