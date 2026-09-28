// Package structclone is structured cloning -- what structuredClone and
// postMessage do to a value -- for hosts: a value is serialized in one
// runtime into a form that belongs to none, and deserialized in the same one
// or another, on another goroutine.
//
// The quickjs package fills the functions in; runtimes and values are
// *quickjs.Runtime and quickjs.Value, passed as any so that this package need
// not import it.
package structclone

// DataCloneError is a value that cannot be cloned, with V8's message for it,
// which a host throws as a DOMException named DataCloneError.
type DataCloneError struct{ Message string }

func (e *DataCloneError) Error() string { return e.Message }

// Codec is how a host clones and transfers objects of its own.
type Codec struct {
	// Serialize returns a token for a host object that can be cloned, and
	// whether v is one. It is asked about errors and about objects whose
	// prototype is not Object.prototype, never about an object literal.
	Serialize func(v any) (token any, ok bool, err error)
	// Transfer returns a token for a host object in the transfer list, and
	// whether v is one that can be transferred. It is asked about each entry
	// once, and turns down one listed twice.
	Transfer func(v any) (token any, ok bool, err error)
	// Revive makes, in the runtime deserializing, the object a token stands
	// for.
	Revive func(token any) (any, error)
}

// Brand is how a host object clones, which a host sets on it when it makes
// it: its prototype, which a script may change, cannot say.
type Brand int

const (
	// Host objects are asked of the codec's Serialize, whatever their
	// prototype, and are not cloned if it declines them.
	Host Brand = iota + 1
	// Opaque objects clone as empty objects: their state is not in their
	// properties.
	Opaque
	// Unsupported objects cannot be cloned.
	Unsupported
	// NeedsTransfer objects can only be transferred.
	NeedsTransfer
)

var (
	// SetBrand brands v, an ordinary object, with how it clones. It reports
	// false for anything else.
	SetBrand func(v any, b Brand) bool

	// Serialize serializes v in rt, transferring what transfer lists: an
	// ArrayBuffer is detached, its bytes moving with the result. A value that
	// cannot be cloned is a *DataCloneError; an exception a getter throws is
	// the *quickjs.Error it is.
	Serialize func(rt, v any, transfer []any, codec *Codec) (any, error)
	// Deserialize makes in rt the value that Serialize returned, which is
	// deserialized once.
	Deserialize func(rt, data any, codec *Codec) (any, error)
)
