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
func wasmNativeExtTableAddr() uintptr
func wasmNativeExtTablePtr() unsafe.Pointer

var (
	nativeEntry     = wasmNativeHandlersAddr()
	nativeTable     = wasmNativeTableAddr()
	nativeCallHAddr = wasmNativeCallHAddr()
	nativeExtTable  = wasmNativeExtTableAddr()
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
	c.extTable = nativeExtTable
	c.callH = nativeCallHAddr
	c.goTable = uintptr(unsafe.Pointer(&nativeExitTable[0]))
	c.slowTable = uintptr(unsafe.Pointer(&regSlowTable[0]))
	c.storeRef = nativeStoreRefAddr
	c.wb = uintptr(unsafe.Pointer(&nativeWB.enabled))
	if nativeMarkFlag != nil {
		c.wb = uintptr(unsafe.Pointer(nativeMarkFlag))
	}
	wasmNativeEnter(nativeEntry, unsafe.Pointer(c))
}

// nativeExitTable is the exit stub's table of Go handlers: nativeExitRun
// for every operation.
var nativeExitTable [256]regHandler

// nativeGo says which operations of the first table, and of the second
// (rExt's, by X), have no native handler: those an exit runs in Go.
var nativeGo, nativeExtGo [256]bool

func init() {
	for i := range nativeExitTable {
		nativeExitTable[i] = nativeExitRun
	}
	t := (*[256]uintptr)(wasmNativeTablePtr())
	ext := (*[256]uintptr)(wasmNativeExtTablePtr())
	exit := t[rInvalid]
	for i := range t {
		nativeGo[i] = t[i] == exit
		nativeExtGo[i] = ext[i] == exit
	}
}

// nativeExitRun runs the instruction native code left to Go, and after it,
// as the Go table's loop does, every instruction that has no native
// handler either, so that a run of them costs one exit: it answers where
// native code goes on, as the instruction's handler does. Going on in Go
// past instructions native code runs, to spare a loop that mixes the two
// an exit a turn, measured slower on every workload but the SIMD one.
func nativeExitRun(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	pc = regTable[uint8(w)](l, w, pc, f)
	code := unsafe.Pointer(&f.rc.code[0])
	for pc >= 0 {
		w = *(*uint64)(unsafe.Add(code, uintptr(pc)<<3))
		if op := uint8(w); !nativeGo[op] && (op != uint8(rExt) || !nativeExtGo[uint8(w>>8)]) {
			break
		}
		pc = regTable[uint8(w)](l, w, pc, f)
	}
	return pc
}
