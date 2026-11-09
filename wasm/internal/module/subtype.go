package module

// Subtyping follows V8's wasm-subtyping.cc: the abstract hierarchies
//
//	any > eq > {i31, struct, array} > none,  struct > (struct types) > none
//	func > (function types) > nofunc
//	extern > noextern
//	exn > noexn
//
// and, between type definitions, the declared supertype chains compared by
// canonical index, so that types of different recursion groups or modules
// that are the same type are related as one.

// upcast returns the abstract heap type a heap type is a subtype of
// directly: struct, array or func for an index, the heap type itself
// otherwise.
func upcast(h HeapType, m *Module) HeapType {
	if !h.IsIndex() {
		return h
	}
	switch m.Types[h].Kind {
	case TypeStruct:
		return HeapStruct
	case TypeArray:
		return HeapArray
	}
	return HeapFunc
}

// abstractSubtype reports whether abstract heap type sub is a subtype of
// abstract heap type super.
func abstractSubtype(sub, super HeapType) bool {
	if sub == super || sub == HeapBottom {
		return true
	}
	switch sub {
	case HeapEq:
		return super == HeapAny
	case HeapI31, HeapStruct, HeapArray:
		return super == HeapEq || super == HeapAny
	case HeapNone:
		return super == HeapAny || super == HeapEq || super == HeapI31 ||
			super == HeapStruct || super == HeapArray
	case HeapNoFunc:
		return super == HeapFunc
	case HeapNoExtern:
		return super == HeapExtern
	case HeapNoExn:
		return super == HeapExn
	}
	return false
}

// isNullKind reports whether h is a bottom of one of the hierarchies.
func isNullKind(h HeapType) bool {
	return h == HeapNone || h == HeapNoFunc || h == HeapNoExtern || h == HeapNoExn
}

// IsHeapSubtype reports whether heap type sub of module subMod is a subtype of
// heap type super of module superMod.
func IsHeapSubtype(sub, super HeapType, subMod, superMod *Module) bool {
	if sub == super && subMod == superMod {
		return true
	}
	if sub == HeapBottom {
		return true
	}
	if super.IsIndex() {
		if !sub.IsIndex() {
			// Only a none type is below a type definition, and only the
			// none of the definition's own hierarchy.
			if !isNullKind(sub) {
				return false
			}
			return abstractSubtype(sub, upcast(super, superMod))
		}
		want := superMod.Canonical[super]
		i := uint32(sub)
		for {
			if subMod.Canonical[i] == want {
				return true
			}
			i = subMod.Types[i].Super
			if i == NoSuper {
				return false
			}
		}
	}
	return abstractSubtype(upcast(sub, subMod), super)
}

// IsSubtype reports whether value type sub of module subMod is a subtype of
// value type super of module superMod.
func IsSubtype(sub, super ValueType, subMod, superMod *Module) bool {
	if super.Kind == KindTop {
		return true
	}
	if sub.Kind != KindRef {
		if sub.Kind == KindBottom {
			return true
		}
		return sub.Kind == super.Kind
	}
	if super.Kind != KindRef {
		return false
	}
	if sub.Nullable && !super.Nullable {
		return false
	}
	return IsHeapSubtype(sub.Heap, super.Heap, subMod, superMod)
}

// IsSubtypeOf reports whether sub is a subtype of super, both of module m.
func IsSubtypeOf(sub, super ValueType, m *Module) bool {
	if sub == super {
		return true
	}
	return IsSubtype(sub, super, m, m)
}

// IsHeapSubtypeOf reports whether heap type sub is a subtype of super, both of
// module m.
func IsHeapSubtypeOf(sub, super HeapType, m *Module) bool {
	return IsHeapSubtype(sub, super, m, m)
}

// EquivalentTypes reports whether t1 and t2 of module m are the same type.
func EquivalentTypes(t1, t2 ValueType, m *Module) bool {
	if t1 == t2 {
		return true
	}
	if !t1.HasIndex() || !t2.HasIndex() {
		return false
	}
	if t1.Nullable != t2.Nullable {
		return false
	}
	return m.Canonical[t1.Heap] == m.Canonical[t2.Heap]
}

// EquivalentTypesAcross reports whether t1 of m1 and t2 of m2 are the same
// type.
func EquivalentTypesAcross(t1, t2 ValueType, m1, m2 *Module) bool {
	if m1 == m2 {
		return EquivalentTypes(t1, t2, m1)
	}
	if !t1.HasIndex() || !t2.HasIndex() {
		return t1 == t2
	}
	if t1.Nullable != t2.Nullable {
		return false
	}
	return m1.Canonical[t1.Heap] == m2.Canonical[t2.Heap]
}

// ValidSubtypeDefinition reports whether type sub may declare super as its
// supertype: the same kind, super not final, and sub's structure a subtype of
// super's.
func ValidSubtypeDefinition(sub, super uint32, m *Module) bool {
	s, p := &m.Types[sub], &m.Types[super]
	if s.Kind != p.Kind || p.Final {
		return false
	}
	switch s.Kind {
	case TypeFunc:
		sf, pf := s.Func, p.Func
		if len(sf.Params) != len(pf.Params) || len(sf.Results) != len(pf.Results) {
			return false
		}
		for i := range sf.Params {
			if !IsSubtypeOf(pf.Params[i], sf.Params[i], m) {
				return false
			}
		}
		for i := range sf.Results {
			if !IsSubtypeOf(sf.Results[i], pf.Results[i], m) {
				return false
			}
		}
		return true
	case TypeStruct:
		ss, ps := s.Struct, p.Struct
		if len(ss.Fields) < len(ps.Fields) {
			return false
		}
		for i, pf := range ps.Fields {
			if !validFieldSubtype(ss.Fields[i], pf, m) {
				return false
			}
		}
		return true
	case TypeArray:
		return validFieldSubtype(s.Array.Elem, p.Array.Elem, m)
	}
	return false
}

// validFieldSubtype reports whether a field of type sub may refine a field of
// type super: equal mutability, and an equivalent type if mutable, a subtype
// if not.
func validFieldSubtype(sub, super FieldType, m *Module) bool {
	if sub.Mutable != super.Mutable {
		return false
	}
	if sub.Mutable {
		return EquivalentTypes(sub.Type, super.Type, m)
	}
	return IsSubtypeOf(sub.Type, super.Type, m)
}

// Union returns the least upper bound of two value types of m, as V8's Union
// computes it for the validator, and whether it exists. Numbers have one only
// with themselves; references have it within a hierarchy.
func Union(t1, t2 ValueType, m *Module) (ValueType, bool) {
	if t1.Kind == KindBottom {
		return t2, true
	}
	if t2.Kind == KindBottom {
		return t1, true
	}
	if t1.Kind != KindRef || t2.Kind != KindRef {
		if t1 == t2 {
			return t1, true
		}
		return Top, false
	}
	nullable := t1.Nullable || t2.Nullable
	h, ok := commonAncestor(t1.Heap, t2.Heap, m)
	if !ok {
		return Top, false
	}
	return RefMaybeNull(h, nullable), true
}

// topOf returns the top of h's hierarchy.
func topOf(h HeapType, m *Module) HeapType {
	switch upcast(h, m) {
	case HeapFunc, HeapNoFunc:
		return HeapFunc
	case HeapExtern, HeapNoExtern:
		return HeapExtern
	case HeapExn, HeapNoExn:
		return HeapExn
	}
	return HeapAny
}

// commonAncestor returns the least common supertype of two heap types of m.
func commonAncestor(h1, h2 HeapType, m *Module) (HeapType, bool) {
	if h1 == h2 {
		return h1, true
	}
	if topOf(h1, m) != topOf(h2, m) {
		return 0, false
	}
	if IsHeapSubtypeOf(h1, h2, m) {
		return h2, true
	}
	if IsHeapSubtypeOf(h2, h1, m) {
		return h1, true
	}
	if h1.IsIndex() && h2.IsIndex() {
		// Walk h1's chain of supertypes for the first one above h2.
		for i := m.Types[h1].Super; i != NoSuper; i = m.Types[i].Super {
			if IsHeapSubtypeOf(h2, HeapType(i), m) {
				return HeapType(i), true
			}
		}
	}
	a1, a2 := upcast(h1, m), upcast(h2, m)
	for _, c := range [...]HeapType{a1, HeapEq, HeapAny, HeapFunc, HeapExtern, HeapExn} {
		if abstractSubtype(a1, c) && abstractSubtype(a2, c) {
			return c, true
		}
	}
	return 0, false
}
