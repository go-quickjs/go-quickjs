package wasi

import (
	"context"
	"io"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/sysfs"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
)

// procExit closes the calling module with the exit code and ends the call
// with the *sys.ExitError of it.
func procExit(ctx context.Context, mod api.Module, stack []uint64) {
	code := uint32(stack[0])
	if mod != nil {
		_ = mod.CloseWithExitCode(ctx, code)
	}
	panic(sys.NewExitError(code))
}

func procRaise(*call, []uint64) sysfs.Errno { return sysfs.ENOSYS }

func schedYield(c *call, _ []uint64) sysfs.Errno {
	c.sc.Osyield()
	return 0
}

func randomGet(c *call, p []uint64) sysfs.Errno {
	b, ok := c.bytes(uint32(p[0]), uint32(p[1]))
	if !ok {
		return sysfs.EFAULT
	}
	if _, err := io.ReadFull(c.sc.RandSource, b); err != nil {
		return sysfs.EIO
	}
	return 0
}
