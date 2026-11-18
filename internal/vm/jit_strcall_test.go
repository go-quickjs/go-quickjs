//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "testing"

// A string method call whose method is a script function -- an object's
// own, or String.prototype's replaced -- is ordinary call feedback, which
// the JIT inlines.
func TestJITSavedStringCallScriptInlining(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			if _, err := r.Run(compileForTest(t, `
                function replacement(i){return i}
                function custom(n){var s={charCodeAt:replacement},h=0,i;for(let j=0;j<n;j++){i=j&15;h+=s.charCodeAt(++i)}return h}
                function override(n){var s="abcdefghijklmnop",h=0,i;for(let j=0;j<n;j++){i=j&15;h+=s.charCodeAt(++i)}return h}
                String.prototype.charCodeAt=replacement`)); err != nil {
				t.Fatal(err)
			}
			p := compileForTest(t, `custom(1000)+":"+override(1000)`)
			for range 12 {
				v, err := r.Run(p)
				if err != nil || v.String().Go() != "8468:8468" {
					t.Fatalf("got %v, %v; want 8468:8468", v, err)
				}
			}
			for _, name := range []string{"custom", "override"} {
				e := jitNativeForTest(t, r, name)
				inlined := false
				for _, call := range e.inlines {
					inlined = inlined || call.cl != nil && call.cl.fn.Name == "replacement"
				}
				if !inlined {
					t.Fatalf("%s did not inline its saved script method: %+v", name, e.inlines)
				}
			}
		})
	}
}

// The saved call of a sloppy method, with Node's quirks, gives
// f.arguments the argument as passed.
func TestJITSavedStringCallLegacyArguments(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true, NodeQuirks: true})
			if _, err := r.Run(compileForTest(t, savedStringCallLegacySource)); err != nil {
				t.Fatal(err)
			}
			// Compile and enter the built-in's speculation first, then
			// replace the method: its guards must give the call back.
			if _, err := r.Run(compileForTest(t, `var custom=o.charCodeAt;o="abc";
                for(let k=0;k<10;k++)legacy(0);String.prototype.charCodeAt=custom`)); err != nil {
				t.Fatal(err)
			}
			p := compileForTest(t, `legacy(42)`)
			for range 10 {
				v, err := r.Run(p)
				if err != nil || v.String().Go() != "1:42" {
					t.Fatalf("got %q, %v; want 1:42", v.String().Go(), err)
				}
			}
			jitNativeForTest(t, r, "legacy")
		})
	}
}

// charCodeAt called with an argument evaluated after its read, by the
// JIT and the interpreter alike: the built-in, a replacement, a getter
// read once a call, and a replacement made by the argument's conversion.
func TestJITSavedStringCall(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitAgreesForTest(t, `
                var saved = String.prototype.charCodeAt, reads = 0;
                function codes(s,n) {
                    let h=0;
                    for(let j=0;j<n;j++) {let i=j&3;h+=s.charCodeAt(++i);h+=s.charCodeAt(i+1)}
                    return h;
                }
                function once(s,i) {let a=s.charCodeAt(++i);return a}
            `,
				`codes("abcdef",1000)`, `codes("abcdef",1000)`, `codes("abcdef",1000)`,
				`(String.prototype.charCodeAt=function(i){return 900+i},codes("abcdef",1000))`,
				`(String.prototype.charCodeAt=saved,codes("abcdef",1000))`,
				`(Object.defineProperty(String.prototype,"charCodeAt",{configurable:true,get(){reads++;return saved}}), codes("abcdef",100)+":"+reads)`,
				`(Object.defineProperty(String.prototype,"charCodeAt",{configurable:true,writable:true,value:saved}), once("abcdef",{valueOf(){String.prototype.charCodeAt=function(){return 7};return 0}}))`,
			)
			jitNativeForTest(t, r, "codes")
		})
	}
}
