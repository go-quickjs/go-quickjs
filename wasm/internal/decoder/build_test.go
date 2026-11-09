package decoder

import "bytes"

// A tiny module builder for tests: sections as byte slices, the code
// section from function bodies.

func uleb(v uint64) []byte {
	var b []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
			continue
		}
		return append(b, c)
	}
}

func sleb(v int64) []byte {
	var b []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && c&0x40 == 0) || (v == -1 && c&0x40 != 0) {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

// cat concatenates byte slices and bytes.
func cat(parts ...any) []byte {
	var b []byte
	for _, p := range parts {
		switch p := p.(type) {
		case []byte:
			b = append(b, p...)
		case byte:
			b = append(b, p)
		case int:
			b = append(b, byte(p))
		case rune:
			b = append(b, byte(p))
		case string:
			b = append(b, uleb(uint64(len(p)))...)
			b = append(b, p...)
		default:
			panic("cat: unknown part")
		}
	}
	return b
}

// vec is a vector: its count, then its items.
func vec(items ...[]byte) []byte {
	b := uleb(uint64(len(items)))
	for _, it := range items {
		b = append(b, it...)
	}
	return b
}

// sec is a section with id and payload.
func sec(id byte, payload []byte) []byte {
	return cat(id, uleb(uint64(len(payload))), payload)
}

func wasmModule(sections ...[]byte) []byte {
	return cat([]byte{0, 'a', 's', 'm', 1, 0, 0, 0}, bytes.Join(sections, nil))
}

// functype is a function type of params and results, each a one-byte value
// type.
func functype(params, results []byte) []byte {
	return cat(0x60, uleb(uint64(len(params))), params, uleb(uint64(len(results))), results)
}

// body is a function body: local declarations, then code.
func body(locals []byte, code ...any) []byte {
	b := cat(locals, cat(code...))
	return cat(uleb(uint64(len(b))), b)
}

var noLocals = []byte{0}

// fnModule is a module whose type section holds types, with one function
// per body, all of type 0 unless funcTypes says otherwise, preceded by
// sections before (sections after the function section and before code)
// and followed by after.
type modSpec struct {
	types     [][]byte
	imports   [][]byte
	funcTypes []uint32
	tables    [][]byte
	memories  [][]byte
	tags      [][]byte
	globals   [][]byte
	exports   [][]byte
	start     []byte
	elems     [][]byte
	dataCount []byte
	bodies    [][]byte
	datas     [][]byte
	customs   [][]byte
}

func (s modSpec) bytes() []byte {
	var secs [][]byte
	if s.types != nil {
		secs = append(secs, sec(1, vec(s.types...)))
	}
	if s.imports != nil {
		secs = append(secs, sec(2, vec(s.imports...)))
	}
	if s.bodies != nil {
		var fs [][]byte
		for i := range s.bodies {
			t := uint32(0)
			if i < len(s.funcTypes) {
				t = s.funcTypes[i]
			}
			fs = append(fs, uleb(uint64(t)))
		}
		secs = append(secs, sec(3, vec(fs...)))
	}
	if s.tables != nil {
		secs = append(secs, sec(4, vec(s.tables...)))
	}
	if s.memories != nil {
		secs = append(secs, sec(5, vec(s.memories...)))
	}
	if s.tags != nil {
		secs = append(secs, sec(13, vec(s.tags...)))
	}
	if s.globals != nil {
		secs = append(secs, sec(6, vec(s.globals...)))
	}
	if s.exports != nil {
		secs = append(secs, sec(7, vec(s.exports...)))
	}
	if s.start != nil {
		secs = append(secs, sec(8, s.start))
	}
	if s.elems != nil {
		secs = append(secs, sec(9, vec(s.elems...)))
	}
	if s.dataCount != nil {
		secs = append(secs, sec(12, s.dataCount))
	}
	if s.bodies != nil {
		secs = append(secs, sec(10, vec(s.bodies...)))
	}
	if s.datas != nil {
		secs = append(secs, sec(11, vec(s.datas...)))
	}
	for _, c := range s.customs {
		secs = append(secs, sec(0, c))
	}
	return wasmModule(secs...)
}

// Value type encodings.
const (
	tI32       = 0x7f
	tI64       = 0x7e
	tF32       = 0x7d
	tF64       = 0x7c
	tV128      = 0x7b
	tFuncRef   = 0x70
	tExternRef = 0x6f
	tAnyRef    = 0x6e
	tEqRef     = 0x6d
	tI31Ref    = 0x6c
	tStructRef = 0x6b
	tArrayRef  = 0x6a
	tExnRef    = 0x69
	tNullRef   = 0x71
	tRef       = 0x64
	tRefNull   = 0x63
	tVoid      = 0x40
)

func bs(b ...byte) []byte { return b }

// voidFn is the type () -> ().
var voidFn = functype(nil, nil)

// oneFunc is a module of one function of type sig with the given body.
func oneFunc(sig []byte, b []byte) []byte {
	return modSpec{types: [][]byte{sig}, bodies: [][]byte{b}}.bytes()
}
