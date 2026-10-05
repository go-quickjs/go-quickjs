package quickjs

import (
	"iter"
	"reflect"
	"runtime"
	"strings"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// Go sequences as JavaScript iterators.
//
// A value of type iter.Seq[T] or iter.Seq2[K, V] -- a Go function's result,
// a field, an element -- is handed to script as an iterator: for-of, spread,
// destructuring and the iterator helpers take it, and it asks the sequence
// for each value only when script asks for it. An iter.Seq2's values are
// [key, value] arrays, as a Map's entries are.
//
// The sequence is pulled with iter.Pull, so it runs only while script waits
// for its next value, and stops -- its yield returns false -- when script
// stops early: a break, a return, a destructuring that takes fewer than it
// gives, or the iterator's return method. A sequence script drops without
// stopping is stopped after the garbage collector finds the iterator, on the
// runtime's goroutine, when the runtime next runs its jobs; one that is still
// running when the runtime closes is stopped by Close.

// seqKind reports whether t is iter.Seq or iter.Seq2 of some types.
func seqKind(t reflect.Type) (two, ok bool) {
	if t.Kind() != reflect.Func || t.PkgPath() != "iter" {
		return false, false
	}
	switch name := t.Name(); {
	case strings.HasPrefix(name, "Seq2["):
		return true, true
	case strings.HasPrefix(name, "Seq["):
		return false, true
	}
	return false, false
}

// seqIter is a sequence being pulled, as the host keeps it to stop it.
type seqIter struct {
	once sync.Once
	stop func()
}

// end stops the sequence, once, whoever asks first: script, the collector
// or Close.
func (s *seqIter) end() { s.once.Do(s.stop) }

// encodeSeq hands script an iterator over the sequence rv.
func encodeSeq(rt *vm.Runtime, rv reflect.Value, two bool) (vm.Value, error) {
	if rv.IsNil() {
		return vm.Null, nil
	}
	yieldType := rv.Type().In(0)
	pull := func(yield func([2]reflect.Value) bool) {
		y := reflect.MakeFunc(yieldType, func(args []reflect.Value) []reflect.Value {
			var kv [2]reflect.Value
			copy(kv[:], args)
			return []reflect.Value{reflect.ValueOf(yield(kv))}
		})
		rv.Call([]reflect.Value{y})
	}
	pnext, pstop := iter.Pull(pull)
	s := &seqIter{stop: pstop}
	host, _ := rt.Host.(*Runtime)

	next := func() (vm.Value, bool, error) {
		kv, ok := pnext()
		if !ok {
			return vm.Undefined, false, nil
		}
		k, err := encodeReflect(rt, kv[0])
		if err != nil {
			return vm.Undefined, false, rt.ThrowTypeError("%s", err.Error())
		}
		if !two {
			return k, true, nil
		}
		v, err := encodeReflect(rt, kv[1])
		if err != nil {
			return vm.Undefined, false, rt.ThrowTypeError("%s", err.Error())
		}
		return vm.Obj(rt.NewArray([]vm.Value{k, v})), true, nil
	}
	stop := func() {
		s.end()
		if host != nil {
			delete(host.seqs, s)
		}
	}
	it := rt.NewHostIterator(next, stop)
	if host != nil {
		host.keepSeq(s, it.Object())
	}
	return it, nil
}

// keepSeq records a sequence handed to script as the iterator o, so that
// Close can stop it, and has it stopped once o is collected.
func (r *Runtime) keepSeq(s *seqIter, o *vm.Object) {
	if r.closed {
		s.end()
		return
	}
	if r.seqs == nil {
		r.seqs = map[*seqIter]struct{}{}
		r.OnClose(func() {
			for s := range r.seqs {
				s.end()
			}
			r.seqs = nil
		})
	}
	r.seqs[s] = struct{}{}
	if r.seqDropped == nil {
		// One piece of work that never ends, and never keeps the runtime
		// busy, carries every dropped sequence's stop to the runtime's
		// goroutine.
		r.seqDropped = r.StartAsyncWork()
		r.seqDropped.Unref()
	}
	w := r.seqDropped
	runtime.AddCleanup(o, func(s *seqIter) {
		w.Post(func(err error) {
			s.end()
			if err == nil {
				delete(r.seqs, s)
			}
		})
	}, s)
}
