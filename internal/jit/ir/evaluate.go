package ir

import (
	"errors"
	"math"
	"unsafe"
)

// ErrState reports an invalid scratch size or a non-resumable entry PC.
var ErrState = errors.New("jit IR: invalid execution state")

// Evaluate executes a validated program in Go for at most budget instructions.
// Slots is caller-owned scratch, containing locals followed by operands. Exits
// leave all live slots in interpreter order; unused slots may hold stale values.
// It can resume at every reachable state map, including with budget zero.
func (p *Program) Evaluate(slots []Value, pc int, budget uint64) (Exit, error) {
	return p.EvaluateArrays(slots, nil, pc, budget)
}

// EvaluateArrays is the correctness oracle for borrowed dense array storage.
func (p *Program) EvaluateArrays(slots []Value, arrays []ArrayView, pc int, budget uint64) (Exit, error) {
	if len(arrays) != 0 && len(arrays) != MaxSlots {
		return Exit{}, ErrState
	}
	if p == nil || len(slots) != p.Locals+p.StackSize || pc < 0 || pc >= len(p.Code) ||
		len(p.Maps) != len(p.Code) || p.Maps[pc].Depth < 0 {
		return Exit{}, ErrState
	}
	read := func(o Operand) Value {
		if o.Slot == -1 {
			return o.Literal
		}
		return slots[o.Slot]
	}
	var steps uint64
	for {
		exit := Exit{Kind: GuardExit, State: p.Maps[pc], Steps: steps}
		if steps == budget {
			exit.Kind = BudgetExit
			return exit, nil
		}
		in := p.Code[pc]
		if in.Check && slots[in.CheckSlot].Kind == Uninitialized {
			return exit, nil
		}
		next := pc + 1
		switch in.Op {
		case Nop:
		case Host, Call, TypeTest, BindingWrite, BindingCheck, Resolved, ObjectLiteral, FieldDefine, ArrayLiteral, StringConst, UpvalueRead, UpvalueWrite, StringAdd:
			// TypeTest's objects are Go's to classify.
			exit.Kind = HostExit
			return exit, nil
		case StringMethod, StringCode:
			exit.Kind = HostExit
			receiver := read(in.Left)
			if in.Op == StringCode {
				callee := receiver
				if callee.Kind != Opaque || callee.Bits >= uint64(len(arrays)) || arrays[callee.Bits].DenseLength != CharCodeAtBuiltin {
					return exit, nil
				}
				receiver = read(in.Right)
			}
			if receiver.Kind != String || receiver.Bits >= uint64(len(arrays)) {
				return exit, nil
			}
			view := arrays[receiver.Bits]
			if in.Op == StringMethod {
				if view.WritableHole == 0 || view.WritableHole > uint64(len(arrays)) {
					return exit, nil
				}
				slots[in.Dest] = Value{Kind: Opaque, Bits: view.WritableHole - 1}
			} else {
				index := read(in.Third)
				i := math.Float64frombits(index.Bits)
				if index.Kind != Number || i < 0 || i >= float64(view.DenseLength) || i != math.Trunc(i) || view.Data == nil || view.Length != 1 && view.Length != 2 {
					return exit, nil
				}
				var code uint16
				if view.Length == 1 {
					code = uint16(*(*byte)(unsafe.Add(view.Data, uintptr(i))))
				} else {
					code = *(*uint16)(unsafe.Add(view.Data, uintptr(i)*2))
				}
				slots[in.Dest] = Float(float64(code))
			}
		case ReferenceRead:
			exit.Kind = HostExit
			obj := read(in.Left)
			if obj.Kind != Opaque || obj.Bits >= uint64(len(arrays)) {
				return exit, nil
			}
			view := arrays[obj.Bits]
			if view.Data == nil || view.DenseLength > MaxProperties || view.NumberLimit != 0 || view.WritableHole != 0 || view.Length == 0 {
				return exit, nil
			}
			found := false
			for _, ref := range unsafe.Slice((*ReferenceCell)(view.Data), int(view.DenseLength)) {
				if ref.Key != in.Key {
					continue
				}
				cell := ref.Cell
				if cell == nil || cell.Key != in.Key || cell.Flags&^uint8(7) != 0 || cell.Bits != ref.Bits || cell.Reference != ref.Reference || ref.Handle >= MaxSlots {
					return exit, nil
				}
				slots[in.Dest] = Value{Kind: Opaque, Bits: ref.Handle}
				found = true
				break
			}
			if !found {
				return exit, nil
			}
		case PropertyRead, PropertyWrite, BindingRead:
			exit.Kind = HostExit
			obj := read(in.Left)
			if obj.Kind != Opaque || obj.Bits >= uint64(len(arrays)) {
				return exit, nil
			}
			view := arrays[obj.Bits]
			if view.NumberLimit != 0 || view.Data == nil || view.DenseLength > MaxProperties || view.WritableHole == 0 && (view.Length == 0 || in.Op == BindingRead) {
				return exit, nil
			}
			var cell *PropertyCell
			for i := uint64(0); i < view.DenseLength; i++ {
				var p *PropertyCell
				if view.WritableHole != 0 {
					p = (*PropertyCell)(unsafe.Add(view.Data, uintptr(i)*unsafe.Sizeof(PropertyCell{})))
				} else {
					ref := (*ReferenceCell)(unsafe.Add(view.Data, uintptr(i)*unsafe.Sizeof(ReferenceCell{})))
					if ref.Key != in.Key {
						continue
					}
					p = ref.Cell
				}
				if p != nil && p.Key == in.Key {
					cell = p
					break
				}
			}
			limit := view.WritableHole
			if limit == 0 {
				limit = view.Length
			}
			if cell == nil || cell.Flags&^uint8(7) != 0 || cell.Bits >= limit {
				return exit, nil
			}
			if in.Op != PropertyWrite {
				slots[in.Dest] = Value{Kind: Number, Bits: cell.Bits}
			} else {
				value := read(in.Right)
				if cell.Flags&1 == 0 || value.Kind != Number {
					return exit, nil
				}
				bits := value.Bits
				if math.IsNaN(math.Float64frombits(bits)) {
					bits = 0x7ff8000000000000
				}
				cell.Bits = bits
			}
		case Insert3:
			n := in.Dest
			a, b, c := slots[n], slots[n+1], slots[n+2]
			slots[n], slots[n+1], slots[n+2], slots[n+3] = c, a, b, c
		case Insert2:
			n := in.Dest
			a, b := slots[n], slots[n+1]
			slots[n], slots[n+1], slots[n+2] = b, a, b
		case ArrayRead, ArrayWrite, ArrayKey, ArrayUpdate, ArrayLength:
			if in.Op == ArrayWrite {
				exit.Kind = HostExit
			}
			obj := read(in.Left)
			if obj.Kind != Opaque || obj.Bits >= uint64(len(arrays)) {
				return exit, nil
			}
			view := arrays[obj.Bits]
			if view.NumberLimit == 0 {
				return exit, nil
			}
			if in.Op == ArrayLength {
				slots[in.Dest] = Float(float64(view.Length))
				break
			}
			key := read(in.Right)
			old := key
			if in.Op == ArrayUpdate {
				var ok bool
				key, ok = binary(in.Operator, old, Float(1))
				if !ok {
					return exit, nil
				}
				if in.Postfix {
					key = old
				}
			}
			if key.Kind != Number {
				return exit, nil
			}
			x := math.Float64frombits(key.Bits)
			if x < 0 || x > math.MaxUint32 || math.Trunc(x) != x {
				return exit, nil
			}
			if in.Op == ArrayKey {
				break
			}
			if uint64(x) >= view.DenseLength || view.Data == nil {
				return exit, nil
			}
			cell := (*uint64)(unsafe.Add(view.Data, uintptr(x)*16))
			if *cell >= view.NumberLimit && !(in.Op == ArrayWrite && view.WritableHole != 0 && *cell == view.WritableHole) {
				return exit, nil
			}
			if in.Op == ArrayWrite {
				v := read(in.Third)
				if v.Kind != Number {
					return exit, nil
				}
				if math.IsNaN(math.Float64frombits(v.Bits)) {
					v.Bits = 0x7ff8000000000000
				}
				*cell = v.Bits
			} else {
				v := Value{Bits: *cell, Kind: Number}
				if in.Op == ArrayUpdate {
					slots[in.Extra], _ = binary(in.Operator, old, Float(1))
				}
				slots[in.Dest] = v
			}
		case Copy:
			slots[in.Dest] = read(in.Left)
		case CopyPair:
			a, b := read(in.Left), read(in.Right)
			slots[in.Dest], slots[in.Extra] = a, b
		case StoreLoad:
			slots[in.Dest] = read(in.Left)
			slots[in.Extra] = read(in.Right)
		case Swap:
			slots[in.Dest], slots[in.Extra] = slots[in.Extra], slots[in.Dest]
		case Binary:
			v, ok := binary(in.Operator, read(in.Left), read(in.Right))
			if !ok {
				if in.Operator == Eq || in.Operator == Ne || in.Operator == Mod {
					exit.Kind = HostExit
				}
				return exit, nil
			}
			slots[in.Dest] = v
		case Unary:
			a := read(in.Left)
			if in.Operator == Not {
				b, ok := truth(a)
				if !ok {
					return exit, nil
				}
				slots[in.Dest] = Bool(!b)
			} else {
				if a.Kind != Number {
					return exit, nil
				}
				if in.Operator == Int32 || in.Operator == BitNot {
					n := ToUint32(math.Float64frombits(a.Bits))
					if in.Operator == BitNot {
						n = ^n
					}
					a = Float(float64(int32(n)))
				}
				if in.Operator == Neg {
					a.Bits ^= 1 << 63
				}
				slots[in.Dest] = a
			}
		case Update:
			old := read(in.Left)
			v, ok := binary(in.Operator, old, Float(1))
			if !ok {
				return exit, nil
			}
			slots[in.Dest] = v
			if in.Extra >= 0 {
				if in.Postfix {
					v = old
				}
				slots[in.Extra] = v
			}
		case Jump:
			next = in.Target
		case Branch:
			var condition bool
			var ok bool
			if in.Operator == Truth {
				condition, ok = truth(read(in.Left))
			} else {
				var v Value
				v, ok = binary(in.Operator, read(in.Left), read(in.Right))
				condition = v.Bits != 0
			}
			if !ok {
				if in.Operator == Eq || in.Operator == Ne {
					exit.Kind = HostExit
				}
				return exit, nil
			}
			if condition == in.When {
				next = in.Target
			}
		case Return:
			v := read(in.Left)
			if v.Kind == Uninitialized {
				return exit, nil
			}
			exit.Kind, exit.Value, exit.Steps = Returned, v, steps+1
			return exit, nil
		default:
			return Exit{}, ErrState
		}
		steps++
		pc = next
	}
}

func binary(op Operator, a, b Value) (Value, bool) {
	if a.Kind != Number || b.Kind != Number {
		return Value{}, false
	}
	x, y := math.Float64frombits(a.Bits), math.Float64frombits(b.Bits)
	if op >= BitAnd && op <= UShr {
		l, r := ToUint32(x), ToUint32(y)
		switch op {
		case BitAnd:
			l &= r
		case BitOr:
			l |= r
		case BitXor:
			l ^= r
		case Shl:
			l <<= r & 31
		case Shr:
			l = uint32(int32(l) >> (r & 31))
		case UShr:
			return Float(float64(l >> (r & 31))), true
		}
		return Float(float64(int32(l))), true
	}
	switch op {
	case Add:
		return Float(x + y), true
	case Sub:
		return Float(x - y), true
	case Mul:
		return Float(x * y), true
	case Div:
		return Float(x / y), true
	case Mod:
		return Float(math.Mod(x, y)), true
	case Lt:
		return Bool(x < y), true
	case Le:
		return Bool(x <= y), true
	case Gt:
		return Bool(x > y), true
	case Ge:
		return Bool(x >= y), true
	case Eq:
		return Bool(x == y), true
	case Ne:
		return Bool(x != y), true
	}
	return Value{}, false
}

// ToUint32 implements JavaScript's numeric truncation modulo 2^32. Native
// emitters use it for literals; the oracle uses an independent remainder-based
// algorithm to check the emitters' IEEE exponent/mantissa conversion paths.
func ToUint32(n float64) uint32 {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0
	}
	n = math.Mod(math.Trunc(n), 4294967296)
	if n < 0 {
		n += 4294967296
	}
	return uint32(n)
}

func truth(v Value) (bool, bool) {
	switch v.Kind {
	case Undefined, Null:
		return false, true
	case Boolean:
		return v.Bits != 0, true
	case Number:
		n := math.Float64frombits(v.Bits)
		return n != 0 && !math.IsNaN(n), true
	}
	return false, false
}
