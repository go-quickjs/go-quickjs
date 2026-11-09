package wasi

import "github.com/go-quickjs/go-quickjs/wasm/internal/sysfs"

// There are no sockets: a descriptor is not open, or not a socket.

func sockAccept(c *call, p []uint64) sysfs.Errno { return notSocket(c, p[0]) }
func sockRecv(c *call, p []uint64) sysfs.Errno   { return notSocket(c, p[0]) }
func sockSend(c *call, p []uint64) sysfs.Errno   { return notSocket(c, p[0]) }

func sockShutdown(c *call, p []uint64) sysfs.Errno { return notSocket(c, p[0]) }

func notSocket(c *call, fd uint64) sysfs.Errno {
	if _, errno := c.file(fd); errno != 0 {
		return errno
	}
	return sysfs.ENOTSOCK
}
