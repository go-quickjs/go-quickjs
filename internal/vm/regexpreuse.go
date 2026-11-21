package vm

import (
	"unsafe"

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
// Such a call is one the register compiler marks (bytecode.RMarkReuse),
// having read the function's code (its reuseOf): a call of a method named
// exec whose result is stored in a local, l, of a
// function that is no generator, async function or sloppy one with a
// direct eval, that no closure captures and is no parameter -- the
// arguments object may alias one -- and that every instruction reads, if
// it reads it, only for what the array cannot be reached from: its length,
// its index, its input, whether it is truthy, null or undefined, and its
// elements, read by an index. The pieces' Strings are made again too where
// the elements read are only tested, measured, or stored in one other
// local, s, which is only tested and measured the same way.
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
// regexpdead.go tells it of a result nothing reads), which the leaf
// takes as it begins, and uses where it makes the array of a match of
// that receiver. A frame native code runs holds its locals where the
// slots cannot say, so that its calls are told otherwise: native code's
// leaf's call of exec tells the leaf its frame's slot of the local as the
// call's mark, at a site of its own, and the array is made again only
// where that frame made the last (jit_regexpreuse.go). A call Go makes for native code -- an instruction
// run whole at its exit, a call of Go it made (Runtime's reuseQuiet) --
// tells nothing, and where native code goes on with a frame past its
// start, at a loop's header or after a call, the arrays the frame's slots
// hold are forgotten (execReuseForget). Nor is it told where a memory
// meter counts what is made, or a debugger may read the locals.
//
// The functions are generic, each instantiated with struct{} alone: Go
// lays a generic function's instantiations out after every function and
// closure of the package (docs/optimizing.md, "The layout lock").

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
// so first, and untold after; told nothing where Go runs an instruction
// or a call for native code (Runtime's reuseQuiet), meters memory or has
// a debugger.
func reuseCallRest[_ any](r *Runtime, site *execReuseSite, locals []Value, o *Object, fd *funcData, recv Value, args []Value) (Value, error) {
	if r.regexpExecDead == nil || !recv.IsObject() || r.meter != nil || r.debug != nil || r.reuseQuiet != 0 {
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

// execReuseForget drops the arrays the table remembers held in a slot of
// the n locals at the stack's wb, a frame's that native code goes on with
// past its start (vm's jitRegGoOnAt): native code keeps the frame's locals where its slots
// cannot say, so that what a slot holds tells nothing of whether the array
// is done with -- an array native code still reads from a register, made
// again by a call made beneath it, had been written over under it. An
// array dropped is left as it is; the site's next call makes a new one.
func execReuseForget[_ any](r *Runtime, wb, n int) {
	if r.regexpExecDead == nil || n == 0 {
		return
	}
	locals := r.stack[wb : wb+n]
	tb := execReuseLeaf[struct{}](r).reuseTable
	if tb == nil {
		return
	}
	lo := uintptr(unsafe.Pointer(&locals[0]))
	hi := uintptr(unsafe.Pointer(&locals[len(locals)-1]))
	in := func(p *Value) bool {
		at := uintptr(unsafe.Pointer(p))
		return p != nil && at >= lo && at <= hi
	}
	for i := range tb.e {
		if e := &tb.e[i]; e.site != nil && (in(e.held) || in(e.sHeld)) {
			*e = execReuseEntry{}
		}
	}
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

// execReuseAt is the table's entry for the site's arrays of n elements.
func execReuseAt[_ any](tb *execReuseTable, site *execReuseSite, n int) *execReuseEntry {
	return &tb.e[(uintptr(unsafe.Pointer(site))>>4^uintptr(n))%uintptr(len(tb.e))]
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
	e := execReuseAt[struct{}](tb, site, n)
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
