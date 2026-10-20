package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Conversions, destructuring, spreads, computed members and private class
// members in the tree tier.
//
// Each instruction here is a node that does what the interpreter's does,
// calling the helper it calls, or, where its work is written out in the
// interpreter's loop, doing that work with the same errors. Without them a
// function with a template literal, an object destructuring pattern, a call
// of o[k](), a rest parameter, a spread or a private member ran in the
// interpreter.
//
// getGlobalOpt and the helpers after it copy work executeAt does in its own
// cases. executeAt is kept as it is: it is the hot path, whose code and
// size decide how much of it the processor's op cache holds, and moving
// those cases out of it slowed loops that run none of them. TestTreeOps
// runs each on both tiers.
//
// Its functions are variables, each assigned a closure in init, rather
// than functions or methods: Go lays out all of a package's functions
// before any of its closures, so code added here as functions would sit
// between the interpreter's hot functions and the tree nodes' hot
// closures, and move one against the other, where a closure is laid out
// after every closure already there (see AGENTS.md).

var (
	// treeBuildsOps reports whether the tier builds op, one of those opsOp
	// builds, or one treeBuildsSuspend names.
	treeBuildsOps func(op bytecode.Op) bool

	// opsOp builds the instructions treeBuildsOps names. An instruction that
	// works on what lies beneath its operands -- an object a member is defined
	// on, an array a spread adds to -- has the stack spilled first, and reads
	// them from their slots.
	opsOp func(b *tbuilder, pc int, in bytecode.Instr) (int, bool)

	// memberOp builds the instructions of super references, of the in and
	// delete operators, of typeof's read of a global, of the arguments object,
	// of the ** operator, and of the members a class or an object literal
	// defines. It reports -1 where in is none of these, for privateOp.
	memberOp func(b *tbuilder, pc int, in bytecode.Instr, a, k uint32) (int, bool)

	// getGlobalOpt is get_global_opt, typeof's read of a global: a name
	// declared nowhere is undefined rather than a ReferenceError. A lexical
	// binding in its dead zone is declared, though, so that one still throws.
	getGlobalOpt func(r *Runtime, f *frame, cl *closure, name Atom) (Value, error)

	// inOperator is key in obj.
	inOperator func(r *Runtime, key, obj Value) (Value, error)

	// deleteIndexed is delete obj[key], and delete obj.name.
	deleteIndexed func(r *Runtime, obj, key Value, strict bool) (Value, error)

	// toObjectValue is ToObject as a value, the base a delete of a
	// primitive's property deletes from (executeAt's delete_prop,
	// deleteIndexed): a closure here, not a function, so that it moves no
	// code of the interpreter's.
	toObjectValue func(r *Runtime, v Value) (Value, error)

	// deleteVar is delete name. Deleting a binding only succeeds for a
	// configurable global property, which is why a var declaration cannot be
	// deleted.
	deleteVar func(r *Runtime, f *frame, cl *closure, name Atom) (Value, error)

	// superIndexGet is super[key], read from base, which super_base found.
	superIndexGet func(r *Runtime, f *frame, base, rawKey Value) (Value, error)

	// superIndexSet is super[key] = val, on base, which super_base found.
	superIndexSet func(r *Runtime, f *frame, base, rawKey, val Value, strict bool) error

	// concatParts is concat, as executeAt's case does it: a template
	// literal's parts joined. Every part is converted before any of them is
	// joined: a toString may run user code, and when it runs is fixed. A
	// number is left as it is, having no conversion anything can observe and
	// no need of a string of its own. The parts are the operand stack's,
	// which are converted in place.
	concatParts func(r *Runtime, parts []Value) (Value, error)

	// privateOp builds the instructions of private class members, and hands
	// suspendOp the rest. A member's key is the private name the class made,
	// which a local or an upvalue of the function holds (see privateKey).
	privateOp func(b *tbuilder, pc int, in bytecode.Instr, a, k uint32) (int, bool)
)

func init() {
	treeBuildsOps = func(op bytecode.Op) bool {
		switch op {
		case bytecode.OpCheckCoercible, bytecode.OpToString, bytecode.OpToObject, bytecode.OpToNumber,
			bytecode.OpGetIndexThis, bytecode.OpRestParam, bytecode.OpGetArguments,
			bytecode.OpDefineIndex, bytecode.OpCopyDataProps, bytecode.OpArraySpread, bytecode.OpSpreadIter,
			bytecode.OpSetFuncName, bytecode.OpGetPropUnder, bytecode.OpGetIndexUnder, bytecode.OpNipUnder,
			bytecode.OpArrayPush, bytecode.OpConcat, bytecode.OpObjectRest, bytecode.OpCallSpread, bytecode.OpNewSpread,
			bytecode.OpSuperCall, bytecode.OpGetSuperProp, bytecode.OpSetSuperProp,
			bytecode.OpSuperBase, bytecode.OpGetSuperIndex, bytecode.OpSetSuperIndex,
			bytecode.OpIn, bytecode.OpDeleteProp, bytecode.OpDeleteVar, bytecode.OpGetGlobalOpt,
			bytecode.OpApplyArguments, bytecode.OpLazyArguments, bytecode.OpPushCallee, bytecode.OpPos, bytecode.OpPow,
			bytecode.OpNewClass, bytecode.OpSetProtoOf, bytecode.OpSetFieldInit, bytecode.OpSetName,
			bytecode.OpDefineMethod, bytecode.OpDefineGetter, bytecode.OpDefineSetter,
			bytecode.OpDefineGetterIndex, bytecode.OpDefineSetterIndex, bytecode.OpTemplateObject,
			bytecode.OpPrivateName, bytecode.OpGetPrivate, bytecode.OpSetPrivate,
			bytecode.OpDefinePrivate, bytecode.OpDefinePrivateMethod,
			bytecode.OpDefinePrivateGetter, bytecode.OpDefinePrivateSetter, bytecode.OpPrivateIn,
			bytecode.OpNewPrivateMethods, bytecode.OpAddPrivateMethod, bytecode.OpAddPrivateGetter,
			bytecode.OpAddPrivateSetter, bytecode.OpInstallPrivateMethods:
			return true
		}
		return treeBuildsSuspend(op)
	}

	opsOp = func(b *tbuilder, pc int, in bytecode.Instr) (int, bool) {
		a, k := in.A, in.B
		switch in.Op {
		case bytecode.OpCheckCoercible:
			// A destructuring pattern's source, which stays for the pattern.
			if !b.spill() || b.depth() == 0 {
				return 0, false
			}
			at := b.depth() - 1
			return 0, b.stmt(func(c *tctx) {
				if v := c.stack[at]; v.IsNullish() {
					c.at(pc)
					c.throw(c.r.throwTypeError("cannot destructure %s", c.r.describe(v)))
				}
			})
		case bytecode.OpToString:
			v := b.pop().tree()
			b.push(func(c *tctx) Value {
				x := v(c)
				if x.IsString() {
					return x
				}
				c.at(pc)
				s, err := c.r.toString(x)
				if err != nil {
					c.throw(err)
				}
				return Str(s)
			})
		case bytecode.OpToObject:
			v := b.pop().tree()
			b.push(func(c *tctx) Value {
				x := v(c)
				c.at(pc)
				o, err := c.r.toObject(x)
				if err != nil {
					c.throw(err)
				}
				return Obj(o)
			})
		case bytecode.OpToNumber, bytecode.OpPos:
			v := b.pop().tree()
			b.push(func(c *tctx) Value {
				x := v(c)
				c.at(pc)
				n, err := c.r.toNumber(x)
				if err != nil {
					c.throw(err)
				}
				return Float(n)
			})
		case bytecode.OpGetIndexThis:
			// o[k](): the receiver stays beneath the method, which takes the
			// key's place.
			if !b.spill() || b.depth() < 2 {
				return 0, false
			}
			d := b.depth()
			b.pop()
			b.push(func(c *tctx) Value {
				c.at(pc)
				v, err := c.r.getIndexed(c.stack[d-2], c.stack[d-1])
				if err != nil {
					c.throw(err)
				}
				return v
			})
		case bytecode.OpRestParam:
			start := int(a)
			b.push(func(c *tctx) Value {
				var rest []Value
				if start < len(c.f.args) {
					rest = c.f.args[start:]
				}
				return Obj(c.r.newArrayFrom(rest))
			})
		case bytecode.OpGetArguments:
			b.push(func(c *tctx) Value { return Obj(c.r.newArgumentsObject(c.f)) })
		case bytecode.OpDefineIndex:
			// { [k]: v }: the object stays.
			if !b.spill() || b.depth() < 3 {
				return 0, false
			}
			d := b.depth()
			b.pop()
			b.pop()
			flags := memberFlags(a)
			return 0, b.stmt(func(c *tctx) {
				c.at(pc)
				key, err := c.r.toPropertyKey(c.stack[d-2])
				if err != nil {
					c.throw(err)
				}
				if obj := c.stack[d-3]; obj.IsObject() {
					// A checked define: a member named after a property the
					// object will not part with is a TypeError.
					if err := c.r.createDataProperty(obj.Object(), key, c.stack[d-1], flags); err != nil {
						c.throw(err)
					}
				}
			})
		case bytecode.OpCopyDataProps, bytecode.OpArraySpread, bytecode.OpSpreadIter:
			// { ...src } and [...src]: the target stays.
			if !b.spill() || b.depth() < 2 {
				return 0, false
			}
			d := b.depth()
			b.pop()
			op := in.Op
			return 0, b.stmt(func(c *tctx) {
				target, src := c.stack[d-2], c.stack[d-1]
				c.at(pc)
				switch op {
				case bytecode.OpCopyDataProps:
					if target.IsObject() && !src.IsNullish() {
						if err := c.r.copyDataProps(target.Object(), src); err != nil {
							c.throw(err)
						}
					}
				case bytecode.OpArraySpread:
					if target.IsObject() {
						if err := c.r.spreadInto(target.Object(), src); err != nil {
							c.throw(err)
						}
					}
				default:
					vals, err := c.r.spreadToStack(src)
					if err != nil {
						c.throw(err)
					}
					if target.IsObject() {
						target.Object().extOf().elems = append(target.Object().elemsOf(), vals...)
					}
				}
			})
		case bytecode.OpSetFuncName:
			// The function on top is named after the key beneath it.
			if !b.spill() || b.depth() < 2 {
				return 0, false
			}
			d := b.depth()
			return 0, b.stmt(func(c *tctx) {
				if fnVal := c.stack[d-1]; fnVal.IsObject() {
					if fd := fnVal.Object().fn(); fd != nil {
						fd.name = functionNameFromKey(c.stack[d-2], a)
					}
				}
			})
		case bytecode.OpGetPropUnder, bytecode.OpGetIndexUnder:
			// A destructuring pattern's reads of its source, which sits beneath
			// a target's reference: B slots down for a name, A for a key, the
			// key on top of it.
			if !b.spill() {
				return 0, false
			}
			d := b.depth()
			if in.Op == bytecode.OpGetPropUnder {
				at := d - 1 - int(k)
				if at < 0 {
					return 0, false
				}
				b.push(func(c *tctx) Value {
					c.at(pc)
					v, err := c.r.getValueProp(c.stack[at], c.names[a])
					if err != nil {
						c.throw(err)
					}
					return v
				})
				break
			}
			at := d - 2 - int(a)
			if at < 0 {
				return 0, false
			}
			b.push(func(c *tctx) Value {
				c.at(pc)
				v, err := c.r.getIndexed(c.stack[at], c.stack[at+1])
				if err != nil {
					c.throw(err)
				}
				return v
			})
		case bytecode.OpNipUnder:
			// The top value stays; the A beneath it go.
			n := int(a)
			if !b.spill() || b.depth() < n+1 {
				return 0, false
			}
			d := b.depth()
			for range n + 1 {
				b.pop()
			}
			if n > 0 {
				if !b.stmt(func(c *tctx) { c.stack[d-1-n] = c.stack[d-1] }) {
					return 0, false
				}
			}
			b.pushSlot(d - 1 - n)
		case bytecode.OpArrayPush:
			val := b.pop().tree()
			if !b.spill() || b.depth() == 0 {
				return 0, false
			}
			at := b.depth() - 1
			return 0, b.stmt(func(c *tctx) {
				v := val(c)
				if arr := c.stack[at]; arr.IsObject() {
					o := arr.Object()
					o.extOf().elems = append(o.elemsOf(), v)
				}
			})
		case bytecode.OpConcat:
			// A template literal's parts, joined once.
			n := int(a)
			if !b.spill() || b.depth() < n {
				return 0, false
			}
			d := b.depth()
			for range n {
				b.pop()
			}
			b.push(func(c *tctx) Value {
				c.at(pc)
				v, err := concatParts(c.r, c.stack[d-n:d])
				if err != nil {
					c.throw(err)
				}
				return v
			})
		case bytecode.OpObjectRest:
			// { a, ...rest }: the keys the pattern named sit above its source.
			n := int(a)
			if !b.spill() || b.depth() < n+1 {
				return 0, false
			}
			d := b.depth()
			for range n + 1 {
				b.pop()
			}
			b.push(func(c *tctx) Value {
				c.at(pc)
				v, err := c.r.objectRest(c.stack[d-n-1], c.stack[d-n:d])
				if err != nil {
					c.throw(err)
				}
				return v
			})
		case bytecode.OpCallSpread, bytecode.OpNewSpread:
			// f(...a) and new F(...a), whose arguments are gathered into an
			// array; a call has a receiver beneath the callee, undefined for a
			// plain call.
			n := 2
			if in.Op == bytecode.OpCallSpread {
				n = 3
			}
			if !b.spill() || b.depth() < n {
				return 0, false
			}
			d := b.depth()
			for range n {
				b.pop()
			}
			op := in.Op
			b.push(func(c *tctx) Value {
				var args []Value
				if a := c.stack[d-1]; a.IsObject() {
					args = a.Object().elemsOf()
				}
				c.at(pc)
				var v Value
				var err error
				if op == bytecode.OpNewSpread {
					v, err = c.r.construct(c.stack[d-2], args)
				} else {
					v, err = c.r.call(c.stack[d-2], c.stack[d-3], args)
				}
				if err != nil {
					c.throw(err)
				}
				return v
			})
		case bytecode.OpSuperCall:
			// super(...): its value is the object it bound.
			n := int(a)
			switch k {
			case bytecode.SuperSpread:
				// The spread form gathered the arguments into an array.
				n = 1
			case bytecode.SuperForward:
				// The implicit constructor's passes on its own.
				n = 0
			}
			if !b.spill() || b.depth() < n {
				return 0, false
			}
			d := b.depth()
			for range n {
				b.pop()
			}
			spread, forward := k == bytecode.SuperSpread, k == bytecode.SuperForward
			b.push(func(c *tctx) Value {
				var args []Value
				if forward {
					args = c.f.args
				} else if spread {
					if a := c.stack[d-1]; a.IsObject() {
						args = a.Object().elemsOf()
					}
				} else {
					args = append([]Value(nil), c.stack[d-n:d]...)
				}
				c.at(pc)
				if err := c.r.superCall(c.f, args); err != nil {
					c.throw(err)
				}
				return c.f.this
			})
		case bytecode.OpGetSuperProp:
			b.push(func(c *tctx) Value {
				c.at(pc)
				v, err := c.r.superGet(c.f, c.names[a])
				if err != nil {
					c.throw(err)
				}
				return v
			})
		case bytecode.OpSetSuperProp:
			val := b.pop().tree()
			return 0, b.stmt(func(c *tctx) {
				v := val(c)
				c.at(pc)
				if err := c.r.superSet(c.f, c.names[a], v, c.cl.fn.Strict); err != nil {
					c.throw(err)
				}
			})
		default:
			if n, ok := memberOp(b, pc, in, a, k); ok || n >= 0 {
				return n, ok
			}
			return privateOp(b, pc, in, a, k)
		}
		return 0, true
	}

	memberOp = func(b *tbuilder, pc int, in bytecode.Instr, a, k uint32) (int, bool) {
		strict := b.fn.Strict
		switch in.Op {
		case bytecode.OpSuperBase:
			b.push(func(c *tctx) Value {
				c.at(pc)
				base, _, err := c.r.superRef(c.f)
				if err != nil {
					c.throw(err)
				}
				if base == nil {
					// No prototype to reach: the reference has a null base, which
					// the read or the write reports once it gets there.
					return Null
				}
				return Obj(base)
			})
		case bytecode.OpGetSuperIndex, bytecode.OpSetSuperIndex:
			n := 2
			if in.Op == bytecode.OpSetSuperIndex {
				n = 3
			}
			if !b.spill() || b.depth() < n {
				return 0, false
			}
			d := b.depth()
			for range n {
				b.pop()
			}
			if in.Op == bytecode.OpSetSuperIndex {
				return 0, b.stmt(func(c *tctx) {
					c.at(pc)
					if err := superIndexSet(c.r, c.f, c.stack[d-3], c.stack[d-2], c.stack[d-1], strict); err != nil {
						c.throw(err)
					}
				})
			}
			b.push(func(c *tctx) Value {
				c.at(pc)
				v, err := superIndexGet(c.r, c.f, c.stack[d-2], c.stack[d-1])
				if err != nil {
					c.throw(err)
				}
				return v
			})
		case bytecode.OpIn, bytecode.OpDeleteProp, bytecode.OpPow:
			y, x := b.pop().tree(), b.pop().tree()
			op := in.Op
			b.push(func(c *tctx) Value {
				l := x(c)
				r := y(c)
				if op == bytecode.OpPow && l.IsNumber() && r.IsNumber() {
					return Float(jsPow(l.Number(), r.Number()))
				}
				c.at(pc)
				var v Value
				var err error
				switch op {
				case bytecode.OpIn:
					v, err = inOperator(c.r, l, r)
				case bytecode.OpDeleteProp:
					v, err = deleteIndexed(c.r, l, r, strict)
				default:
					v, err = c.r.arith(op, l, r)
				}
				if err != nil {
					c.throw(err)
				}
				if jitBuilt && op == bytecode.OpPow && l.IsBigInt() && r.IsBigInt() {
					// A power of BigInts, for the JIT (treeBigSeen).
					if l.isShortBig() && r.isShortBig() && v.isShortBig() {
						treeBigOf(c, pc, l, r, v)
					} else {
						treeBigInt(c, pc, true)
					}
				}
				return v
			})
		case bytecode.OpDeleteVar, bytecode.OpGetGlobalOpt:
			op := in.Op
			b.push(func(c *tctx) Value {
				c.at(pc)
				var v Value
				var err error
				if op == bytecode.OpDeleteVar {
					v, err = deleteVar(c.r, c.f, c.cl, c.names[a])
				} else {
					v, err = getGlobalOpt(c.r, c.f, c.cl, c.names[a])
				}
				if err != nil {
					c.throw(err)
				}
				return v
			})
		case bytecode.OpApplyArguments:
			// f.apply(x, arguments), with the arguments object never made.
			this, apply, target := b.pop().tree(), b.pop().tree(), b.pop().tree()
			b.push(func(c *tctx) Value {
				t := target(c)
				ap := apply(c)
				th := this(c)
				c.at(pc)
				v, err := c.r.applyArguments(c.f, a, t, ap, th)
				if err != nil {
					c.throw(err)
				}
				return v
			})
		case bytecode.OpLazyArguments:
			// The arguments object, made the first time it is asked for and
			// kept in the local only this reads.
			b.push(func(c *tctx) Value {
				if !c.locals[a].IsObject() {
					c.locals[a] = Obj(c.r.newArgumentsObject(c.f))
				}
				return c.locals[a]
			})
		case bytecode.OpPushCallee:
			b.push(func(c *tctx) Value {
				if c.f.callee == nil {
					return Undefined
				}
				return Obj(c.f.callee)
			})
		case bytecode.OpTemplateObject:
			idx := int(a)
			b.push(func(c *tctx) Value { return Obj(c.r.templateObject(c.cl.fn, idx)) })
		case bytecode.OpNewClass, bytecode.OpSetProtoOf, bytecode.OpSetFieldInit, bytecode.OpDefineMethod,
			bytecode.OpDefineGetter, bytecode.OpDefineSetter:
			// What is defined, or linked, on the object beneath it, which stays.
			val := b.pop().tree()
			if !b.spill() || b.depth() == 0 {
				return 0, false
			}
			at, op := b.depth()-1, in.Op
			return 0, b.stmt(func(c *tctx) {
				v := val(c)
				obj := c.stack[at]
				switch op {
				case bytecode.OpNewClass:
					c.at(pc)
					if err := c.r.linkClass(obj, v); err != nil {
						c.throw(err)
					}
				case bytecode.OpSetProtoOf:
					if obj.IsObject() {
						switch {
						case v.IsObject():
							obj.Object().proto = v.Object()
						case v.IsNull():
							obj.Object().proto = nil
						}
					}
				case bytecode.OpSetFieldInit:
					if obj.IsObject() && v.IsObject() {
						if fd := obj.Object().fn(); fd != nil {
							fd.fieldInit = v.Object()
						}
					}
				case bytecode.OpDefineMethod:
					// A class method is not enumerable, unlike an object
					// literal's.
					if obj.IsObject() {
						key := c.names[a]
						// A static member named after a function's synthesized
						// length or name replaces it in place, so it has to
						// exist first.
						c.r.materializeFunctionProp(obj.Object(), key)
						obj.Object().setOwnRaw(key, v, propWritable|propConfigurable)
					}
				default:
					if obj.IsObject() && v.IsObject() {
						c.r.defineHalfAccessor(obj.Object(), c.names[a], v.Object(),
							op == bytecode.OpDefineGetter, memberFlags(k))
					}
				}
			})
		case bytecode.OpDefineGetterIndex, bytecode.OpDefineSetterIndex:
			// { get [k]() {} }: the object stays.
			if !b.spill() || b.depth() < 3 {
				return 0, false
			}
			d := b.depth()
			b.pop()
			b.pop()
			getter, flags := in.Op == bytecode.OpDefineGetterIndex, memberFlags(a)
			return 0, b.stmt(func(c *tctx) {
				c.at(pc)
				key, err := c.r.toPropertyKey(c.stack[d-2])
				if err != nil {
					c.throw(err)
				}
				if obj, fn := c.stack[d-3], c.stack[d-1]; obj.IsObject() && fn.IsObject() {
					if err := c.r.defineHalfAccessorChecked(obj.Object(), key, fn.Object(), getter, flags); err != nil {
						c.throw(err)
					}
				}
			})
		case bytecode.OpSetName:
			// The function on top takes the binding's name, where it has none.
			if !b.spill() || b.depth() == 0 {
				return 0, false
			}
			at := b.depth() - 1
			return 0, b.stmt(func(c *tctx) {
				if v := c.stack[at]; v.IsObject() {
					if fd := v.Object().fn(); fd != nil && fd.name == "" {
						fd.name = c.r.atoms.name(c.names[a])
					}
				}
			})
		default:
			return -1, false
		}
		return 0, true
	}

	getGlobalOpt = func(r *Runtime, f *frame, cl *closure, name Atom) (Value, error) {
		env := cl.scope()
		if f.evalVars != nil {
			if p := evalVarProp(f.evalVars, name); p.ok() {
				return p.value(), nil
			}
		}
		if p := r.globalLexProp(env, name); p.ok() {
			if p.value().IsUninitialized() {
				return Undefined, r.throwReferenceError("cannot access %q before it is initialized", r.atoms.name(name))
			}
			return p.value(), nil
		}
		if !r.isGlobalScope(env) {
			if p := r.moduleLexProp(env, name); p.ok() {
				if p.flags()&propUninit != 0 {
					return Undefined, r.throwReferenceError("cannot access %q before it is initialized", r.atoms.name(name))
				}
				if p.flags()&(propAccessor|propPrivate|propDeleted) == 0 {
					return p.value(), nil
				}
			}
		}
		if chainHasProxy(env) {
			// Asked whether it has the name first, as for a read; what is not
			// there is undefined.
			has, err := r.hasPropErr(env, name)
			if err != nil {
				return Undefined, err
			}
			if !has {
				return Undefined, nil
			}
		}
		return r.getProp(env, name, Obj(env))
	}

	inOperator = func(r *Runtime, key, obj Value) (Value, error) {
		if !obj.IsObject() {
			return Undefined, r.throwTypeError("the right operand of \"in\" must be an object")
		}
		n, err := r.toPropertyName(key)
		if err != nil {
			return Undefined, err
		}
		k, known := r.keyFor(obj.Object(), n)
		if !known {
			return False, nil
		}
		has, err := r.hasPropErr(obj.Object(), k)
		if err != nil {
			return Undefined, err
		}
		return Bool(has), nil
	}

	toObjectValue = func(r *Runtime, v Value) (Value, error) {
		o, err := r.toObject(v)
		if err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	}

	deleteIndexed = func(r *Runtime, obj, key Value, strict bool) (Value, error) {
		if obj.IsNullish() {
			// There is nothing to delete from, which is a TypeError before the
			// key is even converted.
			return Undefined, r.throwTypeError("cannot delete a property of %s", r.describe(obj))
		}
		n, err := r.toPropertyName(key)
		if err != nil {
			return Undefined, err
		}
		if !obj.IsObject() {
			// A primitive's properties are its wrapper's, which ToObject
			// makes, as executeAt's case does.
			if obj, err = toObjectValue(r, obj); err != nil {
				return Undefined, err
			}
		}
		k, has := r.keyFor(obj.Object(), n)
		if !has {
			// Deleting what is not there succeeds.
			return True, nil
		}
		ok, err := r.deleteProp(obj.Object(), k, strict)
		if err != nil {
			return Undefined, err
		}
		return Bool(ok), nil
	}

	deleteVar = func(r *Runtime, f *frame, cl *closure, name Atom) (Value, error) {
		// What a direct eval declared can be deleted again, unlike a var the
		// source named: the evaluated code could have declared it anywhere, so
		// nothing may rely on it being there.
		if deleteEvalVar(f.evalVars, name) {
			return True, nil
		}
		if r.globalLexProp(cl.scope(), name).ok() {
			// A lexical binding is not a property and cannot be removed.
			return False, nil
		}
		ok, err := r.deleteProp(cl.scope(), name, false)
		if err != nil {
			return Undefined, err
		}
		return Bool(ok), nil
	}

	superIndexGet = func(r *Runtime, f *frame, base, rawKey Value) (Value, error) {
		if !base.IsObject() {
			return Undefined, r.throwTypeError("\"super\" has no prototype to read from")
		}
		key, err := r.toPropertyKey(rawKey)
		if err != nil {
			return Undefined, err
		}
		this, _ := f.thisValue()
		return r.getProp(base.Object(), key, this)
	}

	superIndexSet = func(r *Runtime, f *frame, base, rawKey, val Value, strict bool) error {
		if !base.IsObject() {
			return r.throwTypeError("\"super\" has no prototype to assign to")
		}
		key, err := r.toPropertyKey(rawKey)
		if err != nil {
			return err
		}
		return r.superSetFrom(f, base, key, val, strict)
	}

	concatParts = func(r *Runtime, parts []Value) (Value, error) {
		for i := range parts {
			if parts[i].IsString() || parts[i].IsNumber() {
				continue
			}
			s, err := r.toString(parts[i])
			if err != nil {
				return Undefined, err
			}
			parts[i] = Str(s)
		}
		// The strings alone may be too long, which is known before anything is
		// written out.
		total := 0
		for _, p := range parts {
			if p.IsString() && total <= maxStringLength {
				total += p.String().length
			}
		}
		if total > maxStringLength {
			return Undefined, r.throwStringLength()
		}
		out, err := r.joinParts(parts)
		if err != nil {
			return Undefined, err
		}
		if out.length > maxStringLength {
			return Undefined, r.throwStringLength()
		}
		return Str(out), nil
	}

	privateOp = func(b *tbuilder, pc int, in bytecode.Instr, a, k uint32) (int, bool) {
		switch in.Op {
		case bytecode.OpPrivateName:
			b.push(func(c *tctx) Value { return c.r.newPrivateName(c.names[a]) })
		case bytecode.OpGetPrivate:
			obj := b.pop().tree()
			b.push(func(c *tctx) Value {
				o := obj(c)
				c.at(pc)
				if !o.IsObject() {
					c.throw(c.r.throwTypeError("cannot read a private member of %s", c.r.describe(o)))
				}
				v, err := c.r.getPrivate(o.Object(), privateKey(c.f, c.cl, k), c.names[a])
				if err != nil {
					c.throw(err)
				}
				return v
			})
		case bytecode.OpSetPrivate:
			val, obj := b.pop().tree(), b.pop().tree()
			return 0, b.stmt(func(c *tctx) {
				o := obj(c)
				v := val(c)
				c.at(pc)
				if !o.IsObject() {
					c.throw(c.r.throwTypeError("cannot write a private member of %s", c.r.describe(o)))
				}
				if err := c.r.setPrivate(o.Object(), privateKey(c.f, c.cl, k), c.names[a], v); err != nil {
					c.throw(err)
				}
			})
		case bytecode.OpDefinePrivate, bytecode.OpDefinePrivateMethod,
			bytecode.OpDefinePrivateGetter, bytecode.OpDefinePrivateSetter:
			// The object the member is defined on stays.
			val := b.pop().tree()
			if !b.spill() || b.depth() == 0 {
				return 0, false
			}
			at, op := b.depth()-1, in.Op
			return 0, b.stmt(func(c *tctx) {
				v := val(c)
				target := c.stack[at]
				if !target.IsObject() {
					return
				}
				key := privateKey(c.f, c.cl, k)
				if op == bytecode.OpDefinePrivateGetter || op == bytecode.OpDefinePrivateSetter {
					if v.IsObject() {
						c.r.definePrivateAccessor(target.Object(), key, v.Object(), op == bytecode.OpDefinePrivateGetter)
					}
					return
				}
				c.at(pc)
				// A private field can only be added once, and only to an object
				// open to new members.
				if target.Object().getOwn(key).ok() {
					c.throw(c.r.throwTypeError("%s is already present on this object", c.r.atoms.name(key)))
				}
				if !target.Object().IsExtensible() {
					c.throw(c.r.throwTypeError("cannot add %s to a non-extensible object", c.r.atoms.name(key)))
				}
				flags := propFlags(propWritable | propPrivate)
				if op == bytecode.OpDefinePrivateMethod {
					// A method is not a place to store anything.
					flags = propPrivate
				}
				target.Object().setOwnRaw(key, v, flags)
			})
		case bytecode.OpPrivateIn:
			obj := b.pop().tree()
			b.push(func(c *tctx) Value {
				o := obj(c)
				if !o.IsObject() {
					c.at(pc)
					c.throw(c.r.throwTypeError(
						"the right operand of \"in\" must be an object, not %s", c.r.describe(o)))
				}
				// An own property and nothing else: an object that inherits from
				// an instance's prototype is not an instance.
				return Bool(o.Object().getOwn(privateKey(c.f, c.cl, k)).ok())
			})
		case bytecode.OpNewPrivateMethods:
			b.push(func(c *tctx) Value { return c.r.newPrivateMethods() })
		case bytecode.OpAddPrivateMethod, bytecode.OpAddPrivateGetter, bytecode.OpAddPrivateSetter:
			fn, list := b.pop().tree(), b.pop().tree()
			op := in.Op
			return 0, b.stmt(func(c *tctx) {
				l := list(c)
				c.r.addPrivateMethod(l, privateKey(c.f, c.cl, k), fn(c), op)
			})
		case bytecode.OpInstallPrivateMethods:
			list := b.pop().tree()
			return 0, b.stmt(func(c *tctx) {
				l := list(c)
				c.at(pc)
				this, bound := c.f.thisValue()
				if !bound {
					c.throw(c.r.throwError(errReference, "\"this\" is not bound until super() has been called"))
				}
				if err := c.r.installPrivateMethods(this, l); err != nil {
					c.throw(err)
				}
			})
		default:
			return suspendOp(b, pc, in)
		}
		return 0, true
	}
}
