package mir

import (
	"testing"
	"unsafe"

	jscompile "github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

func TestWorkspaceARM64GoFails(t *testing.T) {
	ast, err := parser.Parse(`function f(g,n){let s=0;for(let i=0;i<n;i++)s+=g(i);return s}`, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fn, err := jscompile.Compile(ast, jscompile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := jitcompile.LowerSSA(fn.Constants[0].Fn)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ssa.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	ssa.Optimize(f)
	var w Workspace
	w.init()
	var backing *a64GoFail
	for range 2 {
		code, err := compileARM64(&w, f, abi.Encoding{ValueSize: 16, StoreRef: 1, CallGo: 1}, Settings())
		if err != nil {
			t.Fatal(err)
		}
		if len(w.arm64.goFails) == 0 {
			t.Fatal("compile made no failure patches")
		}
		if backing != nil && backing != &w.arm64.goFails[0] {
			t.Fatal("failure table was not reused")
		}
		backing = &w.arm64.goFails[0]
		for _, g := range w.arm64.goFails {
			if g != (a64GoFail{}) {
				t.Fatal("scratch table retains a call description")
			}
		}
		patched := false
		for _, st := range code.GoCalls {
			patched = patched || st.Fail != 0
		}
		if !patched {
			t.Fatal("scratch cleanup lost the calls' failure offsets")
		}
		w.Rewind()
	}
}

func TestWorkspaceARM64GoFailsTrim(t *testing.T) {
	var w Workspace
	before, used := w.Bytes(), w.scratchUsed()
	w.arm64.goFails = make([]a64GoFail, 1<<16)
	if got, want := w.Bytes()-before, capBytes(w.arm64.goFails); got != want {
		t.Fatalf("counts %d bytes, want %d", got, want)
	}
	w.Rewind()
	w.arm64.goFails = w.arm64.goFails[:1]
	if got, want := w.scratchUsed()-used, int(unsafe.Sizeof(a64GoFail{})); got != want {
		t.Fatalf("counts %d bytes used, want %d", got, want)
	}
	for range len(w.needs) {
		w.Rewind()
		if w.arm64.goFails == nil {
			t.Fatal("discarded a table a recent compile needed")
		}
	}
	w.Rewind()
	if w.arm64.goFails != nil || w.Bytes() != before {
		t.Fatalf("keeps %d bytes after the large compile aged out", w.Bytes()-before)
	}
}

// The encoded arm64 buffer is scratch memory, counted at its capacity
// while kept, and trimmed once the recent compiles no longer need it.
func TestWorkspaceARM64Code(t *testing.T) {
	var w Workspace
	before, used := w.Bytes(), w.scratchUsed()
	w.arm64.code = make([]byte, 1<<20)
	if got := w.Bytes() - before; got != cap(w.arm64.code) {
		t.Fatalf("counts %d bytes, want %d", got, cap(w.arm64.code))
	}
	w.Rewind()
	if w.arm64.code == nil {
		t.Fatal("discarded the last compile's buffer")
	}
	w.arm64.code = w.arm64.code[:16]
	if got := w.scratchUsed() - used; got != 16 {
		t.Fatalf("counts %d bytes used, want 16", got)
	}
	for range len(w.needs) {
		w.Rewind()
		if w.arm64.code == nil {
			t.Fatal("discarded a buffer a recent compile needed")
		}
	}
	w.Rewind()
	if w.arm64.code != nil || w.Bytes() != before {
		t.Fatalf("keeps %d bytes after the large compile aged out", w.Bytes()-before)
	}
}
