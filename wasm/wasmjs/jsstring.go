package wasmjs

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// The JS String Builtins: the functions of "wasm:js-string" a module
// compiled with builtins: ["js-string"] imports from the host rather than
// from its imports object, and the string constants it imports as globals
// of the module importedStringConstants names.

// builtin is one of the JS String builtins.
type builtin uint8

const (
	noBuiltin builtin = iota
	bCast
	bTest
	bFromCharCode
	bFromCodePoint
	bCharCodeAt
	bCodePointAt
	bLength
	bConcat
	bSubstring
	bEquals
	bCompare
	bFromCharCodeArray
	bIntoCharCodeArray
)

var builtinNames = map[string]builtin{
	"cast":              bCast,
	"test":              bTest,
	"fromCharCode":      bFromCharCode,
	"fromCodePoint":     bFromCodePoint,
	"charCodeAt":        bCharCodeAt,
	"codePointAt":       bCodePointAt,
	"length":            bLength,
	"concat":            bConcat,
	"substring":         bSubstring,
	"equals":            bEquals,
	"compare":           bCompare,
	"fromCharCodeArray": bFromCharCodeArray,
	"intoCharCodeArray": bIntoCharCodeArray,
}

// builtinSigs holds each builtin's canonical signature, made once.
var builtinSigs = sync.OnceValue(func() map[builtin]module.CanonicalIndex {
	ext, refExt, i32 := module.ExternRef, module.Ref(module.HeapExtern), module.I32
	// (array (mut i16)), final, alone in its recursion group.
	m := &module.Module{
		Types: []module.TypeDef{
			{Kind: module.TypeArray, Array: &module.ArrayType{Elem: module.FieldType{Type: module.I16, Mutable: true}},
				Super: module.NoSuper, Final: true, RecStart: 0, RecSize: 1},
		},
		Canonical: make([]module.CanonicalIndex, 1),
	}
	module.Canonicalize(m, 0, 1)
	a16 := module.RefNull(module.HeapType(m.Canonical[0]))
	sig := func(params []module.ValueType, results ...module.ValueType) module.CanonicalIndex {
		mm := &module.Module{
			Types:     []module.TypeDef{{Kind: module.TypeFunc, Func: &module.FuncType{Params: params, Results: results}, Super: module.NoSuper, Final: true, RecSize: 1}},
			Canonical: make([]module.CanonicalIndex, 1),
		}
		// A reference to the array type names it by its canonical index,
		// as a type outside the group.
		mm.Types = append([]module.TypeDef{m.Types[0]}, mm.Types...)
		mm.Canonical = []module.CanonicalIndex{m.Canonical[0], 0}
		for i, p := range params {
			if p == a16 {
				params[i] = module.RefNull(module.HeapIndex(0))
			}
		}
		mm.Types[1].RecStart = 1
		module.Canonicalize(mm, 1, 1)
		return mm.Canonical[1]
	}
	return map[builtin]module.CanonicalIndex{
		bCast:              sig([]module.ValueType{ext}, refExt),
		bTest:              sig([]module.ValueType{ext}, i32),
		bFromCharCode:      sig([]module.ValueType{i32}, refExt),
		bFromCodePoint:     sig([]module.ValueType{i32}, refExt),
		bCharCodeAt:        sig([]module.ValueType{ext, i32}, i32),
		bCodePointAt:       sig([]module.ValueType{ext, i32}, i32),
		bLength:            sig([]module.ValueType{ext}, i32),
		bConcat:            sig([]module.ValueType{ext, ext}, refExt),
		bSubstring:         sig([]module.ValueType{ext, i32, i32}, refExt),
		bEquals:            sig([]module.ValueType{ext, ext}, i32),
		bCompare:           sig([]module.ValueType{ext, ext}, i32),
		bFromCharCodeArray: sig([]module.ValueType{a16, i32, i32}, refExt),
		bIntoCharCodeArray: sig([]module.ValueType{ext, a16, i32}, i32),
	}
})

// truncated is a name as an error message shows one: at most 50 bytes, the
// last three of a longer one "...".
func truncated(s string) string {
	if len(s) > 50 {
		return s[:47] + "..."
	}
	return s
}

// checkBuiltins checks the imports the compile-time imports provide: a
// string constant must be an immutable global of a reference to extern, and
// a builtin must be of its signature. It returns the error's message and
// offset, and whether there is none.
func (md *moduleData) checkBuiltins() (string, uint32, bool) {
	m := md.m
	for i, imp := range m.Imports {
		if md.ci.hasConstants && imp.Module == md.ci.constants {
			if imp.Kind != module.ExternGlobal || m.Globals[imp.Index].Type.Kind != module.KindRef ||
				m.Globals[imp.Index].Type.Heap != module.HeapExtern || m.Globals[imp.Index].Mutable {
				return fmt.Sprintf("String constant import #%d %q must be an immutable global subtyping externref",
					i, truncated(imp.Name)), imp.Offset, false
			}
		}
		if imp.Kind != module.ExternFunc || !md.ci.jsString || imp.Module != "wasm:js-string" {
			continue
		}
		b, ok := builtinNames[imp.Name]
		if !ok {
			continue
		}
		if m.Canonical[m.Functions[imp.Index].TypeIndex] != builtinSigs()[b] {
			return fmt.Sprintf("Imported builtin function \"wasm:js-string\" %q has incorrect signature", imp.Name),
				imp.Offset, false
		}
	}
	return "", 0, true
}

// resolveBuiltins records which imports are builtins.
func (md *moduleData) resolveBuiltins() {
	if !md.ci.jsString {
		return
	}
	md.builtins = make([]builtin, len(md.m.Imports))
	for i, imp := range md.m.Imports {
		if imp.Kind == module.ExternFunc && imp.Module == "wasm:js-string" {
			md.builtins[i] = builtinNames[imp.Name]
		}
	}
}

// runtimeTrap is a trap that is a RuntimeError of a message of its own.
func runtimeTrap(msg string) error { return &engine.Trap{Msg: msg} }

func illegalCast() error { return &engine.Trap{Code: engine.TrapIllegalCast} }

// stringOf returns the string an externref is, or nil.
func (a *api) stringOf(r engine.Ref) *vm.String {
	if h, ok := r.Host(); ok {
		if v, ok := h.(vm.Value); ok && v.IsString() {
			return v.String()
		}
	}
	return nil
}

// strRef is the externref of a string.
func strRef(s *vm.String) engine.Ref { return engine.HostRef(vm.Str(s)) }

// builtinFunction returns the host function of builtin b, for import index
// index.
func (a *api) builtinFunction(b builtin, index uint32) *engine.Function {
	c := builtinSigs()[b]
	fn := engine.NewHostFunction(c, "", func(ctx context.Context, caller *engine.Instance, args, results []engine.Val) error {
		return a.runBuiltin(b, args, results)
	})
	fn.Index = index
	return fn
}

// runBuiltin runs builtin b.
func (a *api) runBuiltin(b builtin, args, results []engine.Val) error {
	str := func(i int) (*vm.String, error) {
		if s := a.stringOf(args[i].R); s != nil {
			return s, nil
		}
		return nil, illegalCast()
	}
	u32 := func(i int) uint32 { return uint32(args[i].N) }
	switch b {
	case bCast:
		if _, err := str(0); err != nil {
			return err
		}
		results[0].R = args[0].R
	case bTest:
		if a.stringOf(args[0].R) != nil {
			results[0].N = 1
		}
	case bFromCharCode:
		results[0].R = strRef(vm.HostStringFromUnits([]uint16{uint16(u32(0))}))
	case bFromCodePoint:
		cp := u32(0)
		if cp > 0x10FFFF {
			return runtimeTrap(fmt.Sprintf("Invalid code point %d", cp))
		}
		if cp < 0x10000 {
			results[0].R = strRef(vm.HostStringFromUnits([]uint16{uint16(cp)}))
		} else {
			cp -= 0x10000
			results[0].R = strRef(vm.HostStringFromUnits([]uint16{uint16(0xD800 + cp>>10), uint16(0xDC00 + cp&0x3FF)}))
		}
	case bCharCodeAt, bCodePointAt:
		s, err := str(0)
		if err != nil {
			return err
		}
		i := u32(1)
		if uint64(i) >= uint64(s.Len()) {
			return runtimeTrap("string offset out of bounds")
		}
		if b == bCharCodeAt {
			results[0].N = uint64(uint32(s.CharCodeAt(int(i))))
		} else {
			results[0].N = uint64(uint32(s.CodePointAt(int(i))))
		}
	case bLength:
		s, err := str(0)
		if err != nil {
			return err
		}
		results[0].N = uint64(uint32(s.Len()))
	case bConcat:
		s, err := str(0)
		if err != nil {
			return err
		}
		t, err := str(1)
		if err != nil {
			return err
		}
		results[0].R = strRef(s.Concat(t))
	case bSubstring:
		s, err := str(0)
		if err != nil {
			return err
		}
		start, end, n := uint64(u32(1)), uint64(u32(2)), uint64(s.Len())
		end = min(end, n)
		if start > n || end < start {
			results[0].R = strRef(vm.NewString(""))
		} else {
			results[0].R = strRef(s.Substring(int(start), int(end)))
		}
	case bEquals:
		var x, y *vm.String
		if !args[0].R.IsNull() {
			s, err := str(0)
			if err != nil {
				return err
			}
			x = s
		}
		if !args[1].R.IsNull() {
			s, err := str(1)
			if err != nil {
				return err
			}
			y = s
		}
		if (x == nil && y == nil) || (x != nil && y != nil && x.Equals(y)) {
			results[0].N = 1
		}
	case bCompare:
		s, err := str(0)
		if err != nil {
			return err
		}
		t, err := str(1)
		if err != nil {
			return err
		}
		c := s.Compare(t)
		switch {
		case c < 0:
			results[0].N = uint64(uint32(0xFFFFFFFF))
		case c > 0:
			results[0].N = 1
		}
	case bFromCharCodeArray:
		if args[0].R.IsNull() {
			return &engine.Trap{Code: engine.TrapNullDereference}
		}
		elems, n, _ := engine.ArrayNumbers(args[0].R)
		start, end := uint64(u32(1)), uint64(u32(2))
		if start > end || end > uint64(n) {
			return &engine.Trap{Code: engine.TrapArrayOutOfBounds}
		}
		u := make([]uint16, end-start)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(elems[2*(int(start)+i):])
		}
		results[0].R = strRef(vm.HostStringFromUnits(u))
	case bIntoCharCodeArray:
		s, err := str(0)
		if err != nil {
			return err
		}
		if args[1].R.IsNull() {
			return &engine.Trap{Code: engine.TrapNullDereference}
		}
		elems, n, _ := engine.ArrayNumbers(args[1].R)
		start := uint64(u32(2))
		if start+uint64(s.Len()) > uint64(n) {
			return &engine.Trap{Code: engine.TrapArrayOutOfBounds}
		}
		for i := 0; i < s.Len(); i++ {
			binary.LittleEndian.PutUint16(elems[2*(int(start)+i):], uint16(s.CharCodeAt(i)))
		}
		results[0].N = uint64(uint32(s.Len()))
	}
	return nil
}
