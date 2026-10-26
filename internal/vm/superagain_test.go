package vm

import "testing"

// superAgainSrc calls super() a second time in derived constructors. The
// parent's [[Construct]] with new.target makes an object of its own and
// runs the parent on it -- its constructor's body, its public and private
// fields, the object it returns, a derived parent's own super(), Array's
// construction -- and only then does binding this throw a ReferenceError
// (BindThisValue): the instance the first super() bound is unchanged. A
// parent that throws on the second construction throws its own error
// instead. The answers are Node's, its messages aside (each is the
// engine's own): the engine had run the parent's body and initializer
// again on the first instance, so that this.x became 2, and a private
// field's second addition threw a TypeError.
const superAgainSrc = `'use strict'
	const log = []
	let k = 0
	function err(e) { return e.constructor.name }
	class A {
		constructor(x) {
			this.x = x
			log.push('A' + x + ':' + new.target.name + ':' + (Object.getPrototypeOf(this) === new.target.prototype) + ':' + Object.keys(this).join(''))
		}
	}
	class B extends A {
		constructor() {
			super(1)
			const first = this
			try { super(2) } catch (e) { log.push(err(e)) }
			log.push('B x=' + this.x + ' same=' + (this === first) + ' keys=' + Object.keys(this).join(''))
		}
	}
	class F {
		#p = log.push('F#p')
		n = (log.push('F.n'), ++k)
		get p() { return this.#p }
	}
	class G extends F {
		constructor() {
			super()
			const n = this.n
			try { super() } catch (e) { log.push(err(e)) }
			log.push('G n=' + (this.n === n) + ' p=' + (this.p > 0))
		}
	}
	class R { constructor() { log.push('R'); return { tag: ++k } } }
	class S extends R {
		constructor() {
			super()
			const t = this.tag
			try { super() } catch (e) { log.push(err(e)) }
			log.push('S same=' + (this.tag === t))
		}
	}
	class T extends A {
		constructor() {
			const f = () => super(5)
			f()
			try { f() } catch (e) { log.push(err(e)) }
			log.push('T x=' + this.x)
		}
	}
	class M extends A {}
	class N extends M {
		constructor() {
			super(3)
			try { super(4) } catch (e) { log.push(err(e)) }
			log.push('N x=' + this.x)
		}
	}
	class Arr extends Array {
		constructor() {
			super(7, 8)
			try { super(9) } catch (e) { log.push(err(e)) }
			log.push('Arr ' + this.length + ',' + this[0] + ',' + (this instanceof Arr))
		}
	}
	class Th extends A {
		constructor() {
			super(1)
			try { super('throw') } catch (e) { log.push(err(e)) }
		}
	}
	class Bad { constructor(x) { if (x === 'throw') throw new TypeError('in parent'); this.y = x } }
	class W extends Bad {
		constructor() {
			super(1)
			try { super('throw') } catch (e) { log.push(err(e) + ' ' + e.message) }
			log.push('W y=' + this.y)
		}
	}
	function run() {
		log.length = 0
		new B(); new G(); new S(); new T(); new N(); new Arr(); new W()
		return log.join(' | ')
	}
	const first = run()
	let same = true
	for (let i = 0; i < 300; i++) if (run() !== first) same = false
	first + ' || same ' + same`

const superAgainWant = "A1:B:true:x | A2:B:true:x | ReferenceError | B x=1 same=true keys=x | F#p | F.n | F#p | F.n | ReferenceError | G n=true p=true | R | R | ReferenceError | S same=true | A5:T:true:x | A5:T:true:x | ReferenceError | T x=5 | A3:N:true:x | A4:N:true:x | ReferenceError | N x=3 | ReferenceError | Arr 2,7,true | TypeError in parent | W y=1 || same true"

func TestSecondSuperMakesItsOwnObject(t *testing.T) {
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, superAgainSrc))
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != superAgainWant {
			t.Errorf("tree %v: got %s\nwant %s", tree, got, superAgainWant)
		}
	}
	// The error is the engine's, its message the one a super() past the
	// first has always had.
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `'use strict'
		class A { constructor() { this.n = 1 } }
		class B extends A { constructor() { super(); try { super() } catch (e) { this.m = e.constructor.name + ': ' + e.message } } }
		const b = new B(); b.m + ' ' + b.n`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "ReferenceError: super() has already been called 1"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
