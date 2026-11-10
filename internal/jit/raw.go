//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || ((darwin || linux) && arm64))

package jit

import (
	"fmt"
	"runtime"
	"unsafe"
)

// RawCode is machine code another compiler made -- a regular expression's
// (internal/regexp) -- placed in a runtime's arena: entered at its start with
// a context of its maker's own, which it reads and writes as its maker says,
// and returning to Go with a plain return. It keeps the rules the new
// pipeline's code keeps at the execution boundary (see the README): it never
// moves SP, touches the frame pointer or g, or executes a call, and it is
// never a safe point, so it returns to Go often enough for the goroutine to
// be preempted.
type RawCode struct {
	code  []byte
	arena *Arena
}

// PlaceRaw publishes code in a, written then sealed executable.
func PlaceRaw(a *Arena, code []byte) (*RawCode, error) {
	placed, err := a.place(code)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return &RawCode{code: placed, arena: a}, nil
}

// PlaceRawApart publishes codes in a, as PlaceRaw does each, on pages no
// code placed before them shares and that were never executable, so that
// the arena's other code may run on other threads while they are placed.
// The codes are placed together, one after another; on an error none is.
func PlaceRawApart(a *Arena, codes ...[]byte) ([]*RawCode, error) {
	placed, err := a.placeApart(codes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	raws := make([]*RawCode, len(placed))
	for i, p := range placed {
		raws[i] = &RawCode{code: p, arena: a}
	}
	return raws, nil
}

// Base is the address the code begins at, which its maker's addresses in
// it are offsets from.
func (c *RawCode) Base() uintptr { return uintptr(unsafe.Pointer(unsafe.SliceData(c.code))) }

// Size is the memory the code takes in its arena.
func (c *RawCode) Size() int { return len(c.code) }

// Enter runs the code from its start with ctx, which must stay alive and
// unmoved until it returns.
func (c *RawCode) Enter(ctx unsafe.Pointer) {
	enterRaw(&c.code[0], ctx)
	runtime.KeepAlive(c)
}

// EnterAt runs the code from offset off with ctx, as Enter runs it from its
// start: to resume code that returned to Go part way through, at the place
// it said to go on from.
func (c *RawCode) EnterAt(off int, ctx unsafe.Pointer) {
	enterRaw(&c.code[off], ctx)
	runtime.KeepAlive(c)
}

// Close gives the memory back to the arena; it is idempotent.
func (c *RawCode) Close() error {
	if c == nil || len(c.code) == 0 {
		return nil
	}
	if err := c.arena.release(c.code); err != nil {
		return err
	}
	c.code, c.arena = nil, nil
	return nil
}

// enterRaw is enterSSA for code with a context of its maker's own.
//
//go:noescape
func enterRaw(code *byte, ctx unsafe.Pointer)
