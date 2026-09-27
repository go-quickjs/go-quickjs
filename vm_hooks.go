package quickjs

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/internal/vmhook"
)

// node:vm, in stdlib, reaches what it needs beyond the public API through
// vmhook, which is filled in here.
func init() {
	vmhook.Contextify = func(realm, sandbox any) error {
		re := realm.(*Realm)
		sb, _ := sandbox.(Value)
		if re.rt.closed {
			return ErrClosed
		}
		if !sb.v.IsObject() {
			return errors.New("quickjs: a context's sandbox must be an object")
		}
		re.rt.rt.Contextify(re.realm, sb.v.Object())
		return nil
	}
	vmhook.Check = func(rt any, src string, opts vmhook.Options) error {
		r := rt.(*Runtime)
		if err := r.codeGenerationAllowed(); err != nil {
			return err
		}
		_, err := r.compileAt(src, opts.Filename, opts.LineOffset, opts.ColumnOffset)
		return err
	}
	vmhook.Run = func(rt, realm any, src string, opts vmhook.Options) (any, error) {
		r := rt.(*Runtime)
		var re *vm.Realm
		if realm != nil {
			re = realm.(*Realm).realm
		}
		return r.runNested(re, src, opts)
	}
}

// codeGenerationAllowed refuses node:vm where eval is refused.
func (r *Runtime) codeGenerationAllowed() error {
	if r.closed {
		return ErrClosed
	}
	if r.noCodeGeneration {
		return r.Throw(r.NewError("EvalError", "code generation from strings is disabled"))
	}
	return nil
}

// runNested runs a script from inside running code: in realm re, or the
// running one when re is nil, leaving the job queue for the code that called
// it to run, and under a timeout of its own. Running out of that time is an
// Error the caller can catch, as node:vm makes it; the host's own deadline
// running out is still the uncatchable interruption it always is.
func (r *Runtime) runNested(re *vm.Realm, src string, opts vmhook.Options) (Value, error) {
	if err := r.codeGenerationAllowed(); err != nil {
		return Value{}, err
	}
	fn, err := r.compileAt(src, opts.Filename, opts.LineOffset, opts.ColumnOffset)
	if err != nil {
		return Value{}, err
	}
	outer := r.rt.Context()
	var inner context.Context
	if opts.Timeout > 0 {
		base := outer
		if base == nil {
			base = context.Background()
		}
		var cancel context.CancelFunc
		inner, cancel = context.WithTimeout(base, opts.Timeout)
		defer cancel()
		r.rt.SetContext(inner)
		defer r.rt.SetContext(outer)
	}
	var v vm.Value
	if re == nil {
		v, err = r.rt.Run(fn)
	} else {
		v, err = r.rt.RunIn(re, fn)
	}
	if err != nil {
		if inner != nil && inner.Err() != nil && (outer == nil || outer.Err() == nil) {
			e := r.NewError("Error", fmt.Sprintf("Script execution timed out after %dms", opts.Timeout.Milliseconds()))
			if err := e.Set("code", "ERR_SCRIPT_EXECUTION_TIMEOUT"); err != nil {
				return Value{}, err
			}
			return Value{}, r.Throw(e)
		}
		return Value{}, r.wrapError(err)
	}
	return Value{v: v, rt: r.rt}, nil
}
