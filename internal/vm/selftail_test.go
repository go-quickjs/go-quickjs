package vm

import "testing"

// A function calling itself in tail position deeper than tailCallDepth is
// started again in its own frame (selfRestart, Runtime.treeRestart): its
// parameters rebound, its other locals undefined, its first block run
// again. Each case recurses past the depth, so that it is started again
// or, where it may not be, given frames. It answers as Node does -- a var
// and a let made anew at each start, parameters with defaults (which take
// their values from the call's arguments, and so are given a frame each),
// fewer arguments than parameters and more, a method calling itself on
// another object, a binding a closure keeps open (given a frame each, the
// closures seeing their own), two closures of one function (not each
// other's self), a ternary's tail call -- and a recursion a million deep
// runs in its frame. Node, which makes no tail call, is asked 5,000 deep.
// On the trees and in the interpreter, three rounds alike.
func TestSelfTailCalls(t *testing.T) {
	const src = `
		"use strict"
		const r = []
		function count(n, acc) { var seen; let t = 1; if (seen !== undefined) return 'stale'; seen = 1; if (n === 0) return acc + t; return count(n - 1, acc + 1) }
		function defaults(n, step = 2, acc = 0) { let _k = 0; if (n <= 0) return acc; return defaults(n - step, undefined, acc + n) }
		function few(n, a, b) { let _k = 0; if (n === 0) return String(a) + '/' + String(b); return few(n - 1, a) }
		function many(n, a) { let _k = 0; if (n === 0) return a; return many(n - 1, a + 1, 'extra', 'more') }
		class Node2 { constructor(v, next) { this.v = v; this.next = next } sum(acc) { let _k = 0; if (this.next === null) return acc + this.v; return this.next.sum(acc + this.v) } }
		function captured(n, fs) { let _k = 0; if (n === 0) return fs.map(f => f()).join(); fs.push(() => n); return captured(n - 1, fs) }
		function maker() { return function self(n, acc) { let _k = 0; if (n === 0) return acc; return self(n - 1, acc + 1) } }
		const s1 = maker(), s2 = maker()
		function cross(n, acc) { let _k = 0; if (n === 0) return acc; return (n % 2 ? s1 : s2)(n - 1, acc + 1) }
		function deep(n) { let _k = 0; if (n === 0) return 'done'; return deep(n - 1) }
		function tern(n, acc) { let _k = 0; return n === 0 ? acc : tern(n - 1, acc + 2) }
		for (let round = 0; round < 3; round++) {
			const s = []
			let list = null
			for (let i = 1; i <= 50; i++) list = new Node2(i, list)
			s.push(count(100, 0), defaults(200), few(100, 'a', 'b'), many(100, 0), list.sum(0), captured(100, []), cross(100, 0), s1(50, 0), deep(1e6), tern(1000, 0))
			r.push(s.join(' '))
		}
		r[0] + ' | ' + (r[0] === r[1] && r[1] === r[2])`
	const want = "101 10100 a/undefined 100 1275 100,99,98,97,96,95,94,93,92,91,90,89,88,87,86,85,84,83,82,81,80,79,78,77,76,75,74,73,72,71,70,69,68,67,66,65,64,63,62,61,60,59,58,57,56,55,54,53,52,51,50,49,48,47,46,45,44,43,42,41,40,39,38,37,36,35,34,33,32,31,30,29,28,27,26,25,24,23,22,21,20,19,18,17,16,15,14,13,12,11,10,9,8,7,6,5,4,3,2,1 100 50 done 2000 | true"
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got %s, want %s", tree, got, want)
		}
	}
	SetTreeTier(true)
}

// TestSelfTailRestarts counts the starts again selfRestart makes: a
// recursion past tailCallDepth through calls of the function itself is a
// loop in one frame, every call made once the script's frame and the
// function's are tailCallDepth deep started again,
// and none of a recursion that may not be (a binding a closure keeps open,
// another closure of the function, a sloppy function), nor in the
// interpreter, which leaves every such call to the trampoline.
func TestSelfTailRestarts(t *testing.T) {
	old := selfRestart
	defer func() { selfRestart = old }()
	n := 0
	selfRestart = func(r *Runtime, c *tctx, this Value, args []Value) bool {
		ok := old(r, c, this, args)
		if ok {
			n++
		}
		return ok
	}
	cases := []struct {
		src  string
		want int
	}{
		{`"use strict"; function deep(n) { let _k = 0; if (n === 0) return 'done'; return deep(n - 1) }; deep(1000)`, 1000 + 2 - tailCallDepth},
		{`"use strict"; function tern(n, acc) { let _k = 0; return n === 0 ? acc : tern(n - 1, acc + 2) }; tern(1000, 0)`, 1000 + 2 - tailCallDepth},
		{`"use strict"; function captured(n, fs) { let _k = 0; if (n === 0) return fs.length; fs.push(() => n); return captured(n - 1, fs) }; captured(1000, [])`, 0},
		{`"use strict"; function maker() { return function self(n) { let _k = 0; if (n === 0) return 0; return (n % 2 ? s1 : s2)(n - 1) } }; const s1 = maker(), s2 = maker(); s1(1000)`, 0},
		{`function sloppy(n) { let _k = 0; if (n === 0) return 0; return sloppy(n - 1) }; sloppy(1000)`, 0},
	}
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		for _, tc := range cases {
			n = 0
			r := New(Config{})
			_, err := r.Run(compileForTest(t, tc.src))
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if !tree {
				want = 0
			}
			if n != want {
				t.Errorf("tree tier %v: %s: %d starts again, want %d", tree, tc.src, n, want)
			}
		}
	}
	SetTreeTier(true)
}
