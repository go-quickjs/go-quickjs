package engine

import "unsafe"

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

// nativeRun runs register code rc in frame f from pc by the native
// handlers until the frame returns.
func nativeRun(f *frame, rc *regCode, pc int32) {
	c := nativeSet(f, rc, pc)
	c.table = nativeTable
	c.callH = nativeCallHAddr
	c.goTable = uintptr(unsafe.Pointer(&regTable[0]))
	c.slowTable = uintptr(unsafe.Pointer(&regSlowTable[0]))
	wasmNativeEnter(nativeEntry, unsafe.Pointer(c))
}
