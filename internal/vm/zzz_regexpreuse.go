package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

// A match's array that nothing but a local of the function that made it
// can reach is made again, at the call's next match, in the memory of the
// last: the array and the Strings of its pieces, which V8 bumps one
// pointer to make and Go makes in one allocation or two of its collected
// heap -- for a URL's pattern of eleven groups 352 bytes and 240, the most
// of what such an exec costs. Octane's RegExp runs its Exec,
//
//	re.lastIndex = 0; var array = re.exec(string)
//	if (array) for (var i = 0; i < array.length; i++) {
//	  var substring = array[i]; if (substring) sum += substring.length }
//
// some 15,000 times an iteration, each array garbage once it returns.
//
// Such a call is one the register compiler marks (bytecode.RMarkReuse)
// and execReuseOf, reading the stack form, finds: a call of a method named
// exec whose result is stored in a local, l, of a
// function that is no generator, async function or sloppy one with a
// direct eval, that no closure captures and is no parameter -- the
// arguments object may alias one -- and that every instruction reads, if
// it reads it, only for what the array cannot be reached from: its length,
// its index, its input, whether it is truthy, null or undefined, and its
// elements, read by an index. The pieces' Strings are made again too where
// the elements read are only tested, measured, or stored in one other
// local, s, which is only tested and measured the same way (execReuseOf).
// A function that keeps or hands on what it read has its arrays made as
// before.
//
// That a function's code reads the array only so says nothing of when the
// one it made last is done with: the frame that made it may still run,
// beneath the one about to match -- a getter, a valueOf, a recursion made
// the call again -- or a loop of its own may still read the last array's
// pieces in s. So the table of a runtime's arrays (execReuseTable) keeps,
// with each, the slots of the locals its frame holds it in: the array is
// made again only where the slot of l holds it no more, or it is this
// frame's own slot, about to be written, and the slot of s holds none of
// its pieces -- each read where the match is made. A frame that has
// returned has its slots written by the frames that came after it, or
// read as still holding it, which makes a new array, as any doubt does.
// The array itself never leaves the local, so the slots are the only
// places it can be read from: the leaf's legacy statics keep the
// subject and the captures, not the array.
//
// The call is made as any other, through callRest, by the same path: the
// leaf's check, the leaf's call without a frame, the frame and the tick
// where it is refused. The call only tells exec's leaf, just before, of
// the call's local and the frame's locals (execReuseCall, as
// zzz_regexpdead.go tells it of a result nothing reads), which the leaf
// takes as it begins, and uses where it makes the array of a match of
// that receiver. Native code tells it nothing, and so where a runtime runs
// the JIT nothing is told: a frame native code took over in a loop holds
// its locals where the slots cannot say. Nor is it where a memory meter
// counts what is made, or a debugger may read the locals.
//
// The functions are generic, each instantiated with struct{} alone: Go
// lays a generic function's instantiations out after every function and
// closure of the package (see zzz_direct.go).

// execReuseSite is a call of exec whose arrays may be made again: l is the
// local its result is stored in; strs says its pieces' Strings may be made
// again too, and s is the one other local they are stored in, or -1. It is
// the register code's (regReuseSites), and holds nothing of any runtime's.
type execReuseSite struct {
	l, s int
	strs bool
}

// execReuseCall is what exec's leaf is told of the call about to be made:
// its site, its receiver, and the locals of the frame making it.
type execReuseCall struct {
	site   *execReuseSite
	recv   *Object
	locals []Value
}

// execReuseEntry is an array a site's call made, of n elements, and the
// Strings its pieces were made in: held and sHeld are the slots of the
// frame that holds it, l's and s's, or nil.
type execReuseEntry struct {
	site        *execReuseSite
	n           int
	arr         *Object
	strs        []String
	held, sHeld *Value
	// cell is the prototypes' cell the array was made under
	// (execReuseChain), which it is made again only while valid.
	cell *protoCell
}

// execReuseTable is a runtime's arrays to make again, by site and number
// of elements: a site's matches of patterns of different numbers of groups
// -- Exec's, of a hundred patterns -- each have one.
type execReuseTable struct {
	e [32]execReuseEntry
	// clean is the cell of the array's prototypes, Array.prototype's and
	// Object.prototype's, taken where they had no accessor but
	// __proto__ (execReuseChain): while it is valid, they have none.
	clean *protoCell
}

// execReuseChain is the validity cell of the chain of a match's array's
// prototypes, Array.prototype and Object.prototype, where neither has an
// accessor but Object.prototype's __proto__, or nil. An element of the
// array read by a number (numberLocal) -- past its length, the
// prototypes' -- then runs no getter, which would be given the array, and
// while the cell is valid neither changes its layout or its prototype, as
// defining an accessor does.
func execReuseChain[_ any](r *Runtime) *protoCell {
	ap, op := r.proto.array, r.proto.object
	if ap == nil || op == nil || ap.proto() != op || op.proto() != nil {
		return nil
	}
	for _, o := range [2]*Object{ap, op} {
		if s := o.shape; s != nil {
			for _, k := range s.keys[:s.n] {
				if k.flags&propAccessor != 0 && k.flags&propDeleted == 0 && (o != op || k.key != atomProto) {
					return nil
				}
			}
		}
	}
	return r.chainCell(ap)
}

// execReuseLeaf is exec's leaf, regexpLeaves', which r.regexpExecDead
// points into.
func execReuseLeaf[_ any](r *Runtime) *regexpLeaf[struct{}] {
	return (*regexpLeaf[struct{}])(unsafe.Add(unsafe.Pointer(r.regexpExecDead), -int(unsafe.Offsetof(regexpLeaf[struct{}]{}.dead))))
}

// reuseCallRest is callRest's call of o on recv with args, a call of exec
// of the site's, from the frame whose locals are locals: exec's leaf told
// so first, and untold after; told nothing where the runtime runs the JIT,
// meters memory or has a debugger.
func reuseCallRest[_ any](r *Runtime, site *execReuseSite, locals []Value, o *Object, fd *funcData, recv Value, args []Value) (Value, error) {
	if r.regexpExecDead == nil || !recv.IsObject() || r.meter != nil || r.debug != nil || r.jitOn() {
		return r.callRest(o, fd, recv, args)
	}
	l := execReuseLeaf[struct{}](r)
	rc := execReuseCall{site: site, recv: recv.Object(), locals: locals}
	if fd != nil && !fd.bound && (fd.realm == nil || fd.realm == r.Realm) {
		// The leaf's match made here, as callRest would make it, told of
		// the site as it is called -- and of a global or a sticky
		// RegExp's too, the call made once, what statefulCheck asks
		// asked by matchOf with the rest. One refused, or that errs, has
		// made nothing, and is made by callRest's path, which throws an
		// error with a frame.
		if v, err := l.matchOf(r, recv, args, nil, rc, true); err == nil {
			return v, nil
		}
	}
	l.reuse = rc
	v, err := r.callRest(o, fd, recv, args)
	if l.reuse.site != nil {
		l.reuse = execReuseCall{}
	}
	return v, err
}

// heldBy reports whether the slot p holds the array a: a slot of a frame
// that has returned -- at or above the stack's top -- holds nothing.
func heldBy[_ any](r *Runtime, p *Value, a *Object) bool {
	return p != nil && !slotGone[struct{}](r, p) && p.IsObject() && p.Object() == a
}

// piecesIn reports whether the slot p holds a String of strs, as heldBy
// reads it.
func piecesIn[_ any](r *Runtime, p *Value, strs []String) bool {
	if p == nil || len(strs) == 0 || slotGone[struct{}](r, p) || !p.IsString() {
		return false
	}
	at := uintptr(unsafe.Pointer(p.String()))
	first := uintptr(unsafe.Pointer(&strs[0]))
	return at >= first && at <= uintptr(unsafe.Pointer(&strs[len(strs)-1]))
}

// slotGone reports whether the slot p is of the stack at or above its top,
// where no frame is: a frame's registers are beneath the top while it
// runs, and its slots are left as they were when it
// returns, to be written by the frames after it.
func slotGone[_ any](r *Runtime, p *Value) bool {
	if len(r.stack) == 0 {
		return false
	}
	i := (uintptr(unsafe.Pointer(p)) - uintptr(unsafe.Pointer(&r.stack[0]))) / unsafe.Sizeof(Value{})
	return uintptr(unsafe.Pointer(p)) >= uintptr(unsafe.Pointer(&r.stack[0])) && i < uintptr(len(r.stack)) && i >= uintptr(r.stackTop)
}

// matchResultReused is matchResultOf's array of a match of re, for the
// site's call rc: the site's last array of as many elements, and its
// pieces' Strings where the site's are made again too, written again
// where no frame holds it (see the top of this file); a new one, which
// the site keeps, where one may.
func matchResultReused[_ any](r *Runtime, tbp **execReuseTable, rc execReuseCall, re *regexp.Regexp, caps []int, input *String) *Object {
	n := len(caps) / 2
	t, site := r.matchLayout, rc.site
	if t == nil || t.shape.proto != r.proto.array || n > matchBlockMax || re.Flags()&regexp.FlagHasIndices != 0 || site.l >= len(rc.locals) || site.s >= len(rc.locals) ||
		!site.strs && n <= 4 {
		return matchResultOf[struct{}](r, re, caps, input)
	}
	if len(re.GroupNames()) != 0 {
		// The groups' object holds the pieces too.
		return matchResultOf[struct{}](r, re, caps, input)
	}
	tb := *tbp
	if tb == nil {
		tb = new(execReuseTable)
		*tbp = tb
	}
	cell := tb.clean
	if cell == nil || !cell.valid {
		if cell = execReuseChain[struct{}](r); cell == nil {
			tb.clean = nil
			return matchResultOf[struct{}](r, re, caps, input)
		}
		tb.clean = cell
	}
	e := &tb.e[(uintptr(unsafe.Pointer(site))>>4^uintptr(n))%uintptr(len(tb.e))]
	held := &rc.locals[site.l]
	var sHeld *Value
	if site.s >= 0 {
		sHeld = &rc.locals[site.s]
	}
	if e.site != site || e.n != n || e.cell != cell || e.arr.proto() != r.proto.array || e.held != held && heldBy[struct{}](r, e.held, e.arr) || site.strs && piecesIn[struct{}](r, e.sHeld, e.strs) {
		// A new array, with room for a String for each element where the
		// pieces are made again.
		var strs []String
		var arr *Object
		if site.strs {
			arr, strs = matchBlockOf[struct{}](r.proto.array, n, n)
			if strs == nil {
				strs = make([]String, n)
			}
		} else {
			arr, _ = matchBlockOf[struct{}](r.proto.array, n, 0)
		}
		*e = execReuseEntry{site: site, n: n, arr: arr, strs: strs, cell: cell}
	}
	e.held, e.sHeld = held, sHeld
	arr, strs := e.arr, e.strs
	if !site.strs {
		k := 0
		for i := 0; i < len(caps); i += 2 {
			if lo, hi := caps[i], caps[i+1]; lo >= 0 && hi > lo && (lo > 0 || hi < input.length) {
				k++
			}
		}
		strs = make([]String, k)
	}
	elems := arr.extAt().elems
	for i := range elems {
		lo, hi := caps[2*i], caps[2*i+1]
		switch {
		case lo < 0 || hi < 0:
			elems[i] = Undefined
		case hi > lo && (lo > 0 || hi < input.length):
			elems[i] = Str(input.substringInto(&strs[0], lo, hi))
			strs = strs[1:]
		default:
			elems[i] = Str(input.Substring(lo, hi))
		}
	}
	groups := r.groupsObject(r.groupsLayoutOf(re), elems)
	arr.setVals(arr.vals()[:3])
	arr.shape = t.shape
	arr.vals()[0] = Int(caps[0])
	arr.vals()[1] = Str(input)
	arr.vals()[2] = groups
	return arr
}

// execReuseMaxCode is the longest function whose code execReuseOf reads.
const execReuseMaxCode = 1 << 14

// execReuseScan is execReuseOf's reading of fn's code for the local l its
// call stores in: strs and s are what it has found of the elements' reads
// (execReuseSite's), and targets the instructions jumps go to, made when
// first needed.
type execReuseScan[_ any] struct {
	fn      *bytecode.Function
	l, s    int
	strs    bool
	targets []bool
}

// execReuseOf is the site of the call whose result the instruction at
// store stores in local l, or nil where its arrays may not be made again:
// l is a parameter, is captured, or is read for anything but what the
// array cannot be reached from (see the top of this file).
func execReuseOf[_ any](fn *bytecode.Function, store, l int) *execReuseSite {
	code := fn.Code
	if l < fn.ParamCount || l >= len(fn.Locals) || fn.Locals[l].Captured || len(code) > execReuseMaxCode || fn.UsesArguments && fn.MappedArguments {
		return nil
	}
	x := &execReuseScan[struct{}]{fn: fn, l: l, s: -1, strs: true}
	// The store's own value, where it leaves one, is read by what follows.
	if st := code[store]; (st.Op == bytecode.OpPutLocal || st.Op == bytecode.OpSetLocalGet && int(st.B) == l) && !x.arrayRead(store+1) {
		return nil
	}
	for pc, in := range code {
		if pc == store {
			continue
		}
		switch in.Op {
		case bytecode.OpGetLocal, bytecode.OpGetLocalCheck:
			if int(in.A) == l && !x.arrayRead(pc+1) {
				return nil
			}
		case bytecode.OpGetLocal2:
			if int(in.A) == l || int(in.B) == l && !x.arrayRead(pc+1) {
				return nil
			}
		case bytecode.OpSetLocalGet:
			if int(in.B) == l && !x.arrayRead(pc+1) {
				return nil
			}
		case bytecode.OpPutLocal:
			if int(in.A) == l && !x.arrayRead(pc+1) {
				return nil
			}
		case bytecode.OpGetLocalIndex:
			if int(in.B) == l {
				return nil
			}
			if int(in.A) == l {
				// An element read by a local's index, which must be a
				// number.
				if !x.numberLocal(int(in.B)) {
					return nil
				}
				x.pieces(pc + 1)
			}
		case bytecode.OpSetLocal, bytecode.OpInitLocal, bytecode.OpSetLocalCheck, bytecode.OpClearLocal:
			// Another value stored, which leaves nothing behind it.
		case bytecode.OpIncLocal, bytecode.OpDecLocal, bytecode.OpUpdateLocal, bytecode.OpBinLocal,
			bytecode.OpLazyArguments, bytecode.OpCloseUpvalues, bytecode.OpInitParam:
			if int(in.A) == l {
				return nil
			}
		case bytecode.OpLocalBinImm:
			if int(in.A&(1<<24-1)) == l {
				return nil
			}
		case bytecode.OpGetLocalIndexUpdate:
			if int(in.A) == l || int(in.B>>2) == l {
				return nil
			}
		case bytecode.OpParamsToDeadZone, bytecode.OpWithPush:
			return nil
		}
	}
	if x.strs && x.s >= 0 && !x.piecesLocal() {
		x.strs = false
	}
	if !x.strs {
		x.s = -1
	}
	return &execReuseSite{l: l, s: x.s, strs: x.strs}
}

// arrayRead reports whether the instruction at pc, given the array on the
// stack, reads of it only what it cannot be reached from: its length,
// whether it is truthy, a comparison with null or undefined, its index and
// its input, an element by an index that is a number -- or drops it. An
// element read notes what reads the element (pieces).
func (x *execReuseScan[_]) arrayRead(pc int) bool {
	fn := x.fn
	code := fn.Code
	if pc >= len(code) {
		return false
	}
	switch in := code[pc]; in.Op {
	case bytecode.OpGetLength, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpNot, bytecode.OpDrop:
		return true
	case bytecode.OpGetProp:
		if int(in.A) < len(fn.Names) {
			name := fn.Names[in.A]
			return name == "index" || name == "input"
		}
	case bytecode.OpPushNull, bytecode.OpPushUndef:
		if pc+1 < len(code) && code[pc+1].Op == bytecode.OpJumpIfCmpFalse {
			switch bytecode.Op(code[pc+1].B) {
			case bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe:
				return true
			}
		}
	case bytecode.OpPushInt, bytecode.OpGetLocal:
		// An element, of an index pushed or of a local holding a number.
		if pc+1 >= len(code) || code[pc+1].Op != bytecode.OpGetIndex || in.Op == bytecode.OpGetLocal && (int(in.A) == x.l || !x.numberLocal(int(in.A))) {
			return false
		}
		x.pieces(pc + 2)
		return true
	}
	return false
}

// pieces notes an element's value on the stack, read by the instruction at
// pc: the pieces' Strings are made again only where every element read is
// only tested or measured, or stored in the one local s.
func (x *execReuseScan[_]) pieces(pc int) {
	fn := x.fn
	u, ok := x.pieceRead(pc)
	switch {
	case !ok:
		x.strs = false
	case u >= 0:
		if x.s >= 0 && x.s != u || u == x.l || u < fn.ParamCount || u >= len(fn.Locals) || fn.Locals[u].Captured {
			x.strs = false
		}
		x.s = u
	}
}

// numberLocal reports whether local k holds a number or nothing at all
// wherever it is read: it is no parameter, no closure captures it, and
// every store of it is of an integer pushed just before -- at a store no
// jump goes to -- or the dead zone's marker, or an increment or decrement
// of it; or it is cleared. An element read by it then reads an index, or
// "undefined", never __proto__, the one key an accessor of the array's
// prototypes may have (execReuseChain).
func (x *execReuseScan[_]) numberLocal(k int) bool {
	fn := x.fn
	if k < fn.ParamCount || k >= len(fn.Locals) || fn.Locals[k].Captured {
		return false
	}
	code := fn.Code
	for pc, in := range code {
		store := false
		switch in.Op {
		case bytecode.OpSetLocal, bytecode.OpPutLocal, bytecode.OpInitLocal, bytecode.OpSetLocalCheck, bytecode.OpSetLocalGet:
			store = int(in.A) == k
		case bytecode.OpLazyArguments, bytecode.OpCloseUpvalues, bytecode.OpInitParam:
			if int(in.A) == k {
				return false
			}
		}
		if !store {
			continue
		}
		if pc == 0 || x.target(pc) {
			return false
		}
		if p := code[pc-1].Op; p != bytecode.OpPushInt && p != bytecode.OpPushUninitialized {
			return false
		}
	}
	return true
}

// target reports whether a jump goes to the instruction at pc.
func (x *execReuseScan[_]) target(pc int) bool {
	if x.targets == nil {
		code := x.fn.Code
		x.targets = make([]bool, len(code)+1)
		for _, in := range code {
			switch in.Op {
			case bytecode.OpPushCatch, bytecode.OpPushFinally, bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue,
				bytecode.OpJumpIfCmpFalse, bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep, bytecode.OpJumpIfNullish,
				bytecode.OpJumpIfNotNullish, bytecode.OpIterNextOrJump, bytecode.OpIterResultOrJump, bytecode.OpIterUnpackDelegate,
				bytecode.OpIterUnpack, bytecode.OpAsyncIterClose, bytecode.OpDispose:
				if int(in.A) < len(x.targets) {
					x.targets[in.A] = true
				}
			case bytecode.OpIterResume:
				if int(in.B) < len(x.targets) {
					x.targets[in.B] = true
				}
			}
		}
	}
	return x.targets[pc]
}

// pieceRead is what the instruction at pc does with an element's
// value on the stack: ok where it only tests or measures it or drops it,
// or stores it in local s alone, s then; not ok where it may keep it
// anywhere else.
func (x *execReuseScan[_]) pieceRead(pc int) (s int, ok bool) {
	code := x.fn.Code
	if pc >= len(code) {
		return -1, false
	}
	switch in := code[pc]; in.Op {
	case bytecode.OpGetLength, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpNot, bytecode.OpDrop:
		return -1, true
	case bytecode.OpSetLocal:
		return int(in.A), true
	case bytecode.OpSetLocalGet, bytecode.OpPutLocal:
		// The value read again at once, which must be tested or measured
		// too.
		if in.Op == bytecode.OpSetLocalGet && in.B != in.A {
			return int(in.A), true
		}
		if u, ok := x.pieceRead(pc + 1); ok && u < 0 {
			return int(in.A), true
		}
	}
	return -1, false
}

// piecesLocal reports whether local s, the one an array's elements are
// stored in, is only read to be tested or measured.
func (x *execReuseScan[_]) piecesLocal() bool {
	fn, s := x.fn, x.s
	for pc, in := range fn.Code {
		read := false
		switch in.Op {
		case bytecode.OpGetLocal, bytecode.OpGetLocalCheck, bytecode.OpPutLocal:
			read = int(in.A) == s
		case bytecode.OpGetLocal2:
			if int(in.A) == s {
				return false
			}
			read = int(in.B) == s
		case bytecode.OpSetLocalGet:
			read = int(in.B) == s
		case bytecode.OpGetLocalIndex:
			if int(in.A) == s || int(in.B) == s {
				return false
			}
		case bytecode.OpGetLocalIndexUpdate:
			if int(in.A) == s || int(in.B>>2) == s {
				return false
			}
		case bytecode.OpIncLocal, bytecode.OpDecLocal, bytecode.OpUpdateLocal, bytecode.OpBinLocal,
			bytecode.OpLazyArguments, bytecode.OpCloseUpvalues, bytecode.OpInitParam:
			if int(in.A) == s {
				return false
			}
		case bytecode.OpLocalBinImm:
			if int(in.A&(1<<24-1)) == s {
				return false
			}
		}
		if read {
			if u, ok := x.pieceRead(pc + 1); !ok || u >= 0 {
				return false
			}
		}
	}
	return true
}
