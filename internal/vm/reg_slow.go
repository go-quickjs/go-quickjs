package vm

// The register interpreter's slow paths kept as closures: ToObject as a
// value (toObjectValue), delete of a name (deleteVar) and a template
// literal's parts joined (concatParts).
//
// Its functions are variables, each assigned a closure in init, laid
// out after every other closure of the package, so that no hot code
// moves for them (see AGENTS.md).

var (
	// toObjectValue is ToObject as a value, the base a delete of a
	// primitive's property deletes from.
	toObjectValue func(r *Runtime, v Value) (Value, error)

	// deleteVar is delete name. Deleting a binding only succeeds for a
	// configurable global property, which is why a var declaration cannot be
	// deleted.
	deleteVar func(r *Runtime, f *frame, cl *closure, name Atom) (Value, error)

	// concatParts is concat: a template literal's parts joined. Every part
	// is converted before any of them is joined: a toString may run user
	// code, and when it runs is fixed. A number is left as it is, having no
	// conversion anything can observe and no need of a string of its own.
	// The parts are the registers',
	// which are converted in place.
	concatParts func(r *Runtime, parts []Value) (Value, error)
)

func init() {

	toObjectValue = func(r *Runtime, v Value) (Value, error) {
		o, err := r.toObject(v)
		if err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	}

	deleteVar = func(r *Runtime, f *frame, cl *closure, name Atom) (Value, error) {
		// What a direct eval declared can be deleted again, unlike a var the
		// source named: the evaluated code could have declared it anywhere, so
		// nothing may rely on it being there.
		if deleteEvalVar(f.evalVars, name) {
			return True, nil
		}
		if r.globalLexProp(cl.scope(), name).ok() {
			// A lexical binding is not a property and cannot be removed.
			return False, nil
		}
		ok, err := r.deleteProp(cl.scope(), name, false)
		if err != nil {
			return Undefined, err
		}
		return Bool(ok), nil
	}

	concatParts = func(r *Runtime, parts []Value) (Value, error) {
		for i := range parts {
			if parts[i].IsString() || parts[i].IsNumber() {
				continue
			}
			s, err := r.toString(parts[i])
			if err != nil {
				return Undefined, err
			}
			parts[i] = Str(s)
		}
		// The strings alone may be too long, which is known before anything is
		// written out.
		total := 0
		for _, p := range parts {
			if p.IsString() && total <= maxStringLength {
				total += p.String().length
			}
		}
		if total > maxStringLength {
			return Undefined, r.throwStringLength()
		}
		out, err := r.joinParts(parts)
		if err != nil {
			return Undefined, err
		}
		if out.length > maxStringLength {
			return Undefined, r.throwStringLength()
		}
		return Str(out), nil
	}

}
