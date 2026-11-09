package wasi

import (
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

	// An event of each subscription ready now; the clocks' delays, for
	// waiting if none is.
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
	delays := make([]int64, nsubs)
	minDelay := int64(math.MaxInt64)
	for i := uint32(0); i < nsubs; i++ {
		sub := subs[i*subscriptionSize : (i+1)*subscriptionSize]
		delays[i] = -1
		switch sub[8] {
		case eventClock:
			id, timeout, flags := le.Uint32(sub[16:]), le.Uint64(sub[24:]), le.Uint16(sub[40:])
			if flags&^subclockAbstime != 0 {
				emit(sub, sysfs.EINVAL, 0)
				continue
			}
			delay := int64(min(timeout, math.MaxInt64))
			if flags&subclockAbstime != 0 {
				now, errno := c.now(id)
				if errno != 0 {
					emit(sub, errno, 0)
					continue
				}
				delay -= now
			} else if id != clockRealtime && id != clockMonotonic {
				emit(sub, sysfs.EINVAL, 0)
				continue
			}
			delays[i] = max(delay, 0)
			minDelay = min(minDelay, delays[i])
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
			c.sc.Nanosleep(minDelay)
		}
		for i := uint32(0); i < nsubs; i++ {
			if delays[i] >= 0 && delays[i] <= minDelay {
				emit(subs[i*subscriptionSize:(i+1)*subscriptionSize], 0, 0)
			}
		}
	}
	c.putU32(neventsPtr, nevents)
	return 0
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
