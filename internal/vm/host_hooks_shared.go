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

// sharedHost is what a SharedMemory over memory a host owns keeps of the
// host's.
type sharedHost struct {
	// owner keeps the memory alive, and is what Atomics.wait and notify
	// wait and notify on, so that waiters on any view of the memory -- the
	// host's own included -- are one list.
	owner any
	// grow grows the memory to n bytes for SharedArrayBuffer.prototype.grow
	// in r, returning the error to throw if it cannot; HostSharedMemoryGrown
	// says how long it is now.
	grow func(r *Runtime, n int) error
}

var (
	// HostSharedMemory returns the memory of SharedArrayBuffers over span,
	// memory owner keeps that never moves, of which the first length bytes
	// are in use, growable up to max bytes by grow if max >= 0. The host
	// tells it of every growth, by grow or otherwise, with
	// HostSharedMemoryGrown; Runtime.NewSharedArrayBuffer makes its buffers,
	// in any runtime, which structured cloning shares as it shares any.
	HostSharedMemory func(span []byte, length int, owner any, max int64, grow func(r *Runtime, n int) error) *SharedMemory
	// HostSharedMemoryGrown records that the memory HostSharedMemory made
	// is n bytes long now. It is safe to call from any goroutine.
	HostSharedMemoryGrown func(m *SharedMemory, n int)
)

func init() {
	HostSharedMemory = func(span []byte, length int, owner any, max int64, grow func(r *Runtime, n int) error) *SharedMemory {
		m := &SharedMemory{mem: span, max: max, pinned: true, host: &sharedHost{owner: owner, grow: grow}}
		m.length.Store(int64(length))
		return m
	}
	HostSharedMemoryGrown = func(m *SharedMemory, n int) {
		// The length only grows, whichever of two growths is told first.
		for {
			old := m.length.Load()
			if int64(n) <= old || m.length.CompareAndSwap(old, int64(n)) {
				return
			}
		}
	}
}
