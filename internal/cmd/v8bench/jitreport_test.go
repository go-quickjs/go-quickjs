//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// Native arm64 dumps select compilediff's arm64 decoder, as the cross
// compiles do, and omit the arena's trailing alignment bytes.
func TestNativeCodeDump(t *testing.T) {
	hook, cross, annotate := vm.JITCodeHook, vm.JITCrossHook, mir.Annotate
	t.Cleanup(func() {
		vm.JITCodeHook, vm.JITCrossHook, mir.Annotate = hook, cross, annotate
	})
	path := filepath.Join(t.TempDir(), "code.jsonl")
	dump := startCodeDump(path)
	// Closed before the directory is removed, which Windows refuses
	// while the file is open.
	t.Cleanup(func() { dump.Close() })
	f, err := ssa.Build(&ir.Program{
		Code: []ir.Instruction{{Op: ir.Return, Left: ir.Literal(ir.Float(1))}},
		Maps: []ir.StateMap{{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := mir.CompileARM64(f, abi.Encoding{ValueSize: 16, StoreRef: 1})
	if err != nil {
		t.Fatal(err)
	}
	text := make([]byte, 64)
	copy(text, []byte{0xC0, 0x03, 0x5F, 0xD6}) // RET on arm64
	c.Text = 4
	vm.JITCodeHook(vm.JITFunction{Name: "f"}, text, c)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec jitCode
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	want, arch := text, ""
	if runtime.GOARCH == "arm64" {
		want, arch = text[:4], "arm64"
	}
	if rec.Arch != arch || rec.Text != 4 || !bytes.Equal(rec.Bytes, want) {
		t.Fatalf("arch %q, text %d, bytes %x; want arch %q, bytes %x", rec.Arch, rec.Text, rec.Bytes, arch, want)
	}
}
