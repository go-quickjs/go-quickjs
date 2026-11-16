//go:build quickjs_nativelayout

// Command layout prints package vm's layout as the native handlers'
// generator reads it (nativegen.Layout, as JSON): vm's NativeLayout,
// which only the quickjs_nativelayout tag builds, and the architecture
// and word size it was built for. nativegen.ReadLayout runs it on the
// host, built with the tags whose layout it reads.
package main

import (
	"encoding/json"
	"log"
	"os"
	"runtime"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/internal/vm/internal/nativegen"
)

func main() {
	offsets, sizes, consts := vm.NativeLayout()
	l := nativegen.Layout{
		GOARCH:   runtime.GOARCH,
		PtrSize:  int64(unsafe.Sizeof(uintptr(0))),
		Offsets:  map[string]int64{},
		Sizes:    map[string]int64{},
		Consts:   consts,
		JITBuilt: consts["jitBuilt"] != 0,
	}
	delete(consts, "jitBuilt")
	for k, v := range offsets {
		l.Offsets[k] = int64(v)
	}
	for k, v := range sizes {
		l.Sizes[k] = int64(v)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "\t")
	if err := enc.Encode(l); err != nil {
		log.Fatal(err)
	}
}
