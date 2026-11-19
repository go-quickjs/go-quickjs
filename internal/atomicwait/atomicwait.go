// Package atomicwait is where the agents of shared memory wait: JavaScript's
// Atomics.wait and Atomics.waitAsync, WebAssembly's memory.atomic.wait32 and
// wait64, and the notifies that wake them. Both engines wait in one table,
// keyed by the memory and the address waited on, so that a notify from
// either wakes the waiters of either on the same memory, longest waiting
// first, and counts them all.
//
// A memory is named by a comparable value its users agree on: the
// WebAssembly memory a buffer is a view of, for one, whichever runtime or
// engine the view is in. The table is the process's, and safe to use from
// any goroutine. It is in shards, each with a lock of its own, a slot's
// shard by its memory's identity and its address (shardOf), so that agents
// on different memories seldom wait for each other's lock: everything a
// wait or a notify does is of one slot, and so of one shard.
package atomicwait

import (
	"hash/maphash"
	"math/bits"
	"reflect"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// Result is how a wait ends.
type Result uint8

const (
	// OK is a waiter woken by a notify.
	OK Result = iota
	// NotEqual is a wait that did not begin: the memory did not hold the
	// value expected.
	NotEqual
	// TimedOut is a waiter whose time ran out, or that was stopped.
	TimedOut
)

// Waiter is an agent waiting: synchronously, woken by its channel's closing,
// or asynchronously, told by a call of deliver.
type Waiter struct {
	at   slot
	wake chan struct{}
	// deliver and timer are an asynchronous waiter's: what tells it how its
	// wait ended, and what does so when its time runs out.
	deliver func(Result)
	timer   *time.Timer
	// told records that the waiter has left the table: notified, out of
	// time, stopped or cancelled.
	told bool
}

// slot is what a waiter waits on.
type slot struct {
	mem  any
	addr uint64
}

// shard holds the waiters of its slots, in the order they began, under its
// lock: 128 bytes, so that no two shards share a cache line, nor the pair
// of lines a processor may fetch together.
type shard struct {
	sync.Mutex
	m map[slot][]*Waiter
	_ [128 - 8 - unsafe.Sizeof(uintptr(0))]byte
}

// table is the shards, shardsFor the processors': made once, at the
// package's start.
var table = make([]shard, shardsFor(runtime.NumCPU()))

// shardShift is how far shardOf shifts the hash down to a shard's index.
var shardShift = 64 - uint(bits.TrailingZeros(uint(len(table))))

// shardsFor is how many shards the table has on cpus processors: four
// for each, so that the slots waited on at once -- about one an agent --
// seldom share one, as a power of two, at least 64 and at most 1024.
func shardsFor(cpus int) int {
	n := 64
	for n < 4*cpus && n < 1024 {
		n *= 2
	}
	return n
}

// seed is the hash's, of the process, for a memory named by a value that
// is no pointer.
var seed = maphash.MakeSeed()

// shardOf is the shard of slot at: its memory's identity and its address
// mixed by a multiplication, whose top bits are the shard. A memory named
// by a pointer, as every one the engines name is, is the pointer itself,
// the interface's data word; one named by another value, by that value's
// hash, as equal values of a type the interface holds a copy of need not
// share their words.
func shardOf(at slot) *shard {
	var id uint64
	if t := reflect.TypeOf(at.mem); t != nil && (t.Kind() == reflect.Pointer || t.Kind() == reflect.UnsafePointer) {
		id = uint64(uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&at.mem))[1])) >> 3
	} else {
		id = maphash.Comparable(seed, at.mem)
	}
	return &table[(id^at.addr<<32^at.addr)*0x9E3779B97F4A7C15>>shardShift]
}

// add puts a waiter in its shard, whose lock is held.
func (t *shard) add(w *Waiter) {
	if t.m == nil {
		t.m = make(map[slot][]*Waiter)
	}
	t.m[w.at] = append(t.m[w.at], w)
}

// remove takes a waiter out of its shard, whose lock is held.
func (t *shard) remove(w *Waiter) {
	list := t.m[w.at]
	for i, x := range list {
		if x == w {
			if len(list) == 1 {
				delete(t.m, w.at)
			} else {
				t.m[w.at] = append(list[:i:i], list[i+1:]...)
			}
			return
		}
	}
}

// Wait waits at addr of mem while equal reports that the memory holds the
// value expected, until a notify wakes the waiter, timeout passes -- never,
// if it is negative -- or stop or abort is closed, either of which may be
// nil. equal is called with the slot's shard locked, so that no notify
// comes between the look and the wait; it must take no lock of its own.
func Wait(mem any, addr uint64, equal func() bool, timeout time.Duration, stop, abort <-chan struct{}) Result {
	at := slot{mem, addr}
	t := shardOf(at)
	t.Lock()
	if !equal() {
		t.Unlock()
		return NotEqual
	}
	if timeout == 0 {
		t.Unlock()
		return TimedOut
	}
	w := &Waiter{at: at, wake: make(chan struct{})}
	t.add(w)
	t.Unlock()

	var expired <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expired = t.C
	}
	select {
	case <-w.wake:
		return OK
	case <-expired:
	case <-stop:
	case <-abort:
	}
	// Out of time, or stopped: the waiter leaves the table -- unless a
	// notify took it out first, which then counts.
	t.Lock()
	defer t.Unlock()
	if w.told {
		return OK
	}
	w.told = true
	t.remove(w)
	return TimedOut
}

// WaitAsync begins an asynchronous wait at addr of mem. It answers at once,
// with a nil Waiter, when equal reports that the memory does not hold the
// value expected (NotEqual) or timeout is zero (TimedOut); otherwise it
// returns the waiter, and calls deliver later, once, from whichever
// goroutine notifies it (OK) or finds its time has run out (TimedOut) -- a
// negative timeout never does. equal is called with the slot's shard
// locked.
func WaitAsync(mem any, addr uint64, equal func() bool, timeout time.Duration, deliver func(Result)) (*Waiter, Result) {
	at := slot{mem, addr}
	t := shardOf(at)
	t.Lock()
	defer t.Unlock()
	if !equal() {
		return nil, NotEqual
	}
	if timeout == 0 {
		return nil, TimedOut
	}
	w := &Waiter{at: at, deliver: deliver}
	t.add(w)
	if timeout > 0 {
		w.timer = time.AfterFunc(timeout, func() {
			t.Lock()
			if w.told {
				t.Unlock()
				return
			}
			w.told = true
			t.remove(w)
			t.Unlock()
			deliver(TimedOut)
		})
	}
	return w, OK
}

// Cancel takes an asynchronous waiter out of the table, unless it has been
// told already, and tells it nothing.
func Cancel(w *Waiter) {
	t := shardOf(w.at)
	t.Lock()
	defer t.Unlock()
	if w.told {
		return
	}
	w.told = true
	if w.timer != nil {
		w.timer.Stop()
	}
	t.remove(w)
}

// Notify wakes up to count of the agents waiting at addr of mem, the longest
// waiting first, and returns how many it woke. An asynchronous waiter is told
// once the shard's lock is let go: telling it may take locks of the host's.
func Notify(mem any, addr uint64, count uint64) uint64 {
	at := slot{mem, addr}
	t := shardOf(at)
	t.Lock()
	list := t.m[at]
	n := uint64(len(list))
	if count < n {
		n = count
	}
	var tell []func(Result)
	for _, w := range list[:n] {
		w.told = true
		if w.deliver == nil {
			close(w.wake)
			continue
		}
		if w.timer != nil {
			w.timer.Stop()
		}
		tell = append(tell, w.deliver)
	}
	switch {
	case n == 0:
	case n == uint64(len(list)):
		delete(t.m, at)
	default:
		t.m[at] = list[n:]
	}
	t.Unlock()
	for _, d := range tell {
		d(OK)
	}
	return n
}
