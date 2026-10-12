package vm

import "testing"

// TestGeneratorValues pins generators and async functions with each count
// of arguments and locals around the sizes newGenerator makes in one
// allocation with the object, in both tiers: what the parameters and the
// arguments object see, locals captured by closures across suspensions,
// and what genRare keeps: an async arrow's new.target and its share of a
// derived constructor's this, and an async generator's queue. A direct
// eval's vars, the rest of it, are TestGeneratorEvalVars's.
func TestGeneratorValues(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
		async           bool
	}{
		{name: "none", src: `function* f() { yield arguments.length } [...f()].join()`, want: "0"},
		{name: "two", src: `function* f(a) { var b = a + 1; yield () => a + b; yield arguments[0] } var g = f(1), h = g.next().value; g.next().value + ',' + h()`, want: "1,3"},
		{name: "three", src: `function* f(a, b) { var c = 3; for (var i = 0; i < 2; i++) yield a + b + c + i } [...f(1, 2)].join()`, want: "6,7"},
		{name: "four", src: `function* f(a, b) { yield a; yield b; yield arguments.length } [...f(1, 2)].join()`, want: "1,2,2"},
		{name: "five", src: `function* f(a, b, c) { var d = a + b; var e = yield d; yield e + c + arguments[2] } var g = f(1, 2, 3); g.next(); g.next(10).value`, want: "16"},
		{name: "extraArgs", src: `function* f(a) { var x = 1, y = 2; yield [a, x, y, arguments.length, arguments[3]].join() } f(1, 2, 3, 4, 5).next().value`, want: "1,1,2,5,4"},
		{name: "eight", src: `function* f(a, b, c, d) { var e = 5, g = 6, h = 7, k = 8; yield a + b + c + d + e + g + h + k } f(1, 2, 3, 4).next().value`, want: "36"},
		{name: "nine", src: `function* f(a, b, c, d) { var e = 5, g = 6, h = 7, k = 8, l = 9; yield a + b + c + d + e + g + h + k + l; yield arguments.length } [...f(1, 2, 3, 4)].join()`, want: "45,4"},
		{name: "many", src: `function* f(...r) { var s = 0; for (var i = 0; i < r.length; i++) { s += r[i]; yield s } } [...f(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)].pop()`, want: "55"},
		{name: "rest", src: `function* f(a, ...r) { yield r.length; yield a } [...f(1, 2, 3)].join()`, want: "2,1"},
		{name: "defaults", src: `function* f(a, b = a * 2) { yield a + b } f(3).next().value + ',' + f(3, 1).next().value`, want: "9,4"},
		{name: "asyncArrowNewTarget", async: true, src: `function F() { this.p = (async () => { await 0; return typeof new.target })() } var p = new F().p`, want: "function"},
		{name: "asyncArrowThis", async: true, src: `class A {} class B extends A { constructor() { var f = async () => { await 0; return this.x }; super(); this.x = 4; this.p = f() } } var p = new B().p`, want: "4"},
		{name: "asyncArrowSuperLater", async: true, src: `class A {} class B extends A { constructor() { var f = async () => { await 0; return this.y }; var q = f(); super(); this.y = 5; this.p = q } } var p = new B().p`, want: "5"},
		{name: "asyncGeneratorQueue", async: true, src: `async function* f() { yield 1; yield 2 } var g = f(); var p = Promise.all([g.next(), g.next(), g.next(), g.return(9)]).then(r => JSON.stringify(r))`, want: `[{"value":1,"done":false},{"value":2,"done":false},{"done":true},{"value":9,"done":true}]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				src := tt.src
				if tt.async {
					src = "var out = 'pending'; " + src + "; Promise.resolve(p).then(v => out = String(v), e => out = 'E ' + e)"
				}
				r := New(Config{})
				v, err := r.Run(compileForTest(t, src))
				if err == nil && tt.async {
					if err = r.runMicrotasks(); err == nil {
						v, err = r.Run(compileForTest(t, "out"))
					}
				}
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

// TestGeneratorOneAllocation pins that a generator with few arguments and
// locals is made in one allocation: its object, its state, and its
// values.
func TestGeneratorOneAllocation(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	for _, src := range []string{
		`(function* (a, b) { yield a; yield b })`,
		`(function* (n) { for (var i = 0; i < n; i++) yield i })`,
		`(function* (a, b, c) { var d, e; yield a })`,
	} {
		fn, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		args := []Value{Int(1), Int(2)}
		if n := testing.AllocsPerRun(100, func() {
			if _, err := r.call(fn, Undefined, args); err != nil {
				t.Fatal(err)
			}
		}); n != 1 {
			t.Errorf("%s: %v allocations, want 1", src, n)
		}
	}
}
