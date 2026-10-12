package vm

import "testing"

// TestCaptureRoom pins the bindings a closure is the first to capture being
// made in one allocation: each is still its own binding, shared with every
// other closure over it, and a binding made afresh -- a loop's let -- is a
// new one, on both tiers.
func TestCaptureRoom(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
	}{
		{name: "shared", src: `function f() { var a = 1, b = 2, c = 3; var g = function () { return a + b + c }; var h = function () { b = 20; c = 30; return a }; var k = function (x) { a = x; return b }; return [g(), h(), g(), k(100), g()].join() } f()`, want: "6,1,51,20,150"},
		{name: "partly open", src: `function f() { var a = 1, b = 2, c = 3; var g = function () { return a++ }; var h = function () { return a + b + c++ }; g(); return [h(), h(), a, c].join() } f()`, want: "7,8,2,5"},
		{name: "after return", src: `function mk() { var x = 1, y = 2, z = 3; return [function () { return x + y + z }, function (v) { y = v }] } function f() { var p = mk(), q = mk(); p[1](10); return p[0]() + ',' + q[0]() } f()`, want: "14,6"},
		{name: "loop lets", src: `function f() { var fs = []; for (let i = 0; i < 3; i++) { let j = i * 10, k = i * 100; fs.push(function () { return i + j + k }) } return fs.map(function (g) { return g() }).join() } f()`, want: "0,111,222"},
		{name: "duplicate", src: `function f(a) { var b = a; function g() { return a + b + (function () { return a * b })() } return g() } f(3)`, want: "15"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				r := New(Config{})
				v, err := r.Run(compileForTest(t, tt.src))
				if err != nil {
					r.Close()
					t.Fatalf("tree %v: %v", on, err)
				}
				got, err := r.ToString(v)
				if err != nil || got.Go() != tt.want {
					t.Errorf("tree %v: got %v, %v; want %s", on, got, err, tt.want)
				}
				r.Close()
			}
		})
	}
}

// TestCaptureRoomAllocs pins what making a closure over three of its
// maker's bindings costs: the function object, and one allocation for the
// three bindings.
func TestCaptureRoomAllocs(t *testing.T) {
	defer SetTreeTier(true)
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, `function f(a, b, c) { return function () { return a + b + c } }; for (let i = 0; i < 10; i++) f(1, 2, 3); f`))
		if err != nil {
			t.Fatal(err)
		}
		args := []Value{Int(1), Int(2), Int(3)}
		if n := testing.AllocsPerRun(100, func() {
			if _, err := r.Call(v, Undefined, args); err != nil {
				t.Fatal(err)
			}
		}); n != 2 {
			t.Errorf("tree %v: %v allocations a call, want 2", on, n)
		}
		r.Close()
	}
}

// TestArgumentsRoom pins an arguments object made with its first elements
// in its own allocation: what it holds and how it grows past them, mapped
// and not, on both tiers.
func TestArgumentsRoom(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
	}{
		{name: "counts", src: `function f() { return arguments.length + ':' + Array.prototype.join.call(arguments) } [f(), f(1), f(1, 2), f(1, 2, 3), f(1, 2, 3, 4, 5), f(1, 2, 3, 4, 5, 6, 7), f(1, 2, 3, 4, 5, 6, 7, 8, 9)].join(' ')`, want: "0: 1:1 2:1,2 3:1,2,3 5:1,2,3,4,5 7:1,2,3,4,5,6,7 9:1,2,3,4,5,6,7,8,9"},
		{name: "grows", src: `function f() { Array.prototype.push.call(arguments, 'x', 'y'); arguments[9] = 'z'; return arguments.length + ':' + Array.prototype.join.call(arguments, '') + ':' + arguments[9] } f(1, 2)`, want: "4:12xy:z"},
		{name: "mapped", src: `function f(a, b) { arguments[0] = 10; b = 20; return a + ',' + arguments[1] + ',' + arguments[2] } f(1, 2, 3)`, want: "10,20,3"},
		{name: "strict", src: `function f(a) { 'use strict'; arguments[0] = 10; return a + ',' + arguments[0] + ',' + arguments.length } f(1, 2)`, want: "1,10,2"},
		{name: "iterate", src: `function f() { return [...arguments].reverse().join() } f(1, 2, 3)`, want: "3,2,1"},
		{name: "mappedShared", src: `function f(a, b, c) { var g = function () { return b }; var args = arguments; args[1] = 'B'; c = 'C'; return g() + args[2] + a + args.length } f(1, 2, 3, 4)`, want: "BC14"},
		{name: "mappedFew", src: `function f(a, b, c) { arguments[0] = 9; arguments[2] = 7; return a + ',' + c + ',' + arguments.length } f(1)`, want: "9,undefined,1"},
		{name: "duplicates", src: `function f(a, a, b) { arguments[0] = 'x'; arguments[1] = 'y'; b = 'z'; return a + arguments[0] + arguments[2] } f(1, 2, 3)`, want: "yxz"},
		{name: "duplicateUnpassed", src: `function f(a, a) { arguments[0] = 5; a = 7; return a + ',' + arguments[0] } f(1)`, want: "7,5"},
		{name: "strictCallee", src: `function f() { 'use strict'; return arguments } function g() { var a = f(), b = f(); var d = Object.getOwnPropertyDescriptor(a, 'callee'); var r = (d.get === Object.getOwnPropertyDescriptor(b, 'callee').get) + ',' + d.configurable; try { a.callee } catch (e) { r += ',' + e.name } try { Object.defineProperty(a, 'callee', { get() {} }) } catch (e) { r += ',' + e.name } return r + ',' + (Object.getOwnPropertyDescriptor(b, 'callee').set === d.get) } g()`, want: "true,false,TypeError,TypeError,true"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				r := New(Config{})
				v, err := r.Run(compileForTest(t, tt.src))
				if err != nil {
					r.Close()
					t.Fatalf("tree %v: %v", on, err)
				}
				got, err := r.ToString(v)
				if err != nil || got.Go() != tt.want {
					t.Errorf("tree %v: got %v, %v; want %s", on, got, err, tt.want)
				}
				r.Close()
			}
		})
	}
}

// TestArgumentsRoomAllocs pins what an arguments object of three costs:
// one allocation, and a mapped one three more -- its aliases, the slice of
// them and what holds the slice -- rather than one for each alias and a map
// of the parameters' names.
func TestArgumentsRoomAllocs(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		src  string
		want float64
	}{
		{`function f() { 'use strict'; return arguments }`, 1},
		{`function f(a, b, c) { return arguments }`, 4},
	} {
		for _, on := range []bool{false, true} {
			SetTreeTier(on)
			r := New(Config{})
			v, err := r.Run(compileForTest(t, tt.src+`; for (let i = 0; i < 10; i++) f(1, 2, 3); f`))
			if err != nil {
				t.Fatal(err)
			}
			args := []Value{Int(1), Int(2), Int(3)}
			if n := testing.AllocsPerRun(100, func() {
				if _, err := r.Call(v, Undefined, args); err != nil {
					t.Fatal(err)
				}
			}); n != tt.want {
				t.Errorf("%s, tree %v: %v allocations a call, want %v", tt.src, on, n, tt.want)
			}
			r.Close()
		}
	}
}

// TestArgumentsCalleePerRealm pins that an unmapped arguments object's
// callee is its own realm's %ThrowTypeError%, once the runtime has made an
// arguments object and copies the rest from its layout: the layout is the
// runtime's, shared by its realms, and the function is the realm's.
func TestArgumentsCalleePerRealm(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	src := `(function () { "use strict"; return Object.getOwnPropertyDescriptor(arguments, "callee").get })()`
	local, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	other := r.NewRealm()
	var got [2]Value
	for i := range got {
		if got[i], err = r.RunIn(other, compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	if local.StrictEquals(got[0]) {
		t.Error("another realm's arguments object has this realm's ThrowTypeError")
	}
	if !got[0].StrictEquals(got[1]) {
		t.Error("one realm's arguments objects have different ThrowTypeError functions")
	}
	again, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if !local.StrictEquals(again) {
		t.Error("this realm's ThrowTypeError changed after another realm made an arguments object")
	}
}
