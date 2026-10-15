package vm

import (
	"maps"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// c ? a : b as a value, built as one node where a and b are expressions
// (logicalJumps, ternaryNode), answers as the interpreter and Node do: a
// plain test of each kind of value, a comparison the test's jump makes --
// of numbers, strings, mixed, NaN, null and undefined --, ternaries in a
// and in b, in an && and holding one, a getter in the branch taken only,
// and one that throws only where it is taken. A branch that calls stays
// blocks; every other function is one node and a return.
func TestTernaryExpressions(t *testing.T) {
	const src = `
		const r = []
		let log = ''
		const o = { get a() { log += 'a'; return 1 }, get b() { log += 'b'; return 2 }, get t() { log += 't'; throw new Error('thrown') }, vals: [0, 1, '', 'x', null, undefined, NaN, -0, 2n, 0n, {}, []] }
		function t1(o, i) { let _k = 0; return o.vals[i] ? 'T' : 'F' }
		function lt(x, y) { let _k = 0; return x < y ? x : y }
		function ge(x, y) { let _k = 0; return x >= y ? 'ge' : 'lt' }
		function seq(x, y) { let _k = 0; return x === y ? 1 : x !== y + 1 ? 2 : 3 }
		function nestThen(x, y) { let _k = 0; return x ? (y ? 'xy' : 'x') : 'none' }
		function nestElse(x, y) { let _k = 0; return x ? 'x' : y ? 'y' : 'none' }
		function inAnd(x, y, z) { let _k = 0; return x && (y ? z : -z) }
		function andIn(x, y, z) { let _k = 0; return x ? y && z : y || z }
		function gets(o, c) { let _k = 0; return c ? o.a : o.b }
		function throws(o, c) { let _k = 0; return c ? o.t : o.a }
		function call(o, c) { let _k = 0; return c ? Math.max(o.vals.length, 3) : o.vals.length }
		for (let round = 0; round < 3; round++) {
			const s = []
			for (let i = 0; i < o.vals.length; i++) s.push(t1(o, i))
			for (const [x, y] of [[1, 2], [2, 1], [3, 3], ['2', 10], ['a', 'b'], [NaN, 1], [1, NaN], [null, 0], [undefined, 0]]) s.push(String(lt(x, y)), ge(x, y), seq(x, y))
			for (let i = 0; i < 4; i++) s.push(nestThen(i & 1, i & 2), nestElse(i & 1, i & 2), inAnd(i & 1, i & 2, i + 1), String(andIn(i & 1, i & 2, i & 3)))
			log = ''
			s.push(gets(o, 1), gets(o, 0), log)
			log = ''
			s.push(throws(o, 0) + log)
			try { throws(o, 1) } catch (e) { s.push(e.message + log) }
			s.push(call(o, 1), call(o, 0))
			r.push(s.join(','))
		}
		r[0] + ' | ' + (r[0] === r[1] && r[1] === r[2])`
	const want = "F,T,F,T,F,F,F,F,T,F,T,T,1,lt,2,1,ge,3,3,ge,1,2,lt,2,a,lt,2,1,lt,2,NaN,lt,2,0,ge,2,0,lt,2,none,none,0,0,x,x,-2,0,none,y,0,2,xy,x,4,3,1,2,ab,1a,thrownat,12,12 | true"
	for _, c := range compileForTest(t, src).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil || c.Fn.Name == "" || c.Fn.Name == "call" || strings.HasPrefix(c.Fn.Name, "get ") {
			continue
		}
		if tr := buildTree(c.Fn); tr == nil || len(tr.blocks) != 2 {
			t.Errorf("%s is not one node and a return", c.Fn.Name)
		}
	}
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, want)
		}
	}
	SetTreeTier(true)
}

// logicalJumps finds an && or || and a ternary from their instructions,
// with those nested in them that end where they do, and lets one go whose
// y, or a, or b, or end another jump goes to -- which a compiled script
// seldom has, so the code is written out here.
func TestLogicalJumpsFound(t *testing.T) {
	var (
		local = func(k uint32) bytecode.Instr { return bytecode.Instr{Op: bytecode.OpGetLocal, A: k} }
		op    = func(o bytecode.Op, a uint32) bytecode.Instr { return bytecode.Instr{Op: o, A: a} }
		ret   = bytecode.Instr{Op: bytecode.OpReturn}
	)
	count := func(code []bytecode.Instr) []int {
		n := make([]int, len(code)+1)
		for _, in := range code {
			switch in.Op {
			case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse,
				bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep:
				n[in.A]++
			}
		}
		return n
	}
	for _, c := range []struct {
		name string
		code []bytecode.Instr
		want map[int]int
	}{
		// a && b
		{"and", []bytecode.Instr{local(0), op(bytecode.OpJumpIfFalseKeep, 3), local(1), ret}, map[int]int{1: 3}},
		// x ? a : y ? b : c, the inner one's end the outer's
		{"nested", []bytecode.Instr{local(0), op(bytecode.OpJumpIfFalse, 4), local(1), op(bytecode.OpJump, 9),
			local(2), op(bytecode.OpJumpIfFalse, 8), local(3), op(bytecode.OpJump, 9), local(4), ret}, map[int]int{1: 4, 5: 8}},
		// x && (y ? a : b), the ternary's end the &&'s
		{"in and", []bytecode.Instr{local(0), op(bytecode.OpJumpIfFalseKeep, 7), local(1), op(bytecode.OpJumpIfFalse, 6),
			local(2), op(bytecode.OpJump, 7), local(3), ret}, map[int]int{1: 7, 3: 6}},
		// a && b, with b a loop's head too, which the jump back goes to
		{"jumped into", []bytecode.Instr{local(0), op(bytecode.OpJumpIfFalseKeep, 3), local(1), op(bytecode.OpJump, 2)}, nil},
		// x ? a : b, with another jump to its end, which stays blocks, and
		// the && in its a, which a jump does not reach, stays one
		{"end jumped to", []bytecode.Instr{local(0), op(bytecode.OpJumpIfFalse, 6), local(1), op(bytecode.OpJumpIfFalseKeep, 5),
			local(2), op(bytecode.OpJump, 7), local(3), ret, op(bytecode.OpJump, 7)}, map[int]int{3: 5}},
	} {
		if got := logicalJumps(c.code, count(c.code), nil); !maps.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
