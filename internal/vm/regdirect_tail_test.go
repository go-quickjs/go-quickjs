package vm

import "testing"

// A built-in's callback with a call in tail position can use direct
// register entry, including below the depth where tail calls give up or
// replace their frame. Its entry must finish both kinds of completion.
func TestRegDirectTailCallbacks(t *testing.T) {
	t.Parallel()
	const src = `
		"use strict";
		function element(type, props, key) { return {type, props, key}; }
		function row(entry) { return element("row", {entry}, entry.id); }
		function variable(type) { return element(type, {length: arguments.length, next: arguments[1]}, arguments[2]); }
		function recurse(n, value) { return n === 0 ? value : recurse(n - 1, value + 1); }
		function boundTarget(add, value) { return value + add; }
		var bound = boundTarget.bind(null, 7);
		function toBound(value) { return bound(value); }
		function takesArgs() { return arguments.length + ":" + arguments[2]; }
		function toArgs(value) { return takesArgs(value, 2, 3); }
		function fail(value) { throw new RangeError("row " + value); }
		function toFail(value) { return fail(value); }
		function target(receiver, value) { return receiver.tag + value; }
		function maker() { return value => target(this, value); }
		var arrow = maker.call({tag: "arrow"});
		function under(n, callback, value) {
			if (n === 0) return [value].map(callback)[0];
			var result = under(n - 1, callback, value);
			return result;
		}
		var out = [];
		for (var round = 0; round < 3; round++) {
			var r = under(50, row, {id: 19}), a = under(50, variable, "item");
			out.push(r.type + ":" + r.props.entry.id + ":" + r.key);
			out.push(a.type + ":" + a.props.length + ":" + a.props.next + ":" + a.key.length);
			out.push(under(50, toBound, 5), under(50, toArgs, 5), under(50, arrow, 5));
			out.push([5000].map(n => recurse(n, 0))[0]);
			try { under(50, toFail, 5); } catch (e) { out.push(e.name + ":" + e.message); }
		}
		out.join("|")`
	const round = "row:19:19|item:3:0:1|12|3:3|arrow5|5000|RangeError:row 5"
	for _, cfg := range []Config{{}, {NodeQuirks: true}} {
		r := newTestRuntime(t, cfg)
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			r.Close()
			t.Fatal(err)
		}
		if got, want := v.String().Go(), round+"|"+round+"|"+round; got != want {
			t.Errorf("quirks %v: got %q, want %q", cfg.NodeQuirks, got, want)
		}
		for _, name := range []string{"row", "variable", "recurse", "toBound", "toArgs", "toFail", "arrow"} {
			fd := r.global.getOwn(r.atoms.intern(name)).value().Object().fn()
			if !fd.closure.fn.HasTailCall || fd.regCall == nil {
				t.Errorf("quirks %v: %s: tail calls %v, direct entry %v", cfg.NodeQuirks, name, fd.closure.fn.HasTailCall, fd.regCall != nil)
			}
		}
		if !cfg.NodeQuirks {
			callback := r.global.getOwn(r.atoms.intern("row")).value()
			ready := r.regCallbackOf(callback, Undefined)
			if ready == nil {
				t.Fatal("tail-calling row callback was not retained")
			}
			ready.end()
		}
		if r.frameDepth != 0 || r.tailLoopDepth != -1 || r.regRestart != nil {
			t.Errorf("unfinished tail completion: frames %d, loop %d, restart %v", r.frameDepth, r.tailLoopDepth, r.regRestart != nil)
		}
		r.Close()
	}
}

// A callback's tail replacement can use more registers than the original
// body. The retained entry must restore its own reservation before the
// built-in makes the next call, and release it when iteration ends.
func TestRegRetainedTailWindow(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	r.global.setOwnRaw(r.atoms.intern("probe"), Obj(r.newNativeFunc("probe", 1,
		func(rt *Runtime, _ Value, args []Value) (Value, error) {
			saved, depth := rt.stackTop, rt.frameDepth
			ready := rt.regCallbackOf(args[0], Undefined)
			if ready == nil {
				t.Fatal("tail callback has no retained entry")
			}
			reserved := rt.stackTop
			var sum float64
			for i := range 3 {
				v, err := ready.call([]Value{Int(i)})
				if err != nil {
					ready.end()
					return Undefined, err
				}
				sum += v.Number()
				if rt.stackTop != reserved || rt.frameDepth != depth {
					t.Errorf("call %d: stack top %d, want %d; frames %d, want %d", i, rt.stackTop, reserved, rt.frameDepth, depth)
				}
			}
			ready.end()
			if rt.stackTop != saved {
				t.Errorf("released stack top %d, want %d", rt.stackTop, saved)
			}
			return Float(sum), nil
		})), propWritable|propConfigurable)
	const src = `"use strict";
		function bigger(x) {
			var a=x+1, b=x+2, c=x+3, d=x+4, e=x+5, f=x+6,
			    g=x+7, h=x+8, i=x+9, j=x+10, k=x+11, l=x+12;
			return x+a+b+c+d+e+f+g+h+i+j+k+l;
		}
		function callback(x) { return bigger(x); }
		function under(n) {
			if (n === 0) { var result = probe(callback); return result; }
			return 0 + under(n - 1);
		}
		[0].map(callback);
		under(50)`
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.Number(), float64(273); got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

// tailChainCallbacks are callbacks with calls in tail position, called by
// built-ins reached through a chain of calls in tail position past
// tailCallDepth: each entry -- direct, retained, through call, apply,
// Reflect.apply, a bound function or Array.from -- finishes its callee's
// tail calls, a frame given in place or a call given up, while a chain
// regTailLoop owns at a depth stays the loop's.
var tailChainCallbacks = []struct{ src, want string }{
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} function f(n){ if(n===0) return [1,2].map(cb); return f(n-1);} f(5000).join()`, "2,3"},
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} function f(n){ if(n===0) return [1,2].map(cb).join(); return f(n-1);} f(5000)`, "2,3"},
	{`"use strict"; function g(a,b){return b-a} function cb(a,b){return g(a,b)} function f(n){ if(n===0) return [1,3,2].sort(cb); return f(n-1);} f(5000).join()`, "3,2,1"},
	{`"use strict"; function g(a,x){return a+x} function cb(a,x){return g(a,x)} function f(n){ if(n===0) return [1,3,2].reduce(cb, 10); return f(n-1);} ""+f(5000)`, "16"},
	{`"use strict"; function g(m){return m.toUpperCase()} function cb(m){return g(m)} function f(n){ if(n===0) return "abc".replace(/b/g, cb); return f(n-1);} f(5000)`, "aBc"},
	{`"use strict"; function g(k,v){return typeof v==="number"?v*2:v} function cb(k,v){return g(k,v)} function f(n){ if(n===0) return JSON.parse("[1,2]", cb); return f(n-1);} f(5000).join()`, "2,4"},
	{`"use strict"; function cb(x){return Math.max(x,1)} function f(n){ if(n===0) return [0,5].map(cb); return f(n-1);} f(5000).join()`, "1,5"},
	{`"use strict"; function h(x){return x*3} function g(x){return h(x)} function cb(x){return g(x)} function f(n){ if(n===0) return [1,2].map(cb); return f(n-1);} f(5000).join()`, "3,6"},
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} var b = cb.bind(null); function f(n){ if(n===0) return [1,2].map(b); return f(n-1);} f(5000).join()`, "2,3"},
	{`"use strict"; function g(x){return [x].map(cb2)[0]} function cb2(x){return x+100} function cb(x){return g(x)} function f(n){ if(n===0) return [1,2].map(cb); return f(n-1);} f(5000).join()`, "101,102"},
	{`"use strict"; function g(x){ if (x > 1) throw new TypeError("t"+x); return x} function cb(x){return g(x)} function f(n){ if(n===0) return [1,2].map(cb); return f(n-1);} try { f(5000) } catch (e) { e.message }`, "t2"},
	{`"use strict"; var d=0; function rec(x,k){ return k===0 ? x : rec(x+1,k-1)} function cb(x){return rec(x, 20000)} function f(n){ if(n===0) return [1,2].map(cb); return f(n-1);} f(5000).join()`, "20001,20002"},
	{`"use strict"; function* gen(){ yield 1 } function cb(x){return Array.from(gen())} function f(n){ if(n===0) return [1].map(cb)[0].join(); return f(n-1);} f(5000)`, "1"},
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} function f(n){ if(n===0) return cb.call(null, 1); return f(n-1);} ""+f(5000)`, "2"},
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} function f(n){ if(n===0) return cb.apply(null, [1]); return f(n-1);} ""+f(5000)`, "2"},
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} function f(n){ if(n===0) return Reflect.apply(cb, null, [1]); return f(n-1);} ""+f(5000)`, "2"},
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} var b=cb.bind(null); function f(n){ if(n===0) return b(1); return f(n-1);} ""+f(5000)`, "2"},
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} function f(n){ if(n===0) return [1,2].map.call([1,2], cb).join(); return f(n-1);} f(5000)`, "2,3"},
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} function f(n){ if(n===0) return Array.prototype.map.call([1,2], cb); return f(n-1);} f(5000).join()`, "2,3"},
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} function f(n){ if(n===0) return Array.from([1,2], cb); return f(n-1);} f(5000).join()`, "2,3"},
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} function f(n){ if(n===0) return [1,2].map(cb.bind(null)); return f(n-1);} f(5000).join()`, "2,3"},
	{`"use strict"; function g(a,b){return a+b} function cb(a,b){return g(a,b)} function f(n){ if(n===0) return [1,2,3].reduce(cb); return f(n-1);} ""+f(5000)`, "6"},
	{`"use strict"; function g(x){return x>1} function cb(x){return g(x)} function f(n){ if(n===0) return [1,2,3].filter(cb).join()+[1,2,3].findIndex(cb)+[1,2].some(cb)+[1,2].every(cb); return f(n-1);} f(5000)`, "2,31truefalse"},
	{`"use strict"; function g(x){return x+1} function cb(x){return g(x)} function f(n){ if(n===0) return new Map([[1,2]]).forEach(cb) === undefined ? "u" : "d"; return f(n-1);} f(5000)`, "u"},
}

func TestRegTailCallbacksInTailChains(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{{}, {NodeQuirks: true}} {
		for i, c := range tailChainCallbacks {
			r := newTestRuntime(t, cfg)
			p := compileForTest(t, c.src)
			for round := range 3 {
				v, err := r.Run(p)
				if err != nil || v.String().Go() != c.want {
					t.Errorf("quirks %v, case %d, round %d: got %v, %v; want %s", cfg.NodeQuirks, i, round, v, err, c.want)
				}
			}
			if r.frameDepth != 0 || r.tailLoopDepth != -1 || r.regRestart != nil {
				t.Errorf("quirks %v, case %d: unfinished tail completion: frames %d, loop %d", cfg.NodeQuirks, i, r.frameDepth, r.tailLoopDepth)
			}
			r.Close()
		}
	}
}

func BenchmarkRegTailCallback(b *testing.B) {
	for _, tc := range []struct{ name, body string }{
		{"tail", `return element("row", {entry}, entry.id);`},
		{"ordinary", `var value = element("row", {entry}, entry.id); return value;`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := newTestRuntime(b, Config{})
			defer r.Close()
			setup := compileForTest(b, `"use strict";
				function element(type, props, key) { return {type, props, key}; }
				function callback(entry) { `+tc.body+` }
				var entries = []; for (var i = 0; i < 100; i++) entries.push({id: i});`)
			if _, err := r.Run(setup); err != nil {
				b.Fatal(err)
			}
			program := compileForTest(b, `entries.map(callback)`)
			for range 20 {
				if _, err := r.Run(program); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := r.Run(program); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
