//go:build !windows

package fixedbase

import (
	"errors"
	"runtime"
)

// LoadedBase says where Windows loads a program, which only Windows can.
func LoadedBase(path string) (uint64, error) {
	return 0, errors.New("where a program loads is asked of Windows, and this is " + runtime.GOOS)
}
