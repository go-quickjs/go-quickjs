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
	return strMethodRun(t, src, native, "charCodeAt", elemStrCharCodeAt)
}

// strMethodRun is strIntrinsicRun of String.prototype's method name,
// whose elemOp is op.
func strMethodRun(t *testing.T, src string, native bool, name string, op uint8) (string, int) {
	t.Helper()
	was := regNativeOn
	regNativeOn = native
	t.Cleanup(func() { regNativeOn = was })
	r := New(Config{})
	defer r.Close()
	m, err := r.Run(compileForTest(t, `String.prototype.`+name))
	if err != nil {
		t.Fatal(err)
	}
	fd := m.Object().fn()
	if fd.elemOp != op {
		t.Fatalf("%s's elemOp %d", name, fd.elemOp)
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

// codePointAt, at and charAt answer alike natively and by the Go table,
// with the collector idle and while it marks.
func TestRegNativeStringUnitCalls(t *testing.T) {
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			nativeMarking(t, marking)
			for _, tc := range stringUnitCalls {
				for k, m := range stringUnitMethods {
					// Twice, the second call with the sites' caches filled.
					src := stringUnitSource(tc.source+";\n"+tc.source, m)
					for _, native := range []bool{false, true} {
						if got := nativeRunSrc(t, src, native); got != tc.want[k] {
							t.Errorf("%s %s, native %v: got %q, want %q", m, tc.name, native, got, tc.want[k])
						}
					}
				}
			}
		})
	}
}

// call_method_w of String.prototype's codePointAt, at and charAt, its
// site's cached built-in, is answered by the native kernel without
// calling the built-in or leaving native code; the Go table calls the
// built-in each time. The built-in makes at's and charAt's string of a
// character the first time it is asked for (unitString), and the kernel
// hands out the runtime's own after that; a code unit of 256 or more,
// whose string is a new one each time, and a rope are the built-in's.
func TestRegNativeStringUnitKernels(t *testing.T) {
	nativeCounting(t)
	rows := []struct {
		method string
		op     uint8
		rows   []string
		want   string
	}{
		{"codePointAt", elemStrCodePointAt, []string{"string_codepoint_local", "string_codepoint_update", "string_codepoint_pairs"}, ":104468:104468:36056082"},
		{"at", elemStrAt, []string{"string_at_local", "string_at_negative"}, ":63:62"},
		{"charAt", elemStrCharAt, []string{"string_charat_local", "string_charat_latin1"}, ":63:63"},
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			nativeMarking(t, marking)
			for _, row := range rows {
				src := nativeKernelSource(t, row.rows...) + "var out = '';\n"
				for _, name := range row.rows {
					src += name + "(1000); out += ':' + global_res;\n"
				}
				src += `out`
				got, calls := strMethodRun(t, src, false, row.method, row.op)
				if want := 1000 * len(row.rows); got != row.want || calls != want {
					t.Fatalf("%s, Go table: got %s, %d calls of the built-in; want %s, %d", row.method, got, calls, row.want, want)
				}
				before := nativeExitOps()
				got, calls = strMethodRun(t, src, true, row.method, row.op)
				if got != row.want {
					t.Fatalf("%s, native: got %s", row.method, got)
				}
				// The first call of each site fills its cache in Go, and
				// at's and charAt's first of each of 16 characters makes
				// its string.
				if most := 17 * len(row.rows); calls > most {
					t.Fatalf("%s, native: the built-in was called %d times, more than %d", row.method, calls, most)
				}
				if n := nativeExitOps()[bytecode.RCallMW] - before[bytecode.RCallMW]; n > int64(len(row.rows)) {
					t.Fatalf("%s, native: call_method_w left native code %d times", row.method, n)
				}
			}
			// Code units past Latin-1, and ropes: the built-in's each time.
			for _, row := range rows[1:] {
				src := `function wide(n){let s="\u4e2d\u6587\u5b57",h=0;for(let j=0;j<n;j++)if(s.M(j%3)==="\u6587")h++;return h}
                    function rope(n){let a="abcdefghijklmnopqrstuvwxyz012345",h=0;for(let j=0;j<n;j++){let s=a+"6789abcdefghijklmnopqrstuvwxyz0123456789";if(s.M(j&15)==="e")h++}return h}
                    wide(999)+":"+rope(1000)`
				got, calls := strMethodRun(t, stringUnitMethod.ReplaceAllString(src, row.method), true, row.method, row.op)
				if got != "333:63" || calls < 1999 {
					t.Fatalf("%s: got %s, %d calls of the built-in; want 333:63, 1999", row.method, got, calls)
				}
			}
		})
	}
}
