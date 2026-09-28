package quickjs

import (
	"errors"

	"github.com/go-quickjs/go-quickjs/internal/structclone"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// Values are cloned between runtimes through structclone, which is filled in
// here.
func init() {
	structclone.SetBrand = func(v any, b structclone.Brand) bool {
		val, ok := v.(Value)
		if !ok || val.rt == nil {
			return false
		}
		return val.rt.SetCloneBrand(val.v, vm.CloneBrand(b))
	}
	structclone.Serialize = func(rt, v any, transfer []any, codec *structclone.Codec) (any, error) {
		r := rt.(*Runtime)
		if r.closed {
			return nil, ErrClosed
		}
		list := make([]vm.Value, len(transfer))
		for i, t := range transfer {
			list[i] = r.vmValue(t)
		}
		data, err := r.rt.Serialize(r.vmValue(v), list, r.vmCodec(codec))
		if err != nil {
			return nil, r.cloneError(err)
		}
		return data, nil
	}
	structclone.Deserialize = func(rt, data any, codec *structclone.Codec) (any, error) {
		r := rt.(*Runtime)
		if r.closed {
			return Value{}, ErrClosed
		}
		s, ok := data.(*vm.Serialized)
		if !ok {
			return Value{}, errors.New("quickjs: not a serialized value")
		}
		v, err := r.rt.Deserialize(s, r.vmCodec(codec))
		if err != nil {
			return Value{}, r.cloneError(err)
		}
		return Value{v: v, rt: r.rt}, nil
	}
}

// vmValue is the engine's value for a quickjs.Value, or undefined for
// anything else.
func (r *Runtime) vmValue(v any) vm.Value {
	if val, ok := v.(Value); ok && val.rt != nil {
		return val.v
	}
	return vm.Undefined
}

// vmCodec is a host's codec as the engine calls it.
func (r *Runtime) vmCodec(c *structclone.Codec) *vm.Codec {
	if c == nil {
		return nil
	}
	wrap := func(f func(any) (any, bool, error)) func(vm.Value) (any, bool, error) {
		if f == nil {
			return nil
		}
		return func(v vm.Value) (any, bool, error) { return f(Value{v: v, rt: r.rt}) }
	}
	out := &vm.Codec{Serialize: wrap(c.Serialize), Transfer: wrap(c.Transfer)}
	if c.Revive != nil {
		out.Revive = func(token any) (vm.Value, error) {
			v, err := c.Revive(token)
			if err != nil {
				return vm.Undefined, err
			}
			return r.vmValue(v), nil
		}
	}
	return out
}

// cloneError is an error from cloning in the public form: a DataCloneError
// as structclone's, an exception as an *Error.
func (r *Runtime) cloneError(err error) error {
	var dce *vm.DataCloneError
	if errors.As(err, &dce) {
		return &structclone.DataCloneError{Message: dce.Message}
	}
	return r.wrapError(err)
}
