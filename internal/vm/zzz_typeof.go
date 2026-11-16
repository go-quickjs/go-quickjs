package vm

// typeof's tests -- typeof f === 'function', typeof x !== 'undefined',
// typeof v == 'object' -- ask the value's kind and make no string, as V8's
// TestTypeOf does (typeofKind, which the register interpreter's typeof
// tests and switch (typeof x) ask), rather than making the name and
// comparing that string with the constant.
//
// The test is the one Value.TypeOf answers: an object with
// [[IsHTMLDDA]] (Annex B's document.all) is "undefined", any other
// callable one "function", and null and every other object "object". Its
// name is the constant's: a constant that is no type's name -- typeof x
// === 'strnig' -- is a test that never holds, the value still evaluated.
// ==, != and their strict forms answer alike, the two sides strings.
//
// Its functions are generic, each instantiated with struct{} alone: Go lays a
// generic function's instantiations out after every function and closure of
// the package (docs/optimizing.md, "The layout lock"), where code written
// anywhere else moves the closures, or the functions after it, against the hot
// ones (AGENTS.md).

// The types' indices in typeofNames.
const (
	typeUndefined = iota
	typeObject
	typeBoolean
	typeNumber
	typeString
	typeSymbol
	typeBigInt
	typeFunction
)

// typeofKind is the index in typeofNames of typeof v, as Value.TypeOf
// answers it.
func typeofKind[_ any](v Value) int {
	switch v.Kind() {
	case KindNumber:
		return typeNumber
	case KindNull:
		return typeObject
	case KindBool:
		return typeBoolean
	case KindString:
		return typeString
	case KindSymbol:
		return typeSymbol
	case KindBigInt:
		return typeBigInt
	case KindObject:
		o := v.object()
		if o.flags&objHTMLDDA != 0 {
			return typeUndefined
		}
		if o.IsCallable() {
			return typeFunction
		}
		return typeObject
	}
	return typeUndefined
}

// typeofOfStr is r's string of the type of index i in typeofNames, as
// typeofString makes it.
func typeofOfStr[_ any](r *Runtime, i int) *String {
	if s := r.typeofStrs[i]; s != nil {
		return s
	}
	s := NewString(typeofNames[i])
	r.typeofStrs[i] = s
	return s
}
