package vm

// What a host package's objects need to be shared between runtimes: to be
// structured-cloned -- posted to a worker, or given to structuredClone --
// and, for memory several runtimes and goroutines see at once, to be the
// memory of a SharedArrayBuffer.
//
// The functions are package variables assigned in init, as host_hooks.go's
// are, so that adding them moves no code before them.

// HostCloneable is host data (HostObject) that structured serialization can
// copy: CloneToken returns what stands for the object in a serialized value,
// and false for an object that cannot be cloned, which is refused as any
// object with state of its own is.
type HostCloneable interface {
	CloneToken() (HostCloneToken, bool)
}

// HostCloneToken is what a HostCloneable object is serialized as. It may be
// deserialized in any runtime, on any goroutine, any number of times: Revive
// makes in r the object it stands for, or returns the error that refuses it
// -- a *DataCloneError for a runtime that cannot have one.
type HostCloneToken interface {
	Revive(r *Runtime) (Value, error)
}
