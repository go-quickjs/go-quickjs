package engine

import (
	"context"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// Val is a value of any type, where values cross between wasm and its host:
// a number in N (an i32 or f32 in its low 32 bits, a float as its bits), a
// vector in N and Hi, a reference in R.
type Val struct {
	N, Hi uint64
	R     Ref
}

// HostFunc is a function the host implements. It reads its arguments from
// args and writes its results to results, and returns an error to throw: a
// *Trap, an *Exception, or anything else, which the caller of the wasm code
// that called the function receives.
type HostFunc func(ctx context.Context, caller *Instance, args, results []Val) error

// Function is a function of an instance, or of the host.
type Function struct {
	hdr header
	// Type is the function's canonical type, Sig its signature with
	// canonical indices.
	Type module.CanonicalIndex
	Sig  *module.FuncType
	// Inst and Index are the defining instance and the function's index in
	// it; Inst is nil for a host function.
	Inst  *Instance
	Index uint32
	// Host is a host function's implementation.
	Host HostFunc
	// Name is the host's name for a host function, for stack traces.
	Name string
}

// NewHostFunction returns a host function of canonical type t.
func NewHostFunction(t module.CanonicalIndex, name string, fn HostFunc) *Function {
	return &Function{hdr: header{rtt: rttOf(t), kind: ObjFunc}, Type: t, Sig: module.Canonical(t).Func, Host: fn, Name: name}
}

// FuncRef returns the reference to f.
func FuncRef(f *Function) Ref {
	if f == nil {
		return Null
	}
	return Ref{p: unsafe.Pointer(f)}
}

// Func returns the function r refers to, or nil.
func (r Ref) Func() *Function {
	if r.p == nil || r.IsI31() || r.Kind() != ObjFunc {
		return nil
	}
	return (*Function)(r.p)
}

// Tag is an exception tag.
type Tag struct {
	Type module.CanonicalIndex
	Sig  *module.FuncType
}

// Exception is a thrown wasm exception: its tag and payload.
type Exception struct {
	hdr  header
	Tag  *Tag
	Vals []Val
	// Host is what the host threw, for an exception that came from it
	// through a JS tag, or nil.
	Host any
}

// Error returns a description of the exception.
func (e *Exception) Error() string { return "wasm exception" }

// ExnRef returns the reference to e.
func ExnRef(e *Exception) Ref { return Ref{p: unsafe.Pointer(e)} }

// Exn returns the exception r refers to, or nil.
func (r Ref) Exn() *Exception {
	if r.p == nil || r.IsI31() || r.Kind() != ObjExn {
		return nil
	}
	return (*Exception)(r.p)
}

// Instance is an instantiated module.
type Instance struct {
	Module *module.Module
	// Code is the module's compiled code, shared by its instances.
	Code     *Code
	Funcs    []*Function
	Tables   []*Table
	Memories []*Memory
	Globals  []*Global
	Tags     []*Tag
	// mem0 is Memories[0], or nil, which loads and stores of the first
	// memory read without indexing.
	mem0 *Memory

	// datas and elems are the data and element segments, nil once dropped.
	datas [][]byte
	elems [][]Ref

	// Name is the name the instance was registered under, for stack traces.
	Name string

	// jit is what the instance's native code reads of it.
	jit jitInstance
}
