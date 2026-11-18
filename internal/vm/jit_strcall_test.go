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

// codePointAt, at and charAt by the JIT as the interpreter gives them,
// each case run until its functions are compiled, idle and marking.
func TestJITStringUnitCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			for _, tc := range stringUnitCalls {
				for k, m := range stringUnitMethods {
					r := jitRuntimeForTest(t, Config{JIT: true})
					p := compileForTest(t, stringUnitSource(tc.source, m))
					for round := range 12 {
						v, err := r.Run(p)
						if err != nil {
							t.Fatalf("%s %s round %d: %v", m, tc.name, round, err)
						}
						if got := v.String().Go(); got != tc.want[k] {
							t.Fatalf("%s %s round %d: got %q, want %q", m, tc.name, round, got, tc.want[k])
						}
					}
				}
			}
		})
	}
}

// A loop of codePointAt, at and charAt calls, compiled, with the
// built-ins, replaced and put back, by the JIT and the interpreter alike.
func TestJITStringUnitLoops(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitAgreesForTest(t, `
                var saved = [String.prototype.codePointAt, String.prototype.at, String.prototype.charAt];
                function units(s,n) {
                    let h=0;
                    for(let j=0;j<n;j++) {
                        let i=(j%(s.length+3))-2;
                        h+=s.codePointAt(++i)|0;
                        let a=s.at(i-1), c=s.charAt(i+1);
                        if(a===undefined)h+=7;else h+=a.length+a.charCodeAt(0);
                        h+=c.length*3;
                        if(c===s[i+1])h+=11;
                    }
                    return h;
                }
            `,
				`units("ab\u00e9\ud83d\ude00z",1000)`, `units("ab\u00e9\ud83d\ude00z",1000)`, `units("abcdef",1000)`,
				`(String.prototype.at=function(i){return "q"+i},units("abcdef",1000))`,
				`(String.prototype.at=saved[1],String.prototype.codePointAt=function(i){return 1000+i},units("abcdef",1000))`,
				`(String.prototype.codePointAt=saved[0],units("ab\u00e9\ud83d\ude00z",1000))`,
			)
			jitNativeForTest(t, r, "units")
		})
	}
}
