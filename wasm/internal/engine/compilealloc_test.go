package engine

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// blockChain is a module whose function f has a chain of n nested blocks,
// a br_table at the innermost, and after each block i a return of i: the
// shape of a Go program's resumption dispatch.
func blockChain(t *testing.T, n int) *Code {
	t.Helper()
	var labels strings.Builder
	for i := range n {
		fmt.Fprintf(&labels, " %d", i)
	}
	body := fmt.Sprintf("(br_table%s (local.get 0))", labels.String())
	for i := range n {
		body = fmt.Sprintf("(block %s) (return (i32.const %d))", body, i)
	}
	b, err := wat.Compile([]byte(`(module (func (export "f") (param i32) (result i32) ` + body + `))`))
	if err != nil {
		t.Fatal(err)
	}
	m, derr := decoder.Decode(b, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
		t.Fatal(derr)
	}
	return NewCode(m)
}

// TestBlockChainCompile checks that a chain of nested blocks, run as one
// group, branches to each block's end, and that compiling it takes memory
// in proportion to the chain, not to its square.
func TestBlockChainCompile(t *testing.T) {
	const n = 300
	inst, err := Instantiate(context.Background(), blockChain(t, n), nil)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := inst.Export("f")
	for _, k := range []int{0, 1, 2, n / 2, n - 2, n - 1, n, n + 5} {
		res, err := Invoke(context.Background(), f.Func, []Val{{N: uint64(k)}})
		if err != nil {
			t.Fatal(err)
		}
		if want := min(k, n-1); int(res[0].N) != want {
			t.Errorf("f(%d) = %d, want %d", k, res[0].N, want)
		}
	}

	bytes := func(n int) uint64 {
		c := blockChain(t, n)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		compileFunction(c, 0)
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	small, large := bytes(1000), bytes(4000)
	if large > 6*small {
		t.Errorf("compiling a chain of 1,000 blocks allocated %d bytes, of 4,000 %d", small, large)
	}
}
