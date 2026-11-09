//go:build quickjs_wasm

package wasmjs

import (
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// wrapperCache maps each of wasm's objects to the JavaScript object that
// stands for it, without keeping either alive: an exported function, a
// memory, a struct is one object however often it crosses into script.
//
// It is keyed by the wasm object's address. A wrapper holds its wasm object,
// so while the wrapper lives the address is the object's; once the wrapper
// has been collected its weak pointer is nil, and the entry is replaced or
// swept.
type wrapperCache struct {
	m map[uintptr]wrapperEntry
	// sweepAt is the size the map is swept of dead entries at.
	sweepAt int
}

type wrapperEntry struct {
	w weak.Pointer[vm.Object]
}

// get returns the wrapper of the object at p, or nil.
func (c *wrapperCache) get(p unsafe.Pointer) *vm.Object {
	e, ok := c.m[uintptr(p)]
	if !ok {
		return nil
	}
	return e.w.Value()
}

// put records o as the wrapper of the object at p, which o keeps alive.
func (c *wrapperCache) put(p unsafe.Pointer, o *vm.Object) {
	if len(c.m) >= c.sweepAt {
		for k, e := range c.m {
			if e.w.Value() == nil {
				delete(c.m, k)
			}
		}
		c.sweepAt = max(64, 2*len(c.m))
	}
	c.m[uintptr(p)] = wrapperEntry{w: weak.Make(o)}
}
