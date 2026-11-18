package engine

import (
	"unsafe"
	_ "unsafe" // linkname
)

// nativeBuilt says the native handlers are built for this architecture.
const nativeBuilt = true

// The native code and its trampolines (native_amd64.s,
// native_handlers_amd64.s).
func wasmNativeEnter(code uintptr, ctx unsafe.Pointer)
func wasmNativeCallH()
func wasmNativeHandlers()
func wasmNativeCallHAddr() uintptr
func wasmNativeHandlersAddr() uintptr
func wasmNativeTableAddr() uintptr
func wasmNativeTablePtr() unsafe.Pointer

var (
	nativeEntry     = wasmNativeHandlersAddr()
	nativeTable     = wasmNativeTableAddr()
	nativeCallHAddr = wasmNativeCallHAddr()
)

func wasmNativeStoreRef()
func wasmNativeStoreRefAddr() uintptr

var nativeStoreRefAddr = wasmNativeStoreRefAddr()

// nativeWB is the runtime's write-barrier flag, set while the collector
// marks. The runtime keeps it for use outside by this linkname and
// promises not to change its type (runtime/mgc.go, go.dev/issue/67401).
//
//go:linkname nativeWB runtime.writeBarrier
var nativeWB struct {
	enabled bool
	pad     [3]byte
	alignme uint64
}

// nativeRun runs register code rc in frame f from pc by the native
// handlers until the frame returns.
func nativeRun(f *frame, rc *regCode, pc int32) {
	c := nativeSet(f, rc, pc)
	c.table = nativeTable
	c.callH = nativeCallHAddr
	c.goTable = uintptr(unsafe.Pointer(&regTable[0]))
	c.slowTable = uintptr(unsafe.Pointer(&regSlowTable[0]))
	c.storeRef = nativeStoreRefAddr
	c.wb = uintptr(unsafe.Pointer(&nativeWB.enabled))
	if nativeMarkFlag != nil {
		c.wb = uintptr(unsafe.Pointer(nativeMarkFlag))
	}
	wasmNativeEnter(nativeEntry, unsafe.Pointer(c))
}
