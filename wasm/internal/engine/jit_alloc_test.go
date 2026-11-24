//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"math"
	"runtime"
	"strings"
	"testing"
)

// TestJITAllocStubConvention checks the allocation stub's frame, in the
// baseline's code and optimized: its Go runs on the goroutine that entered
// the code, from a frame the collector and the unwinder walk through to the
// function that entered it -- which may grow the stack and collect -- and
// the results are the interpreter's, the code having kept what it needs
// across the stub (WASM_JIT_BARRIER=clobber runs it with the stub leaving
// junk in every register it may).
func TestJITAllocStubConvention(t *testing.T) {
	want, werr := runCases(t, poolInstance(t, math.MaxInt32, false), poolCases())
	for _, opt := range []bool{false, true} {
		var calls int
		var bad []string
		g := goid()
		codeSwitch(t, func(c *codeTest) {
			c.jit.allocHook = func() {
				calls++
				pcs := make([]uintptr, 64)
				frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
				after, via := false, false
				for {
					fr, more := frames.Next()
					if after {
						if !strings.Contains(fr.Function, "jit.(*RawCode).EnterAt") && !strings.HasSuffix(fr.Function, ".enterRaw") {
							bad = append(bad, "wasmJITAlloc's caller is "+fr.Function)
						}
						after = false
					}
					if strings.HasSuffix(fr.Function, ".wasmJITAlloc") || strings.HasSuffix(fr.Function, ".wasmJITAllocClobber") {
						via, after = true, true
					}
					if !more {
						break
					}
				}
				if !via {
					bad = append(bad, "not called through wasmJITAlloc")
				}
				if got := goid(); got != g {
					bad = append(bad, "goroutine "+got+", want "+g)
				}
				if deep(2000) != 2000*2001/2 {
					bad = append(bad, "the deep recursion's sum")
				}
				if calls%8 == 1 {
					runtime.GC()
				}
			}
		})
		inst := poolInstance(t, 0, opt)
		got, gerr := runCases(t, inst, poolCases())
		sameOutcomes(t, map[bool]string{false: "baseline", true: "opt"}[opt], poolCases(), want, werr, got, gerr)
		if calls == 0 {
			t.Fatal("the allocation stub was not called")
		}
		if len(bad) > 0 {
			t.Fatalf("%d calls; %s", calls, strings.Join(bad[:min(len(bad), 5)], "; "))
		}
	}
}

// BenchmarkJITAlloc times an allocation by each of native code's paths, in
// the baseline's code and optimized, through the allocation stub and
// through the exit it replaces: structs and short arrays from the pools,
// whose fills are the stub's or the exit's, and arrays of 1 KiB, past what
// the pools hold, each made by the stub or by the exit.
func BenchmarkJITAlloc(b *testing.B) {
	const src = `(module
  (type $s (struct (field (mut i64)) (field (mut i64)) (field (mut i64))))
  (type $a (array (mut i32)))
  (func (export "structs") (param $n i32)
    (loop $l
      (drop (struct.new_default $s))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1))))))
  (func (export "arrays") (param $n i32) (param $len i32)
    (loop $l
      (drop (array.new_default $a (local.get $len)))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))))`
	for _, tier := range []struct {
		name string
		inst func(testing.TB, string) *Instance
	}{{"baseline", func(t testing.TB, src string) *Instance { return nativeInstance(t, src) }}, {"opt", optInstance}} {
		for _, path := range []string{"stub", "exit"} {
			for _, w := range []struct {
				name string
				args []Val
			}{{"struct", []Val{{N: 1000}}}, {"array8", []Val{{N: 1000}, {N: 8}}}, {"array1k", []Val{{N: 1000}, {N: 256}}}} {
				b.Run(tier.name+"/"+w.name+"/"+path, func(b *testing.B) {
					if path == "exit" {
						codeSwitch(b, func(c *codeTest) { c.jit.noAllocStub = true })
					}
					inst := tier.inst(b, src)
					fn := "structs"
					if len(w.args) > 1 {
						fn = "arrays"
					}
					ext, _ := inst.Export(fn)
					b.ResetTimer()
					for range b.N {
						if _, err := Invoke(nil, ext.Func, w.args); err != nil {
							b.Fatal(err)
						}
					}
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1000, "ns/alloc")
				})
			}
		}
	}
}
