//go:build linux || darwin || freebsd || netbsd || openbsd

package sysfs

import (
	"io/fs"
	"path"

	"golang.org/x/sys/unix"
)

// The operations os.Root lacks run as the system's *at calls on the parent
// directory, which the root opens: the last element, a single name, cannot
// lead out of it.

// inParent calls fn with a descriptor of p's parent directory and p's last
// element.
func (d *dirFS) inParent(p string, fn func(dirfd int, base string) error) Errno {
	dir, base := splitPath(p)
	if base == "." || base == ".." {
		dir, base = p, "."
	}
	f, err := d.root.Open(dir)
	if err != nil {
		return ToErrno(err)
	}
	defer f.Close()
	rc, err := f.SyscallConn()
	if err != nil {
		return ToErrno(err)
	}
	var opErr error
	if err := rc.Control(func(fd uintptr) { opErr = fn(int(fd), base) }); err != nil {
		return ToErrno(err)
	}
	return ToErrno(opErr)
}

func (d *dirFS) rename(from, to string) Errno {
	return d.inParent(from, func(ofd int, obase string) error {
		return d.inParent(to, func(nfd int, nbase string) error {
			return unix.Renameat(ofd, obase, nfd, nbase)
		})
	})
}

func (d *dirFS) link(from, to string) Errno {
	return d.inParent(from, func(ofd int, obase string) error {
		return d.inParent(to, func(nfd int, nbase string) error {
			return unix.Linkat(ofd, obase, nfd, nbase, 0)
		})
	})
}

func (d *dirFS) symlink(target, p string) Errno {
	return d.inParent(p, func(fd int, base string) error {
		return unix.Symlinkat(target, fd, base)
	})
}

func (d *dirFS) readlink(p string) (string, Errno) {
	var s string
	e := d.inParent(p, func(fd int, base string) error {
		for size := 256; ; size *= 2 {
			buf := make([]byte, size)
			n, err := unix.Readlinkat(fd, base, buf)
			if err != nil {
				return err
			}
			if n < size {
				s = string(buf[:n])
				return nil
			}
		}
	})
	return s, e
}

func (d *dirFS) utimes(p string, t Times, follow bool) Errno {
	for links := 0; ; links++ {
		fi, err := d.root.Lstat(p)
		if err != nil {
			return ToErrno(err)
		}
		if !follow || fi.Mode()&fs.ModeSymlink == 0 {
			st := StatOf(fi)
			atim, mtim := st.Atim, st.Mtim
			if t.SetAtim {
				atim = t.Atim
			}
			if t.SetMtim {
				mtim = t.Mtim
			}
			ts := []unix.Timespec{unix.NsecToTimespec(atim), unix.NsecToTimespec(mtim)}
			return d.inParent(p, func(fd int, base string) error {
				return unix.UtimesNanoAt(fd, base, ts, unix.AT_SYMLINK_NOFOLLOW)
			})
		}
		if links == maxSymlinks {
			return ELOOP
		}
		target, e := d.readlink(p)
		if e != 0 {
			return e
		}
		if path.IsAbs(target) {
			return EPERM
		}
		// The root resolves the ".." of the joined path, from the link's
		// directory as it is.
		dir, _ := splitPath(p)
		p = tidy(dir + "/" + target)
	}
}
