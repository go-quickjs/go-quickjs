package vm

import "testing"

// TestTreeDeadZone pins that the instructions of a binding's dead zone give
// in the tree tier what they give in the interpreter, and that a function
// with them runs as a tree: a for (let ...) loop, whose binding is marked
// before it is initialized; a read or a write of a let before its
// declaration, directly and from a closure; an assignment to a const,
// directly and from a closure; and parameter lists with defaults, which
// start in the dead zone and are bound one at a time.
func TestTreeDeadZone(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
		// inner names a closure of f's that is to run as a tree too.
		inner string
	}{
		{name: "forLet", src: `function f(n) { let s = 0; for (let i = 0; i < n; i++) s += i; return s } f(10)`, want: "45"},
		{name: "nestedForLet", src: `function f() { let s = ''; for (let i = 0; i < 3; i++) for (let j = 0; j < 2; j++) s += i + '' + j + ','; return s } f()`, want: "00,01,10,11,20,21,"},
		{name: "forLetClosures", src: `function f() { var a = []; for (let i = 0; i < 3; i++) a.push(function () { return i }); var x = a[0], y = a[1], z = a[2]; return x() + ',' + y() + ',' + z() } f()`, want: "0,1,2"},
		{name: "readBefore", src: `function f() { try { return x } catch (e) { return e.name + ': ' + e.message } let x = 1 } f()`, want: `ReferenceError: cannot access "x" before initialization`},
		{name: "writeBefore", src: `function f() { try { x = 2 } catch (e) { return e.name + ': ' + e.message } let x = 1 } f()`, want: `ReferenceError: cannot access "x" before initialization`},
		{name: "readAfter", src: `function f(k) { switch (k) { case 0: let y = 1; return y; case 1: try { return y } catch (e) { return e.name } } } f(0) + ',' + f(1)`, want: "1,ReferenceError"},
		{name: "constWrite", src: `function f() { const x = 1; try { x = 2 } catch (e) { return e.name + ': ' + e.message + ' ' + x } } f()`, want: `TypeError: assignment to constant variable "x" 1`},
		{name: "upvalueRead", src: `function f() { function g() { return x } let r; try { r = g() } catch (e) { r = e.name } let x = 5; return r + g() } f()`, want: "ReferenceError5", inner: "g"},
		{name: "upvalueWriteBefore", src: `function f() { function g() { x = 2 } try { g() } catch (e) { return e.name + ': ' + e.message } let x } f()`, want: `ReferenceError: cannot access "x" before initialization`, inner: "g"},
		{name: "upvalueWrite", src: `function f() { function g() { x = 2 } let x = 1; g(); return x } f()`, want: "2", inner: "g"},
		{name: "upvalueConst", src: `function f() { const x = 1; function g() { x = 2 } try { g() } catch (e) { return e.name + ': ' + e.message + ' ' + x } } f()`, want: `TypeError: assignment to constant variable "x" 1`, inner: "g"},
		{name: "defaults", src: `function f(a, b = a + 1) { return a + b } f(1) + ',' + f(1, 5) + ',' + f(1, undefined) + ',' + f(1, null)`, want: "3,6,3,1"},
		{name: "defaultLater", src: `function f(a = b, b) { return a } var r; try { f() } catch (e) { r = e.name + ': ' + e.message } r + ',' + f(1)`, want: `ReferenceError: cannot access "b" before initialization,1`},
		{name: "defaultSelf", src: `function f(a = a) { return a } var r; try { f() } catch (e) { r = e.name } r + ',' + f(2)`, want: "ReferenceError,2"},
		{name: "defaultArguments", src: `function f(a, b = 2) { return arguments.length + ':' + a + b } f() + ',' + f(1) + ',' + f(1, 3, 4)`, want: "0:undefined2,1:12,3:13"},
		{name: "defaultCalls", src: `function f(a, b = a * 2) { return a + b } var s = 0; for (var i = 0; i < 100; i++) s += f(i); s`, want: "14850"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				fn := compileForTest(t, tt.src)
				r := New(Config{})
				v, err := r.Run(fn)
				if err != nil {
					r.Close()
					t.Fatalf("tree %v: %v", on, err)
				}
				got, err := r.ToString(v)
				if err != nil || got.Go() != tt.want {
					t.Errorf("tree %v: got %v, %v; want %s", on, got, err, tt.want)
				}
				if r.frameDepth != 0 || r.stackTop != 0 {
					t.Errorf("tree %v: left %d frames and %d stack slots", on, r.frameDepth, r.stackTop)
				}
				r.Close()
				if on {
					assertExceptionTree(t, fn, "f")
					if tt.inner != "" {
						for _, k := range fn.Constants {
							if k.Fn != nil && k.Fn.Name == "f" {
								assertExceptionTree(t, k.Fn, tt.inner)
							}
						}
					}
				}
			}
		})
	}
}
