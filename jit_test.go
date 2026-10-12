package quickjs_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
)

// The opt-in is valid in ordinary builds too, where it falls back. Pin its
// independence from code-generation policy and Node compatibility options.
func TestJITOption(t *testing.T) {
	for _, node := range []bool{false, true} {
		opts := []quickjs.Option{quickjs.WithJIT(), quickjs.WithoutCodeGeneration()}
		if node {
			opts = append(opts, quickjs.WithNodeQuirks())
		}
		r := quickjs.New(opts...)
		defer r.Close()
		for _, tc := range []struct{ source, want string }{
			{`function f(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s } f(10000)`, "49995000"},
			{`let count=0; let x=f({valueOf() { count++; return 3 }}); x+":"+count`, "3:4"},
			{`typeof eval`, "undefined"},
			{`function z(n) { let a=0; for(let i=0;i<n;i++) a=-a; return a } Object.is(z(1),-0)`, "true"},
		} {
			if got := evalString(t, r, tc.source); got != tc.want {
				t.Fatalf("node=%v: %s = %s, want %s", node, tc.source, got, tc.want)
			}
		}
		if _, err := r.Eval(`Function("return 1")()`); err == nil {
			t.Fatal("JIT enabled blocked Function constructor")
		}
	}
}

// jitHotFunctions is a program with n hot functions, each run in turn, and
// its answer.
func jitHotFunctions(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "function f%d(n){let t=%d;for(let i=0;i<n;i++)t=(t*31+i+%d)|0;return t}\n", i, i, i)
	}
	b.WriteString("var fs=[")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "f%d,", i)
	}
	b.WriteString("];let t=0;for(let r=0;r<20;r++)for(const f of fs)t=(t+f(200))|0;t")
	return b.String()
}

// WithJITMemoryLimit bounds what the JIT holds, whatever the program: with
// more hot functions than fit, it holds no more than the limit, compiles
// what fits, and the answers are the interpreter's. It is apart from
// WithMemoryLimit, which it overrides the eighth of, and a script's
// ErrMemoryLimit is the same with the JIT on and off. A runtime without the
// JIT, or a build without it, takes the option and does nothing with it.
func TestJITMemoryLimitOption(t *testing.T) {
	src := jitHotFunctions(300)
	plain := quickjs.New()
	defer plain.Close()
	want := evalString(t, plain, src)
	for _, tc := range []struct {
		name  string
		opts  []quickjs.Option
		limit int64
	}{
		{"jit 256K", []quickjs.Option{quickjs.WithJIT(), quickjs.WithJITMemoryLimit(256 << 10)}, 256 << 10},
		{"jit 1M", []quickjs.Option{quickjs.WithJIT(), quickjs.WithJITMemoryLimit(1 << 20)}, 1 << 20},
		{"jit 4M over a memory limit", []quickjs.Option{quickjs.WithJIT(), quickjs.WithMemoryLimit(64 << 20), quickjs.WithJITMemoryLimit(4 << 20)}, 4 << 20},
		{"jit default", []quickjs.Option{quickjs.WithJIT(), quickjs.WithJITMemoryLimit(0)}, 8 << 20},
		{"no jit", []quickjs.Option{quickjs.WithJITMemoryLimit(256 << 10)}, 0},
	} {
		r := quickjs.New(tc.opts...)
		if got := evalString(t, r, src); got != want {
			t.Fatalf("%s: %s, want %s", tc.name, got, want)
		}
		st := hostaccess.VM(r).JITStats()
		t.Logf("%s: %+v", tc.name, st)
		r.Close()
		if st.Budget == 0 {
			// No JIT: in this build, or this runtime.
			if st.Compiled != 0 || st.Held != 0 {
				t.Fatalf("%s: the JIT ran: %+v", tc.name, st)
			}
			continue
		}
		if tc.limit == 0 {
			t.Fatalf("%s: a JIT budget without the JIT: %+v", tc.name, st)
		}
		if st.Budget != tc.limit || st.Held > st.Budget || st.Compiled == 0 {
			t.Fatalf("%s: %+v", tc.name, st)
		}
		if tc.limit == 256<<10 && st.Refused+st.Evicted == 0 {
			t.Fatalf("%s: the program fit: %+v", tc.name, st)
		}
	}
	// A script's memory limit is the same with the JIT on and off, whatever
	// the JIT's.
	for _, opts := range [][]quickjs.Option{nil, {quickjs.WithJIT(), quickjs.WithJITMemoryLimit(64 << 20)}} {
		r := quickjs.New(append(opts, quickjs.WithMemoryLimit(1<<20))...)
		_, err := r.Eval(`new ArrayBuffer(2 << 20)`)
		r.Close()
		if !errors.Is(err, quickjs.ErrMemoryLimit) {
			t.Fatalf("over a script's limit, with %d options: %v", len(opts), err)
		}
	}
}
