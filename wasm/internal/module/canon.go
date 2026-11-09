package module

import (
	"encoding/binary"
	"sync"
)

// CanonicalIndex names a type definition process-wide: the isorecursive
// canonicalization gives two type definitions the same index exactly when
// they are the same type, whatever modules and recursion groups they are
// defined in.
type CanonicalIndex uint32

// NoCanonical marks a canonical type without a supertype.
const NoCanonical = ^CanonicalIndex(0)

// CanonicalType is a canonical type definition. The heap types of the
// references in it are canonical indices.
type CanonicalType struct {
	Kind   TypeKind
	Final  bool
	Super  CanonicalIndex
	Depth  uint32
	Func   *FuncType
	Struct *StructType
	Array  *ArrayType
}

// The canonicalizer is shared by every module of the process. It only grows:
// a canonical index, once given, names its type for the life of the process.
var canon struct {
	sync.RWMutex
	groups map[string]CanonicalIndex
	types  []CanonicalType
}

// Canonical returns the canonical type definition of index i.
func Canonical(i CanonicalIndex) *CanonicalType {
	canon.RLock()
	t := &canon.types[i]
	canon.RUnlock()
	return t
}

// IsCanonicalSubtype reports whether canonical type sub is super or declares
// it, directly or not, as a supertype.
func IsCanonicalSubtype(sub, super CanonicalIndex) bool {
	if sub == super {
		return true
	}
	canon.RLock()
	defer canon.RUnlock()
	for i := canon.types[sub].Super; i != NoCanonical; i = canon.types[i].Super {
		if i == super {
			return true
		}
	}
	return false
}

// keyWriter encodes a recursion group so that two groups have the same key
// exactly when they define the same types: a reference into the group is
// written relative to its start, one out of it as its canonical index.
type keyWriter struct {
	b          []byte
	m          *Module
	start, end uint32
}

func (k *keyWriter) u32(v uint32) { k.b = binary.LittleEndian.AppendUint32(k.b, v) }

func (k *keyWriter) heap(h HeapType) {
	switch {
	case !h.IsIndex():
		k.b = append(k.b, 'a', h.Code())
	case uint32(h) >= k.start && uint32(h) < k.end:
		k.b = append(k.b, 'r')
		k.u32(uint32(h) - k.start)
	default:
		k.b = append(k.b, 'c')
		k.u32(uint32(k.m.Canonical[h]))
	}
}

func (k *keyWriter) value(t ValueType) {
	k.b = append(k.b, byte(t.Kind))
	if t.Kind == KindRef {
		if t.Nullable {
			k.b = append(k.b, 1)
		} else {
			k.b = append(k.b, 0)
		}
		k.heap(t.Heap)
	}
}

func (k *keyWriter) field(f FieldType) {
	k.value(f.Type)
	if f.Mutable {
		k.b = append(k.b, 1)
	} else {
		k.b = append(k.b, 0)
	}
}

// Canonicalize gives the recursion group of types [start, start+size) of m
// their canonical indices, recording them in m.Canonical. The types before
// the group must have theirs.
func Canonicalize(m *Module, start, size uint32) {
	k := keyWriter{m: m, start: start, end: start + size}
	k.u32(size)
	for i := start; i < start+size; i++ {
		t := &m.Types[i]
		k.b = append(k.b, byte(t.Kind))
		if t.Final {
			k.b = append(k.b, 1)
		} else {
			k.b = append(k.b, 0)
		}
		if t.Super == NoSuper {
			k.b = append(k.b, 'n')
		} else {
			k.heap(HeapType(t.Super))
		}
		switch t.Kind {
		case TypeFunc:
			k.u32(uint32(len(t.Func.Params)))
			for _, p := range t.Func.Params {
				k.value(p)
			}
			k.u32(uint32(len(t.Func.Results)))
			for _, r := range t.Func.Results {
				k.value(r)
			}
		case TypeStruct:
			k.u32(uint32(len(t.Struct.Fields)))
			for _, f := range t.Struct.Fields {
				k.field(f)
			}
		case TypeArray:
			k.field(t.Array.Elem)
		}
	}
	key := string(k.b)

	canon.Lock()
	defer canon.Unlock()
	if canon.groups == nil {
		canon.groups = make(map[string]CanonicalIndex)
	}
	base, ok := canon.groups[key]
	if !ok {
		base = CanonicalIndex(len(canon.types))
		canon.groups[key] = base
		for i := start; i < start+size; i++ {
			canon.types = append(canon.types, canonicalDef(m, &m.Types[i], start, base))
		}
	}
	for i := uint32(0); i < size; i++ {
		m.Canonical[start+i] = base + CanonicalIndex(i)
	}
}

// canonicalDef returns type definition t of m, whose recursion group starts
// at start and takes canonical indices from base, with its references
// rewritten to canonical indices.
func canonicalDef(m *Module, t *TypeDef, start uint32, base CanonicalIndex) CanonicalType {
	heap := func(h HeapType) HeapType {
		if !h.IsIndex() {
			return h
		}
		if uint32(h) >= start {
			return HeapType(base) + h - HeapType(start)
		}
		return HeapType(m.Canonical[h])
	}
	value := func(v ValueType) ValueType {
		if v.Kind == KindRef {
			v.Heap = heap(v.Heap)
		}
		return v
	}
	c := CanonicalType{Kind: t.Kind, Final: t.Final, Super: NoCanonical, Depth: t.Depth}
	if t.Super != NoSuper {
		c.Super = CanonicalIndex(heap(HeapType(t.Super)))
	}
	switch t.Kind {
	case TypeFunc:
		f := &FuncType{
			Params:  make([]ValueType, len(t.Func.Params)),
			Results: make([]ValueType, len(t.Func.Results)),
		}
		for i, p := range t.Func.Params {
			f.Params[i] = value(p)
		}
		for i, r := range t.Func.Results {
			f.Results[i] = value(r)
		}
		c.Func = f
	case TypeStruct:
		s := &StructType{Fields: make([]FieldType, len(t.Struct.Fields))}
		for i, fl := range t.Struct.Fields {
			s.Fields[i] = FieldType{Type: value(fl.Type), Mutable: fl.Mutable}
		}
		c.Struct = s
	case TypeArray:
		c.Array = &ArrayType{Elem: FieldType{Type: value(t.Array.Elem.Type), Mutable: t.Array.Elem.Mutable}}
	}
	return c
}
