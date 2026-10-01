package quickjs

import (
	"errors"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// Structured cloning: what structuredClone and postMessage do to a value. A
// value is serialized in one runtime into a Serialized that belongs to none,
// handed to another goroutine, and deserialized in the same runtime or
// another. Its shared references and cycles come out as they went in, a
// transferred ArrayBuffer's bytes move with it, and a SharedArrayBuffer's
// memory is shared, so that workers given one see each other's writes.

// Serialized is a value cloned out of a runtime. It belongs to no runtime and
// may be handed to any goroutine. It is deserialized once: deserializing
// moves its buffers' bytes into the runtime it is deserialized in, so a value
// sent to many is serialized once and copied for each.
type Serialized struct {
	s    *vm.Serialized
	used atomic.Bool
}

// Copy is another of the same value, to be deserialized as this one can be.
// It is safe to call from any goroutine, but not once this one has been
// deserialized, which leaves nothing to copy.
func (s *Serialized) Copy() *Serialized {
	return &Serialized{s: s.s.Copy()}
}

// ErrDeserialized is what deserializing a Serialized a second time returns.
var ErrDeserialized = errors.New("quickjs: the value has been deserialized already")

// DataCloneError is a value that cannot be cloned -- a function, a symbol, an
// object the host branded unsupported -- with V8's message for it. A host
// throws it as a DOMException named DataCloneError, which is what a script
// expects of structuredClone and postMessage.
type DataCloneError struct{ Message string }

func (e *DataCloneError) Error() string { return e.Message }

// CloneOptions says how a value is serialized.
type CloneOptions struct {
	// Transfer lists ArrayBuffers to move rather than copy -- they are
	// detached here -- and host objects for the codec to transfer.
	Transfer []Value
	// Codec clones the host's own objects; nil for none.
	Codec *CloneCodec
}

// CloneCodec is how a host clones and transfers objects of its own, such as
// a MessagePort or a Blob: it turns them into tokens of its choosing when a
// value is serialized, and back into objects when it is deserialized.
type CloneCodec struct {
	// Serialize returns a token for a host object that can be cloned, and
	// whether v is one. It is asked about errors, about objects branded
	// CloneHost, and about objects whose prototype is not Object.prototype;
	// never about an object literal.
	Serialize func(v Value) (token any, ok bool, err error)
	// Transfer returns a token for a host object in the transfer list, and
	// whether v is one that can be transferred. It is asked about each entry
	// once, and turns down one listed twice.
	Transfer func(v Value) (token any, ok bool, err error)
	// Revive makes, in the runtime deserializing, the object a token stands
	// for.
	Revive func(token any) (Value, error)
}

// CloneBrand is how an object the host made clones, which its prototype --
// which a script may change -- cannot say.
type CloneBrand int

const (
	// CloneHost objects are asked of the codec's Serialize, whatever their
	// prototype, and are not cloned if it declines them.
	CloneHost CloneBrand = iota + 1
	// CloneOpaque objects clone as empty objects: their state is not in
	// their properties.
	CloneOpaque
	// CloneUnsupported objects cannot be cloned.
	CloneUnsupported
	// CloneTransferOnly objects can only be transferred, as a stream is.
	CloneTransferOnly
)

// SetCloneBrand brands v, an ordinary object, with how it clones, and
// reports whether it could: anything else -- a primitive, an array, a
// function, an object of a built-in class -- cannot be branded.
func (v Value) SetCloneBrand(b CloneBrand) bool {
	if v.rt == nil || b < CloneHost || b > CloneTransferOnly {
		return false
	}
	return v.rt.SetCloneBrand(v.v, vm.CloneBrand(b))
}

// Serialize clones v out of the runtime, transferring what opts lists. A
// value that cannot be cloned is a *DataCloneError, and nothing has been
// transferred; an exception a getter throws is the *Error it is.
func (r *Runtime) Serialize(v Value, opts *CloneOptions) (*Serialized, error) {
	if r.closed || r.rt == nil {
		return nil, ErrClosed
	}
	if v.rt != nil && v.rt != r.rt {
		return nil, errors.New("quickjs: a value of another runtime cannot be serialized here")
	}
	var transfer []vm.Value
	var codec *CloneCodec
	if opts != nil {
		transfer = make([]vm.Value, len(opts.Transfer))
		for i, t := range opts.Transfer {
			transfer[i] = r.vmValue(t)
		}
		codec = opts.Codec
	}
	s, err := r.rt.Serialize(r.vmValue(v), transfer, r.vmCodec(codec))
	if err != nil {
		return nil, r.cloneError(err)
	}
	return &Serialized{s: s}, nil
}

// Deserialize makes in the runtime the value s holds, with codec reviving the
// host's objects. A Serialized is deserialized once; another time is
// ErrDeserialized.
func (r *Runtime) Deserialize(s *Serialized, codec *CloneCodec) (Value, error) {
	if r.closed || r.rt == nil {
		return Value{}, ErrClosed
	}
	if s == nil || s.s == nil {
		return Value{}, errors.New("quickjs: not a serialized value")
	}
	if !s.used.CompareAndSwap(false, true) {
		return Value{}, ErrDeserialized
	}
	v, err := r.rt.Deserialize(s.s, r.vmCodec(codec))
	if err != nil {
		return Value{}, r.cloneError(err)
	}
	return Value{v: v, rt: r.rt}, nil
}

// vmValue is the engine's value for a Value, or undefined for one that is
// not of this runtime.
func (r *Runtime) vmValue(v Value) vm.Value {
	if v.rt == r.rt && v.rt != nil {
		return v.v
	}
	return vm.Undefined
}

// vmCodec is a host's codec as the engine calls it.
func (r *Runtime) vmCodec(c *CloneCodec) *vm.Codec {
	if c == nil {
		return nil
	}
	wrap := func(f func(Value) (any, bool, error)) func(vm.Value) (any, bool, error) {
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
// as this package's, an exception as an *Error.
func (r *Runtime) cloneError(err error) error {
	var dce *vm.DataCloneError
	if errors.As(err, &dce) {
		return &DataCloneError{Message: dce.Message}
	}
	return r.wrapError(err)
}
