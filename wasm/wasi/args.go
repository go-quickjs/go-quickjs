package wasi

import "github.com/go-quickjs/go-quickjs/wasm/internal/sysfs"

func argsGet(c *call, p []uint64) sysfs.Errno {
	return c.putStrings(c.sc.Args, uint32(p[0]), uint32(p[1]))
}

func argsSizesGet(c *call, p []uint64) sysfs.Errno {
	return c.putSizes(c.sc.Args, uint32(p[0]), uint32(p[1]))
}

func environGet(c *call, p []uint64) sysfs.Errno {
	return c.putStrings(c.sc.Environ, uint32(p[0]), uint32(p[1]))
}

func environSizesGet(c *call, p []uint64) sysfs.Errno {
	return c.putSizes(c.sc.Environ, uint32(p[0]), uint32(p[1]))
}

// size returns the bytes ss take with a NUL after each.
func size(ss []string) uint64 {
	var n uint64
	for _, s := range ss {
		n += uint64(len(s)) + 1
	}
	return n
}

// putSizes writes the count of ss at countPtr and the bytes they take at
// sizePtr.
func (c *call) putSizes(ss []string, countPtr, sizePtr uint32) sysfs.Errno {
	n := size(ss)
	if n > 0xffffffff {
		return sysfs.EOVERFLOW
	}
	if !c.putU32(countPtr, uint32(len(ss))) || !c.putU32(sizePtr, uint32(n)) {
		return sysfs.EFAULT
	}
	return 0
}

// putStrings writes ss, each NUL-terminated, at buf, and a pointer to each
// at ptrs.
func (c *call) putStrings(ss []string, ptrs, buf uint32) sysfs.Errno {
	n := size(ss)
	if n > 0xffffffff {
		return sysfs.EOVERFLOW
	}
	pb, ok := c.array(ptrs, uint32(len(ss)), 4)
	if !ok {
		return sysfs.EFAULT
	}
	bb, ok := c.bytes(buf, uint32(n))
	if !ok {
		return sysfs.EFAULT
	}
	off := uint32(0)
	for i, s := range ss {
		le.PutUint32(pb[4*i:], buf+off)
		copy(bb[off:], s)
		off += uint32(len(s))
		bb[off] = 0
		off++
	}
	return 0
}
