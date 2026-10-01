package quickjs

import (
	"context"
	"errors"

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
func (r *Runtime) NewRealm(opts ...RealmOption) (*Realm, error) {
	if r.closed {
		return nil, ErrClosed
	}
	var c realmConfig
	for _, o := range opts {
		o(&c)
	}
	if c.sandbox != nil && (c.sandbox.rt != r.rt || !c.sandbox.v.IsObject()) {
		return nil, errors.New("quickjs: a realm's sandbox must be an object of its runtime")
	}
	re := &Realm{rt: r, realm: r.rt.NewRealm()}
	if c.sandbox != nil {
		r.rt.Contextify(re.realm, c.sandbox.v.Object())
	}
	return re, nil
}

// RealmOption configures NewRealm.
type RealmOption func(*realmConfig)

type realmConfig struct {
	sandbox *Value
}

// WithSandbox makes the realm a context of sandbox, an object of the runtime,
// as node:vm's createContext does: the realm's global names are the
// sandbox's properties first, and then the realm's own built-ins, and what a
// script of the realm declares globally becomes a property of the sandbox.
func WithSandbox(sandbox Value) RealmOption {
	return func(c *realmConfig) { c.sandbox = &sandbox }
}

// RunProgram runs a compiled program as a script of the realm, as
// Runtime.RunProgram does in the runtime's own realm.
func (re *Realm) RunProgram(p *Program) (Value, error) {
	return re.RunProgramContext(context.Background(), p)
}

// RunProgramContext is RunProgram with cancellation. Called from inside a
// running script -- by a Go function the script called -- it leaves the job
// queue to that script, and a ctx of its own that ends stops this run alone:
// it returns an error wrapping ctx's, and the script runs on.
func (re *Realm) RunProgramContext(ctx context.Context, p *Program) (result Value, err error) {
	r := re.rt
	if r.closed {
		return Value{}, ErrClosed
	}
	if p == nil {
		return Value{}, errors.New("quickjs: RunProgram of a nil *Program")
	}
	defer r.guard(&err)
	fn, err := p.code(r)
	if err != nil {
		return Value{}, err
	}
	return r.runIn(ctx, re.realm, fn)
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
