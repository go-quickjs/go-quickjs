package quickjs

import (
	"context"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// Realm is a realm of a Runtime: a global object and a set of built-ins of its
// own, which code evaluated in it sees.
//
// A Runtime has the realm it was made with, which its own methods evaluate in,
// and may make more. They share everything else -- the call stack, the job
// queue, the module loader, the symbols -- so a value made in one realm can be
// handed to code in another, as an ordinary Value of the same Runtime. A
// function runs in the realm it was made in, whoever calls it: what it makes,
// what it throws and the global object it falls back to are its realm's. An
// array from another realm is not an instance of this realm's Array, as in a
// browser an array from another frame is not.
type Realm struct {
	rt    *Runtime
	realm *vm.Realm
}

// NewRealm makes a realm with its own global object and built-ins. The host's
// globals -- whatever Set added to the runtime's own realm -- are not in it
// until they are set on it too.
func (r *Runtime) NewRealm() (*Realm, error) {
	if r.closed {
		return nil, ErrClosed
	}
	return &Realm{rt: r, realm: r.rt.NewRealm()}, nil
}

// Global returns the realm's global object.
func (re *Realm) Global() Value {
	if re.rt.closed {
		return Value{}
	}
	return Value{v: vmObj(re.realm.Global()), rt: re.rt.rt}
}

// Eval runs src as a script of the realm, returning its completion value, as
// Runtime.Eval does for the runtime's own realm.
func (re *Realm) Eval(src string) (Value, error) {
	return re.EvalFileContext(context.Background(), "<eval>", src)
}

// EvalFileContext runs src as a script of the realm called name in stack
// traces, stopping when ctx is done, as Runtime.EvalFileContext does for the
// runtime's own realm.
func (re *Realm) EvalFileContext(ctx context.Context, name, src string) (Value, error) {
	return re.rt.evalIn(ctx, re.realm, name, src)
}

// Set defines a global of the realm, converting v as Runtime.Set does. A Go
// function becomes a function of this realm, so what it throws is this realm's
// errors.
func (re *Realm) Set(name string, v any) error {
	r := re.rt
	if r.closed {
		return ErrClosed
	}
	var err error
	r.rt.InRealm(re.realm, func() {
		var val vm.Value
		if val, err = r.encode(v); err == nil {
			err = r.wrapError(r.rt.DefineProp(re.realm.Global(), r.rt.Intern(name), val))
		}
	})
	return err
}
