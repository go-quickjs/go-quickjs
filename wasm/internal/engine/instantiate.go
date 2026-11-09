package engine

import (
	"context"
	"fmt"
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Extern is an imported or exported entity.
type Extern struct {
	Kind   module.ExternKind
	Func   *Function
	Table  *Table
	Memory *Memory
	Global *Global
	Tag    *Tag
}

// LinkError is an import that does not match what the module declares.
type LinkError struct{ Msg string }

func (e *LinkError) Error() string { return e.Msg }

// AllocError is a memory or table instantiation could not allocate.
type AllocError struct{ Msg string }

func (e *AllocError) Error() string { return e.Msg }

// importName names import i as link errors do.
func importName(m *module.Module, i int) string {
	imp := m.Imports[i]
	return fmt.Sprintf("Import #%d %q %q", i, imp.Module, imp.Name)
}

// checkImport checks that ext may be import i of module m.
func checkImport(m *module.Module, i int, ext Extern) error {
	imp := m.Imports[i]
	name := importName(m, i)
	if ext.Kind != imp.Kind {
		switch imp.Kind {
		case module.ExternFunc:
			return &LinkError{name + ": function import requires a callable"}
		case module.ExternTable:
			return &LinkError{name + ": table import requires a WebAssembly.Table"}
		case module.ExternMemory:
			return &LinkError{name + ": memory import must be a WebAssembly.Memory object"}
		case module.ExternGlobal:
			return &LinkError{name + ": global import must be a number, valid Wasm reference, or WebAssembly.Global object"}
		case module.ExternTag:
			return &LinkError{name + ": tag import requires a WebAssembly.Tag"}
		}
	}
	switch imp.Kind {
	case module.ExternFunc:
		want := m.Canonical[m.Functions[imp.Index].TypeIndex]
		if f := ext.Func; f.Type != want && !module.IsCanonicalSubtype(f.Type, want) {
			return &LinkError{name + ": imported function does not match the expected type"}
		}
	case module.ExternTable:
		decl := m.Tables[imp.Index]
		t := ext.Table
		if t.Is64 != decl.Limits.Is64 {
			return &LinkError{fmt.Sprintf("cannot import %s table as %s", addrType(t.Is64), addrType(decl.Limits.Is64))}
		}
		if t.Size() < decl.Limits.Initial {
			return &LinkError{fmt.Sprintf("table import %d is smaller than initial %d, got %d", imp.Index, decl.Limits.Initial, t.Size())}
		}
		if decl.Limits.HasMax {
			if !t.HasMax {
				return &LinkError{fmt.Sprintf("table import %d has no maximum length; required: %d", imp.Index, decl.Limits.Max)}
			}
			if t.Max > decl.Limits.Max {
				return &LinkError{fmt.Sprintf("table import %d has a larger maximum size %x than the module's declared maximum %d", imp.Index, t.Max, decl.Limits.Max)}
			}
		}
		if !module.EquivalentTypesAcross(t.Type, decl.Type, t.Module, m) {
			return &LinkError{name + ": imported table does not match the expected type"}
		}
	case module.ExternMemory:
		decl := m.Memories[imp.Index]
		mem := ext.Memory
		if mem.Is64 != decl.Limits.Is64 {
			return &LinkError{fmt.Sprintf("cannot import %s memory as %s", addrType(mem.Is64), addrType(decl.Limits.Is64))}
		}
		if mem.Pages() < decl.Limits.Initial {
			return &LinkError{fmt.Sprintf("%s: memory import has %d pages which is smaller than the declared initial of %d", name, mem.Pages(), decl.Limits.Initial)}
		}
		if decl.Limits.HasMax {
			if !mem.HasMax {
				// The maximum named is the imported memory's, which it lacks:
				// -1 as an unsigned 32-bit number.
				return &LinkError{fmt.Sprintf("%s: memory import has no maximum limit, expected at most %d", name, uint32(math.MaxUint32))}
			}
			if mem.Max > decl.Limits.Max {
				return &LinkError{fmt.Sprintf("%s: memory import has a larger maximum size %d than the module's declared maximum %d", name, mem.Max, decl.Limits.Max)}
			}
		}
		if mem.Shared != decl.Shared {
			return &LinkError{fmt.Sprintf("%s: mismatch in shared state of memory, declared = %d, imported = %d", name, b2i(decl.Shared), b2i(mem.Shared))}
		}
	case module.ExternGlobal:
		decl := m.Globals[imp.Index]
		g := ext.Global
		if g.Mutable != decl.Mutable {
			return &LinkError{name + ": imported global does not match the expected mutability"}
		}
		ok := false
		if decl.Mutable {
			ok = module.EquivalentTypesAcross(g.Type, decl.Type, g.Module, m)
		} else {
			ok = module.IsSubtype(g.Type, decl.Type, g.Module, m)
		}
		if !ok {
			return &LinkError{name + ": imported global does not match the expected type"}
		}
	case module.ExternTag:
		want := m.Canonical[m.Tags[imp.Index].TypeIndex]
		if ext.Tag.Type != want {
			return &LinkError{name + ": imported tag does not match the expected type"}
		}
	}
	return nil
}

func addrType(is64 bool) string {
	if is64 {
		return "i64"
	}
	return "i32"
}

// Instantiate instantiates code's module with imports, one for each of the
// module's imports in order, and runs its start function. An instance whose
// segments do not fit, or whose start function traps, is returned with the
// error, as what it already initialized is visible through its imports.
func Instantiate(ctx context.Context, code *Code, imports []Extern) (inst *Instance, err error) {
	// Evaluating a constant expression may trap -- an array too large --
	// before any instance is made.
	defer func() {
		if r := recover(); r != nil {
			tr, ok := r.(*Trap)
			if !ok {
				panic(r)
			}
			inst, err = nil, tr
		}
	}()
	return instantiate(ctx, code, imports)
}

func instantiate(ctx context.Context, code *Code, imports []Extern) (*Instance, error) {
	m := code.Module
	inst := &Instance{Module: m, Code: code}
	// Memories are linked first, then the other imports in order.
	for i, imp := range m.Imports {
		if imp.Kind == module.ExternMemory {
			if err := checkImport(m, i, imports[i]); err != nil {
				return nil, err
			}
		}
	}
	for i, imp := range m.Imports {
		if imp.Kind != module.ExternMemory {
			if err := checkImport(m, i, imports[i]); err != nil {
				return nil, err
			}
		}
	}
	inst.Funcs = make([]*Function, len(m.Functions))
	inst.Tables = make([]*Table, len(m.Tables))
	inst.Memories = make([]*Memory, len(m.Memories))
	inst.Globals = make([]*Global, len(m.Globals))
	inst.Tags = make([]*Tag, len(m.Tags))
	for i, imp := range m.Imports {
		ext := imports[i]
		switch imp.Kind {
		case module.ExternFunc:
			inst.Funcs[imp.Index] = ext.Func
		case module.ExternTable:
			inst.Tables[imp.Index] = ext.Table
		case module.ExternMemory:
			inst.Memories[imp.Index] = ext.Memory
		case module.ExternGlobal:
			inst.Globals[imp.Index] = ext.Global
		case module.ExternTag:
			inst.Tags[imp.Index] = ext.Tag
		}
	}
	for i := range m.Functions {
		if inst.Funcs[i] == nil {
			ti := m.Functions[i].TypeIndex
			c := m.Canonical[ti]
			inst.Funcs[i] = &Function{hdr: header{rtt: rttOf(c), kind: ObjFunc}, Type: c, Sig: module.Canonical(c).Func, Inst: inst, Index: uint32(i)}
		}
	}
	for i, mem := range m.Memories {
		if inst.Memories[i] != nil {
			continue
		}
		l := mem.Limits
		inst.Memories[i] = NewMemory(l.Initial, l.Max, l.HasMax, l.Is64, mem.Shared)
		if inst.Memories[i] == nil {
			return nil, &AllocError{"Out of memory: Cannot allocate Wasm memory for new instance"}
		}
	}
	for i := range m.Tags {
		if inst.Tags[i] == nil {
			c := m.Canonical[m.Tags[i].TypeIndex]
			inst.Tags[i] = &Tag{Type: c, Sig: module.Canonical(c).Func}
		}
	}
	for i, g := range m.Globals {
		if inst.Globals[i] != nil {
			continue
		}
		v := evalConst(inst, g.Init, g.Type)
		inst.Globals[i] = &Global{Num: v.N, Hi: v.Hi, Ref: v.R, Type: g.Type, Module: m, Mutable: g.Mutable}
	}
	for i, t := range m.Tables {
		if inst.Tables[i] != nil {
			continue
		}
		var init Ref
		if t.Init != nil {
			init = evalConst(inst, *t.Init, t.Type).R
		}
		l := t.Limits
		if l.Initial > MaxTableSize {
			return nil, &AllocError{fmt.Sprintf("table %d: initial size %d exceeds the maximum table size", i, l.Initial)}
		}
		inst.Tables[i] = NewTable(t.Type, m, l.Initial, l.Max, l.HasMax, l.Is64, init)
	}
	inst.elems = make([][]Ref, len(m.Elems))
	for i, seg := range m.Elems {
		if seg.Mode == module.ElemDeclarative {
			continue
		}
		refs := make([]Ref, len(seg.Elems))
		for j, e := range seg.Elems {
			refs[j] = evalConst(inst, e, seg.Type).R
		}
		inst.elems[i] = refs
	}
	inst.datas = make([][]byte, len(m.Datas))
	for i, d := range m.Datas {
		inst.datas[i] = m.Bytes[d.Init.Offset:d.Init.End():d.Init.End()]
	}
	// Active segments, elements before data.
	for i, seg := range m.Elems {
		if seg.Mode != module.ElemActive {
			continue
		}
		t := inst.Tables[seg.Table]
		off := evalConst(inst, seg.Offset, offsetType(t.Is64)).N
		if !t.Is64 {
			off = uint64(uint32(off))
		}
		refs := inst.elems[i]
		if off > t.Size() || uint64(len(refs)) > t.Size()-off {
			return inst, &Trap{Code: TrapTableOutOfBounds}
		}
		copy(t.Elems[off:], refs)
		inst.elems[i] = nil
	}
	for i := range m.Elems {
		if m.Elems[i].Mode == module.ElemDeclarative {
			inst.elems[i] = nil
		}
	}
	for i, seg := range m.Datas {
		if !seg.Active {
			continue
		}
		mem := inst.Memories[seg.Memory]
		off := evalConst(inst, seg.Offset, offsetType(mem.Is64)).N
		if !mem.Is64 {
			off = uint64(uint32(off))
		}
		data := inst.datas[i]
		b := mem.bytes()
		if off > uint64(len(b)) || uint64(len(data)) > uint64(len(b))-off {
			return inst, &Trap{Code: TrapMemOutOfBounds, Msg: fmt.Sprintf(
				"data segment %d is out of bounds (offset %d, length %d, memory size %d)", i, off, len(data), len(b))}
		}
		copy(b[off:], data)
		inst.datas[i] = nil
	}
	if m.HasStart {
		if _, err := Invoke(ctx, inst.Funcs[m.Start], nil); err != nil {
			return inst, err
		}
	}
	return inst, nil
}

func offsetType(is64 bool) module.ValueType {
	if is64 {
		return module.I64
	}
	return module.I32
}

// Export returns the entity export name of inst refers to.
func (inst *Instance) Export(name string) (Extern, bool) {
	for _, e := range inst.Module.Exports {
		if e.Name != name {
			continue
		}
		switch e.Kind {
		case module.ExternFunc:
			return Extern{Kind: e.Kind, Func: inst.Funcs[e.Index]}, true
		case module.ExternTable:
			return Extern{Kind: e.Kind, Table: inst.Tables[e.Index]}, true
		case module.ExternMemory:
			return Extern{Kind: e.Kind, Memory: inst.Memories[e.Index]}, true
		case module.ExternGlobal:
			return Extern{Kind: e.Kind, Global: inst.Globals[e.Index]}, true
		case module.ExternTag:
			return Extern{Kind: e.Kind, Tag: inst.Tags[e.Index]}, true
		}
	}
	return Extern{}, false
}

// evalConst evaluates constant expression e of type t in inst.
func evalConst(inst *Instance, e module.ConstExpr, t module.ValueType) Val {
	switch e.Kind {
	case module.ConstI32:
		return Val{N: uint64(e.Value)}
	case module.ConstRefFunc:
		return Val{R: FuncRef(inst.Funcs[e.Value])}
	case module.ConstRefNull:
		return Val{}
	}
	r := reader{b: inst.Module.Bytes, pc: int(e.Code.Offset)}
	var stack []Val
	pop := func() Val {
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		return v
	}
	for {
		op := r.opcode()
		switch op {
		case opcode.End:
			return stack[len(stack)-1]
		case opcode.I32Const:
			stack = append(stack, Val{N: uint64(uint32(r.s32()))})
		case opcode.I64Const:
			stack = append(stack, Val{N: uint64(r.s64())})
		case opcode.F32Const:
			stack = append(stack, Val{N: uint64(r.fixed32())})
		case opcode.F64Const:
			stack = append(stack, Val{N: r.fixed64()})
		case opcode.S128Const:
			lo := r.fixed64()
			stack = append(stack, Val{N: lo, Hi: r.fixed64()})
		case opcode.GlobalGet:
			g := inst.Globals[r.u32()]
			stack = append(stack, Val{N: g.Num, Hi: g.Hi, R: g.Ref})
		case opcode.RefNull:
			r.heapType()
			stack = append(stack, Val{})
		case opcode.RefFunc:
			stack = append(stack, Val{R: FuncRef(inst.Funcs[r.u32()])})
		case opcode.I32Add, opcode.I32Sub, opcode.I32Mul:
			b, a := uint32(pop().N), uint32(pop().N)
			var v uint32
			switch op {
			case opcode.I32Add:
				v = a + b
			case opcode.I32Sub:
				v = a - b
			default:
				v = a * b
			}
			stack = append(stack, Val{N: uint64(v)})
		case opcode.I64Add, opcode.I64Sub, opcode.I64Mul:
			b, a := pop().N, pop().N
			var v uint64
			switch op {
			case opcode.I64Add:
				v = a + b
			case opcode.I64Sub:
				v = a - b
			default:
				v = a * b
			}
			stack = append(stack, Val{N: v})
		default:
			v, ok := evalConstGC(inst, op, &r, &stack)
			if !ok {
				panic("engine: unsupported constant instruction " + op.Name())
			}
			_ = v
		}
	}
}
