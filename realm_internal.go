package quickjs

import (
	"github.com/go-quickjs/go-quickjs/internal/realmhook"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// A second realm is not public API yet; the conformance runner reaches one
// through realmhook, which is filled in here.
func init() {
	realmhook.NewRealm = func(rt any) (realmhook.Realm, error) {
		r := rt.(*Runtime)
		if r.closed {
			return nil, ErrClosed
		}
		return &hookRealm{rt: r, realm: r.rt.NewRealm()}, nil
	}
}

// hookRealm is a realm the conformance runner made.
type hookRealm struct {
	rt    *Runtime
	realm *vm.Realm
}

func (h *hookRealm) Global() any {
	return Value{v: vmObj(h.realm.Global()), rt: h.rt.rt}
}

func (h *hookRealm) Eval(src string) (result any, err error) {
	r := h.rt
	if r.closed {
		return Value{}, ErrClosed
	}
	defer r.guard(&err)
	fn, err := r.compile(src, "<eval>")
	if err != nil {
		return Value{}, err
	}
	v, err := r.rt.RunIn(h.realm, fn)
	if err != nil {
		return Value{}, r.wrapError(err)
	}
	if err := r.rt.DrainJobs(); err != nil {
		return Value{}, r.wrapError(err)
	}
	return Value{v: v, rt: r.rt}, nil
}

func (h *hookRealm) Set(name string, v any) error {
	r := h.rt
	if r.closed {
		return ErrClosed
	}
	var err error
	r.rt.InRealm(h.realm, func() {
		var val vm.Value
		if val, err = r.encode(v); err == nil {
			err = r.wrapError(r.rt.DefineProp(h.realm.Global(), r.rt.Intern(name), val))
		}
	})
	return err
}
