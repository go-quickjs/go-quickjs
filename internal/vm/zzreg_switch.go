package vm

import (
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regSwitchTable is a switch instruction's table, as the tree tier's
// switchLookup makes one of the same cases: a number by an array's index
// where the cases are integers of a small range, by a short list or a map
// otherwise; a string by its code units, past the longest case's length
// not even flattened; true, false and null by their kind. Each selector's
// first case is the one it goes to, as the chain of === would.
type regSwitchTable struct {
	dense  []int32
	lo, hi float64
	few    []float64
	fewTo  []int32
	nums   map[float64]int32
	// byLen is where flat's strings of each length start, as the tree's
	// switchLookup groups them.
	byLen []int32
	flat  []bytecode.SwitchCase
	other [3]int32
	// typeTo is the body of each type's name in typeofNames, for
	// switch_typeof.
	typeTo [8]int32
}

// newRegSwitchTable is the table of cases.
func newRegSwitchTable(cases []bytecode.SwitchCase) *regSwitchTable {
	t := &regSwitchTable{other: [3]int32{-1, -1, -1}, lo: math.Inf(1), hi: math.Inf(-1)}
	for i := range t.typeTo {
		t.typeTo[i] = -1
	}
	nums := map[float64]int32{}
	var order []float64
	seen := map[string]bool{}
	var strs []bytecode.SwitchCase
	ints := true
	for _, c := range cases {
		switch c.Kind {
		case bytecode.SwitchNum:
			f := c.Num
			if f != f {
				continue // NaN equals nothing.
			}
			if _, ok := nums[f]; !ok {
				nums[f] = int32(c.To)
				order = append(order, f)
				t.lo, t.hi = min(t.lo, f), max(t.hi, f)
				ints = ints && f == math.Trunc(f) && !math.IsInf(f, 0)
			}
		case bytecode.SwitchStr:
			if !seen[c.Str] {
				seen[c.Str] = true
				strs = append(strs, c)
				for i, name := range typeofNames {
					if name == c.Str {
						t.typeTo[i] = int32(c.To)
					}
				}
			}
		default:
			if i := c.Kind - bytecode.SwitchTrue; t.other[i] < 0 {
				t.other[i] = int32(c.To)
			}
		}
	}
	if len(strs) > 0 {
		most := 0
		lens := make([]int, len(strs))
		for i, s := range strs {
			lens[i] = NewString(s.Str).length
			most = max(most, lens[i])
		}
		t.byLen = make([]int32, most+2)
		for n := 0; n <= most; n++ {
			t.byLen[n] = int32(len(t.flat))
			for i, s := range strs {
				if lens[i] == n {
					t.flat = append(t.flat, s)
				}
			}
		}
		t.byLen[most+1] = int32(len(t.flat))
	}
	switch {
	case len(nums) == 0:
	case ints && t.hi-t.lo < switchDense:
		t.dense = make([]int32, int(t.hi-t.lo)+1)
		for i := range t.dense {
			t.dense[i] = -1
		}
		for f, to := range nums {
			t.dense[int(f-t.lo)] = to
		}
	case len(nums) <= 8:
		for _, f := range order {
			t.few, t.fewTo = append(t.few, f), append(t.fewTo, nums[f])
		}
	default:
		t.nums = nums
	}
	return t
}

// lookup is the pc of the body v goes to, or -1: a string the constant a
// case was made from first by its pointer, as the tree's lookup asks.
func (t *regSwitchTable) lookup(v Value, consts []Value) int32 {
	switch {
	case v.IsNumber():
		f := v.num
		if t.dense != nil {
			if f >= t.lo && f <= t.hi {
				if i := int(f - t.lo); float64(i)+t.lo == f {
					return t.dense[i]
				}
			}
			return -1
		}
		for i, g := range t.few {
			if f == g {
				return t.fewTo[i]
			}
		}
		if t.nums != nil {
			if to, ok := t.nums[f]; ok {
				return to
			}
		}
	case v.IsString():
		s := (*String)(v.ref)
		if s.length >= len(t.byLen)-1 {
			return -1
		}
		for _, e := range t.flat[t.byLen[s.length]:t.byLen[s.length+1]] {
			if int(e.K) < len(consts) && consts[e.K].ref == unsafe.Pointer(s) {
				return int32(e.To)
			}
			if s.left != nil {
				s.Go()
			}
			if s.s == e.Str {
				return int32(e.To)
			}
		}
	case v.IsBool():
		if v.Truthy() {
			return t.other[0]
		}
		return t.other[1]
	case v.IsNull():
		return t.other[2]
	}
	return -1
}

// regCold is the operations runReg leaves to a case of one call: each
// case added to the loop moves the code of every other, which no placement
// varies, so only what runs per turn of a hot loop is a case of its own.
// It switches on the operation again. None of them throws.
func (r *Runtime) regCold(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) {
	w := regAt(c.code, k.pc-1)
	switch w.Op() {
	case bytecode.RNewArrayE:
		r.regNewArrayE(f, c, k, regs)
	case bytecode.RSwitch:
		regSwitch(c, k, regs[w.A()], f.cl.consts)
	case bytecode.RSwitchTypeof:
		regSwitchTypeof(c, k, regs[w.A()])
	case bytecode.RTypeof:
		r.regTypeof(c, k, regs)
	case bytecode.RNewObjectLit:
		r.regNewObjectLit(f, c, k, regs)
	}
}

// regColdSub is cold's operations, by the sub-operation in X: one opcode
// value for every operation that does not run per turn of a hot loop, and
// may throw. The loop runs cold through the slow path's case, regSlow,
// whose error it returns: an error check in regCold's case made empty_loop
// 7.5% and string_build1 6.4% slower at every placement, and a case of
// cold's own, before the free values', empty_loop 7.6% and loop_sum 4.4%,
// the loop's blocks laid out otherwise.
// Every one of them may throw: the frame's pc is the instruction's first.
func (r *Runtime) regColdSub(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	f.pc = c.src[pc] + 1
	switch bytecode.RColdOp(w.X()) {
	case bytecode.RColdNewRegExp:
		v, err := r.newRegExpLiteral(regConst(f.cl.fn, c, w.BC()))
		if err != nil {
			return err
		}
		regs[w.A()] = v
		return nil
	case bytecode.RColdWrap:
		return r.regColdWrap(c, k, w, regs)
	case bytecode.RColdForOfStart, bytecode.RColdForInStart:
		start := (*Runtime).startForOf
		if bytecode.RColdOp(w.X()) == bytecode.RColdForInStart {
			start = (*Runtime).startForIn
		}
		v, err := start(r, regs[w.B()])
		if err != nil {
			return err
		}
		regs[w.A()] = v
		return nil
	case bytecode.RColdIterClose:
		return r.iterCloseNormal(regs[w.B()])
	case bytecode.RColdIterCloseThrow:
		// As closeIteratorsIn closes a cursor an exception leaves: what
		// its return method throws is dropped.
		st := cursorOf(regs[w.B()])
		if st == nil || st.forIn || st.done || st.delegation || st.arr != nil && !r.unwalkToClose(st) {
			return nil
		}
		st.done = true
		r.closeIterator(st.iter)
		return nil
	case bytecode.RColdCheckGlobalLex, bytecode.RColdCheckGlobalVar, bytecode.RColdDefineGlobalVar,
		bytecode.RColdDeclareGlobalLex, bytecode.RColdDefineGlobalFunc, bytecode.RColdInitGlobalLex,
		bytecode.RColdDeclareModuleLex, bytecode.RColdInitModuleLex:
		return r.regColdTop(f, c, pc, w, regs)
	case bytecode.RColdIterStep, bytecode.RColdIterRest, bytecode.RColdCheckCoercible, bytecode.RColdToPropertyKey,
		bytecode.RColdObjectRest, bytecode.RColdRestParam, bytecode.RColdToString, bytecode.RColdTemplateObject:
		return r.regColdPattern(f, c, pc, w, regs)
	case bytecode.RColdCopyDataProps, bytecode.RColdDefineGetter, bytecode.RColdDefineSetter,
		bytecode.RColdDefineGetterIndex, bytecode.RColdDefineSetterIndex, bytecode.RColdDefineIndex,
		bytecode.RColdSetFuncName, bytecode.RColdSetHomeObject, bytecode.RColdSetProtoOf,
		bytecode.RColdArrayPush, bytecode.RColdArraySpread, bytecode.RColdCallSpread:
		return r.regColdObject(f, c, pc, w, regs)
	case bytecode.RColdAssignConst:
		// An assignment to a constant is a runtime error, not an early one.
		return r.throwTypeError("assignment to constant variable %q", r.atoms.name(regName(f.cl, c, pc, w.B())))
	case bytecode.RColdThrowRef:
		return r.throwError(errReference, "%s", r.atoms.name(regName(f.cl, c, pc, w.B())))
	case bytecode.RColdDebug:
		return r.regDebug(f, c, pc, w, regs)
	case bytecode.RColdMovFar:
		regs[w.A()] = *regFar(regs, w.BC())
		return nil
	case bytecode.RColdMovToFar:
		*regFar(regs, w.BC()) = regs[w.A()]
		return nil
	case bytecode.RColdFarInit:
		for i, n := w.BC(), w.BC()+regAuxAt(c, pc); i < n; i++ {
			*regFar(regs, i) = Undefined
		}
		return nil
	case bytecode.RColdNewClass, bytecode.RColdSetFieldInit, bytecode.RColdDefineMethod,
		bytecode.RColdClassGetter, bytecode.RColdClassSetter, bytecode.RColdClassGetterIndex,
		bytecode.RColdClassSetterIndex, bytecode.RColdClassIndex, bytecode.RColdPrivateName,
		bytecode.RColdNewPrivateMethods, bytecode.RColdAddPrivateMethod, bytecode.RColdAddPrivateGetter,
		bytecode.RColdAddPrivateSetter, bytecode.RColdInstallPrivateMethods, bytecode.RColdDefinePrivate,
		bytecode.RColdDefinePrivateMethod, bytecode.RColdDefinePrivateGetter, bytecode.RColdDefinePrivateSetter,
		bytecode.RColdPrivateIn, bytecode.RColdSuperCall,
		bytecode.RColdSuperForward, bytecode.RColdSuperSpread, bytecode.RColdSuperGet, bytecode.RColdSuperSet,
		bytecode.RColdSuperBase, bytecode.RColdSuperGetIndex, bytecode.RColdSuperSetIndex, bytecode.RColdThis,
		bytecode.RColdCheckThis, bytecode.RColdNewTarget, bytecode.RColdDerivedRet, bytecode.RColdDeleteSuper,
		bytecode.RColdCloseRegs:
		return r.regColdClass(f, c, pc, w, regs)
	case bytecode.RColdCheckGlobalRef:
		found, err := r.regCheckGlobalRef(f, regName(f.cl, c, pc, w.B()), int(w.C()))
		regs[w.A()] = Bool(found)
		return err
	case bytecode.RColdAssertResolved:
		if !regs[w.A()].Truthy() {
			return r.throwReferenceError("%s is not defined", r.atoms.name(regName(f.cl, c, pc, w.B())))
		}
		return nil
	case bytecode.RColdGetGlobalOpt:
		v, err := r.regGlobalOpt(f, regName(f.cl, c, pc, w.B()))
		regs[w.A()] = v
		return err
	case bytecode.RColdDeleteVar:
		v, err := deleteVar(r, f, f.cl, regName(f.cl, c, pc, w.B()))
		regs[w.A()] = v
		return err
	case bytecode.RColdImport:
		options := Undefined
		if w.C()&3 == 2 {
			options = regs[w.B()+1]
		}
		regs[w.A()] = r.importCall(uint32(w.C()>>2), regs[w.B()], options)
		return nil
	case bytecode.RColdImportMeta:
		regs[w.A()] = r.importMeta(f.cl.env)
		return nil
	case bytecode.RColdNewDispose, bytecode.RColdAddDisposable, bytecode.RColdDispose, bytecode.RColdDisposeStep:
		return r.regColdDispose(w, regs)
	case bytecode.RColdGenEntry, bytecode.RColdEndParams, bytecode.RColdAwait,
		bytecode.RColdYieldStar, bytecode.RColdIterResume, bytecode.RColdIterUnpackDelegate,
		bytecode.RColdForAwaitOfStart, bytecode.RColdAsyncIterNext, bytecode.RColdIterResult,
		bytecode.RColdAsyncIterClose, bytecode.RColdAsyncIterCheck:
		return r.regColdGen(f, c, k, w, regs)
	case bytecode.RColdGetUpvalCheck, bytecode.RColdSetUpvalCheck:
		return r.regColdNames(f, w, regs)
	case bytecode.RColdDirectEval:
		return r.regDirectEval(f, c, pc, w, regs)
	case bytecode.RColdWithGet, bytecode.RColdWithGetThis, bytecode.RColdWithDelete, bytecode.RColdWithSet,
		bytecode.RColdWithResolve, bytecode.RColdWithRead, bytecode.RColdWithWrite, bytecode.RColdWithPush,
		bytecode.RColdWithPop:
		return r.regColdWith(f, c, pc, w, regs)
	}
	// get_private and set_private, whose X is no sub-operation
	// (bytecode.RPrivateX), which the loop's slow path sends here: so they
	// move none of the hot block's code.
	if w.Op() != bytecode.RCold {
		return r.regPrivate(f, c, k, regs)
	}
	panic("register code: no cold operation " + bytecode.RColdOp(w.X()).String())
}
