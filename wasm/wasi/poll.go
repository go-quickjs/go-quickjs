package wasi

import (
	"context"
	"io"
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/internal/sysfs"
)

// The event types.
const (
	eventClock   = 0
	eventFdRead  = 1
	eventFdWrite = 2
)

// subclockAbstime is the subclockflags bit of an absolute timeout.
const subclockAbstime = 1

// The sizes of a subscription and of an event.
const (
	subscriptionSize = 48
	eventSize        = 32
)

func pollOneoff(c *call, p []uint64) sysfs.Errno {
	in, out, nsubs, neventsPtr := uint32(p[0]), uint32(p[1]), uint32(p[2]), uint32(p[3])
	if nsubs == 0 {
		return sysfs.EINVAL
	}
	subs, ok := c.array(in, nsubs, subscriptionSize)
	if !ok {
		return sysfs.EFAULT
	}
	events, ok := c.array(out, nsubs, eventSize)
	if !ok {
		return sysfs.EFAULT
	}
	if _, ok := c.bytes(neventsPtr, 4); !ok {
		return sysfs.EFAULT
	}

	// An event of each subscription ready now, and the earliest clock's
	// delay, for waiting if none is.
	nevents := uint32(0)
	emit := func(sub []byte, errno sysfs.Errno, nbytes uint64) {
		ev := events[nevents*eventSize : (nevents+1)*eventSize]
		clear(ev)
		copy(ev[0:8], sub[0:8])
		le.PutUint16(ev[8:], uint16(errno))
		ev[10] = sub[8]
		le.PutUint64(ev[16:], nbytes)
		nevents++
	}
	sc := subClocks{c: c}
	minDelay := int64(math.MaxInt64)
	for i := uint32(0); i < nsubs; i++ {
		sub := subs[i*subscriptionSize : (i+1)*subscriptionSize]
		switch sub[8] {
		case eventClock:
			delay, errno := sc.delay(sub)
			if errno != 0 {
				emit(sub, errno, 0)
				continue
			}
			minDelay = min(minDelay, delay)
		case eventFdRead, eventFdWrite:
			e, errno := c.file(uint64(le.Uint32(sub[16:])))
			if errno != 0 {
				emit(sub, errno, 0)
				continue
			}
			emit(sub, 0, readable(e, sub[8]))
		default:
			return sysfs.EINVAL
		}
	}
	if nevents == 0 {
		if minDelay > 0 {
			c.sleep(minDelay)
		}
		for i := uint32(0); i < nsubs; i++ {
			sub := subs[i*subscriptionSize : (i+1)*subscriptionSize]
			if sub[8] != eventClock {
				continue
			}
			if delay, errno := sc.delay(sub); errno == 0 && delay <= minDelay {
				emit(sub, 0, 0)
			}
		}
	}
	c.putU32(neventsPtr, nevents)
	return 0
}

// subClocks finds clock subscriptions' delays, reading each clock once for
// a call, so that a subscription looked at again has the delay it had: the
// subscriptions are read where they lie in memory, and the host keeps
// nothing of its own for each.
type subClocks struct {
	c    *call
	now  [2]int64 // the real-time and monotonic clocks' readings
	read [2]bool
}

// delay returns the nanoseconds until clock subscription sub is due, zero
// if it is, or the errno of its event if it is not valid.
func (sc *subClocks) delay(sub []byte) (int64, sysfs.Errno) {
	id, timeout, flags := le.Uint32(sub[16:]), le.Uint64(sub[24:]), le.Uint16(sub[40:])
	if flags&^subclockAbstime != 0 || id != clockRealtime && id != clockMonotonic {
		return 0, sysfs.EINVAL
	}
	delay := int64(min(timeout, math.MaxInt64))
	if flags&subclockAbstime != 0 {
		if !sc.read[id] {
			now, errno := sc.c.now(id)
			if errno != 0 {
				return 0, errno
			}
			sc.now[id], sc.read[id] = now, true
		}
		delay -= sc.now[id]
	}
	return max(delay, 0), 0
}

// sleep sleeps for ns nanoseconds, or until the call's context is done,
// which then ends the call as the engine's check of the context does,
// with the context's error. The engine's own sleepers return when the
// context is done and run on the call's goroutine. A host's runs on one
// of its own, which the call stops waiting for when the context is done,
// leaving the sleeper to return when it will, so that one that does not
// watch the context cannot hold the call; a panic in it is the call's for
// as long as the call waits.
func (c *call) sleep(ns int64) {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	done := ctx.Done()
	if done == nil || c.sc.OwnSleep {
		c.sc.Nanosleep(ctx, ns)
		if err := ctx.Err(); err != nil {
			panic(err)
		}
		return
	}
	if err := ctx.Err(); err != nil {
		panic(err)
	}
	slept := make(chan any, 1)
	go func() {
		defer func() { slept <- recover() }()
		c.sc.Nanosleep(ctx, ns)
	}()
	select {
	case p := <-slept:
		if p != nil {
			panic(p)
		}
	case <-done:
		panic(ctx.Err())
	}
}

// readable returns the bytes a read of e's file would find, for an event of
// type typ: those after its offset for a regular file, zero otherwise.
func readable(e *sysfs.Entry, typ byte) uint64 {
	if typ != eventFdRead || e.IsDir {
		return 0
	}
	st, errno := e.File.Stat()
	if errno != 0 || !st.Mode.IsRegular() {
		return 0
	}
	off, errno := e.File.Lseek(0, io.SeekCurrent)
	if errno != 0 || off >= st.Size {
		return 0
	}
	return uint64(st.Size - off)
}
