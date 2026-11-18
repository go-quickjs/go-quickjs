//go:build amd64 || arm64

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The saved string calls answer alike natively and by the Go table, with
// the collector idle and while it marks.
func TestRegNativeSavedStringCalls(t *testing.T) {
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			nativeMarking(t, marking)
			for _, tc := range savedStringCalls {
				for _, native := range []bool{false, true} {
					// Twice, the second call with the site's cache filled.
					src := tc.source + ";\n" + tc.source
					if got := nativeRunSrc(t, src, native); got != tc.want {
						t.Errorf("%s, native %v: got %q, want %q", tc.name, native, got, tc.want)
					}
				}
			}
		})
	}
}

// strIntrinsicRun runs src natively, or by the Go table, on a runtime
// whose String.prototype.charCodeAt counts the calls of its Go function,
// and returns its value and that count. The built-in keeps its elemOp,
// so the native handlers still know it.
func strIntrinsicRun(t *testing.T, src string, native bool) (string, int) {
	t.Helper()
	was := regNativeOn
	regNativeOn = native
	t.Cleanup(func() { regNativeOn = was })
	r := New(Config{})
	defer r.Close()
	m, err := r.Run(compileForTest(t, `String.prototype.charCodeAt`))
	if err != nil {
		t.Fatal(err)
	}
	fd := m.Object().fn()
	if fd.elemOp != elemStrCharCodeAt {
		t.Fatalf("charCodeAt's elemOp %d", fd.elemOp)
	}
	calls, call := 0, fd.native
	fd.native = func(r *Runtime, this Value, args []Value) (Value, error) {
		calls++
		return call(r, this, args)
	}
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if r.frameDepth != 0 {
		t.Fatalf("frame depth %d left", r.frameDepth)
	}
	return v.String().Go(), calls
}

// call_method_w of String.prototype.charCodeAt, its site's cached
// built-in, is answered by the native kernel without calling the
// built-in or leaving native code; the Go table calls the built-in each
// time. A rope receiver, a concatenation of 64 bytes or more, is the
// built-in's, by nativeBuiltin's call.
func TestRegNativeStrIntrinsic(t *testing.T) {
	nativeCounting(t)
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			nativeMarking(t, marking)
			src := nativeKernelSource(t, "string_charcode_update", "string_charcode_add") + `
                string_charcode_update(1000);
                var a = global_res;
                string_charcode_add(1000);
                a + ":" + global_res`
			got, calls := strIntrinsicRun(t, src, false)
			if got != "104468:104468" || calls != 2000 {
				t.Fatalf("Go table: got %s, %d calls of the built-in; want 104468:104468, 2000", got, calls)
			}
			before := nativeExitOps()
			got, calls = strIntrinsicRun(t, src, true)
			if got != "104468:104468" {
				t.Fatalf("native: got %s", got)
			}
			// The first call of each site fills its cache in Go.
			if calls > 2 {
				t.Fatalf("native: the built-in was called %d times", calls)
			}
			if n := nativeExitOps()[bytecode.RCallMW] - before[bytecode.RCallMW]; n > 2 {
				t.Fatalf("native: call_method_w left native code %d times", n)
			}
			got, calls = strIntrinsicRun(t, `
                function rope(n){let a="abcdefghijklmnopqrstuvwxyz012345",h=0,i;for(let j=0;j<n;j++){let s=a+"6789abcdefghijklmnopqrstuvwxyz0123456789";i=(j&15)-1;h+=s.charCodeAt(++i)}return ''+h}
                rope(1000)`, true)
			if got != "104468" || calls < 1000 {
				t.Fatalf("rope: got %s, %d calls of the built-in", got, calls)
			}
		})
	}
}

// A script function in the built-in's place, an object's own or
// String.prototype's, is the site's cached callee like any other, which
// the call handler enters; the built-in put back is the kernel's again.
func TestRegNativeStrIntrinsicOverride(t *testing.T) {
	nativeCounting(t)
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			nativeMarking(t, marking)
			for _, name := range []string{"string_charcode_custom", "string_charcode_override"} {
				before := nativeExitOps()
				got := nativeRunSrc(t, nativeKernelSource(t, "extra_charcode_custom", name)+name+`(1000); ''+global_res`, true)
				if got != "8468" {
					t.Fatalf("%s: got %s", name, got)
				}
				if n := nativeExitOps()[bytecode.RCallMW] - before[bytecode.RCallMW]; n > 2 {
					t.Fatalf("%s: the call missed its cache %d times", name, n)
				}
			}
			got, calls := strIntrinsicRun(t, `
                var saved=String.prototype.charCodeAt;
                function read(i){let v="abc".charCodeAt(i+0);return v}
                function many(){let h=0;for(let i=0;i<1000;i++)h+=read(i&1);return h}
                var out=[many()];
                String.prototype.charCodeAt=function(i){return i+7};
                out.push(many());
                String.prototype.charCodeAt=saved;
                out.push(many());
                out.join("|")`, true)
			// The built-in is called where the site's cache is filled
			// with it, at its first call and again after the override.
			if got != "97500|7500|97500" || calls > 2 {
				t.Fatalf("got %s, %d calls of the built-in; want 97500|7500|97500, at most 2", got, calls)
			}
		})
	}
}
