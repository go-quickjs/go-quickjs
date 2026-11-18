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
// any goroutine.
package atomicwait

import (
	"sync"
	"time"
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

// table holds the waiters of every slot, in the order they began.
var table struct {
	sync.Mutex
	m map[slot][]*Waiter
}

// add puts a waiter in the table. The lock is held.
func add(w *Waiter) {
	if table.m == nil {
		table.m = make(map[slot][]*Waiter)
	}
	table.m[w.at] = append(table.m[w.at], w)
}

// remove takes a waiter out of the table. The lock is held.
func remove(w *Waiter) {
	list := table.m[w.at]
	for i, x := range list {
		if x == w {
			if len(list) == 1 {
				delete(table.m, w.at)
			} else {
				table.m[w.at] = append(list[:i:i], list[i+1:]...)
			}
			return
		}
	}
}

// Wait waits at addr of mem while equal reports that the memory holds the
// value expected, until a notify wakes the waiter, timeout passes -- never,
// if it is negative -- or stop or abort is closed, either of which may be
// nil. equal is called with the table locked, so that no notify comes
// between the look and the wait; it must take no lock of its own.
func Wait(mem any, addr uint64, equal func() bool, timeout time.Duration, stop, abort <-chan struct{}) Result {
	table.Lock()
	if !equal() {
		table.Unlock()
		return NotEqual
	}
	if timeout == 0 {
		table.Unlock()
		return TimedOut
	}
	w := &Waiter{at: slot{mem, addr}, wake: make(chan struct{})}
	add(w)
	table.Unlock()

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
	table.Lock()
	defer table.Unlock()
	if w.told {
		return OK
	}
	w.told = true
	remove(w)
	return TimedOut
}

// WaitAsync begins an asynchronous wait at addr of mem. It answers at once,
// with a nil Waiter, when equal reports that the memory does not hold the
// value expected (NotEqual) or timeout is zero (TimedOut); otherwise it
// returns the waiter, and calls deliver later, once, from whichever
// goroutine notifies it (OK) or finds its time has run out (TimedOut) -- a
// negative timeout never does. equal is called with the table locked.
func WaitAsync(mem any, addr uint64, equal func() bool, timeout time.Duration, deliver func(Result)) (*Waiter, Result) {
	table.Lock()
	defer table.Unlock()
	if !equal() {
		return nil, NotEqual
	}
	if timeout == 0 {
		return nil, TimedOut
	}
	w := &Waiter{at: slot{mem, addr}, deliver: deliver}
	add(w)
	if timeout > 0 {
		w.timer = time.AfterFunc(timeout, func() {
			table.Lock()
			if w.told {
				table.Unlock()
				return
			}
			w.told = true
			remove(w)
			table.Unlock()
			deliver(TimedOut)
		})
	}
	return w, OK
}

// Cancel takes an asynchronous waiter out of the table, unless it has been
// told already, and tells it nothing.
func Cancel(w *Waiter) {
	table.Lock()
	defer table.Unlock()
	if w.told {
		return
	}
	w.told = true
	if w.timer != nil {
		w.timer.Stop()
	}
	remove(w)
}

// Notify wakes up to count of the agents waiting at addr of mem, the longest
// waiting first, and returns how many it woke. An asynchronous waiter is told
// once the table's lock is let go: telling it may take locks of the host's.
func Notify(mem any, addr uint64, count uint64) uint64 {
	at := slot{mem, addr}
	table.Lock()
	list := table.m[at]
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
		delete(table.m, at)
	default:
		table.m[at] = list[n:]
	}
	table.Unlock()
	for _, d := range tell {
		d(OK)
	}
	return n
}
