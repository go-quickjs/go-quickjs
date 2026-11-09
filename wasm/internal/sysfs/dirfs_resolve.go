package sysfs

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Where the system has no *at calls, the operations os.Root lacks resolve
// their paths to host paths, following symbolic links within the root, and
// refuse a link that is absolute or leads out of it. A path changed between
// the resolution and the operation is not seen.

// resolve returns the host path of p, its symbolic links followed, the last
// element's only if followLast is true.
func (d *dirFS) resolve(p string, followLast bool) (string, Errno) {
	todo := strings.Split(p, "/")
	var done []string
	links := 0
	for len(todo) > 0 {
		e := todo[0]
		todo = todo[1:]
		switch e {
		case "", ".":
			continue
		case "..":
			if len(done) == 0 {
				return "", EPERM
			}
			done = done[:len(done)-1]
			continue
		}
		host := filepath.Join(append([]string{d.dir}, append(done, e)...)...)
		last := len(todo) == 0
		if last && !followLast {
			done = append(done, e)
			break
		}
		fi, err := os.Lstat(host)
		if err != nil {
			if last && os.IsNotExist(err) {
				done = append(done, e)
				break
			}
			return "", ToErrno(err)
		}
		if fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			if links++; links > maxSymlinks {
				return "", ELOOP
			}
			target, err := os.Readlink(host)
			if err != nil {
				return "", EPERM
			}
			if filepath.IsAbs(target) || filepath.VolumeName(target) != "" || strings.HasPrefix(target, `\`) {
				return "", EPERM
			}
			todo = append(strings.Split(filepath.ToSlash(target), "/"), todo...)
			continue
		}
		if !last && !fi.IsDir() {
			return "", ENOTDIR
		}
		done = append(done, e)
	}
	return filepath.Join(append([]string{d.dir}, done...)...), 0
}

func (d *dirFS) resolvedRename(from, to string) Errno {
	o, e := d.resolve(from, false)
	if e != 0 {
		return e
	}
	n, e := d.resolve(to, false)
	if e != 0 {
		return e
	}
	return ToErrno(os.Rename(o, n))
}

func (d *dirFS) resolvedLink(from, to string) Errno {
	o, e := d.resolve(from, false)
	if e != 0 {
		return e
	}
	n, e := d.resolve(to, false)
	if e != 0 {
		return e
	}
	return ToErrno(os.Link(o, n))
}

func (d *dirFS) resolvedSymlink(target, p string) Errno {
	h, e := d.resolve(p, false)
	if e != 0 {
		return e
	}
	if runtime.GOOS == "windows" {
		target = filepath.FromSlash(target)
	}
	return ToErrno(os.Symlink(target, h))
}

func (d *dirFS) resolvedReadlink(p string) (string, Errno) {
	h, e := d.resolve(p, false)
	if e != 0 {
		return "", e
	}
	s, err := os.Readlink(h)
	if err != nil {
		return "", ToErrno(err)
	}
	if runtime.GOOS == "windows" {
		s = filepath.ToSlash(s)
	}
	return s, 0
}

func (d *dirFS) resolvedUtimes(p string, t Times, follow bool) Errno {
	h, e := d.resolve(p, follow)
	if e != 0 {
		return e
	}
	fi, err := os.Lstat(h)
	if err != nil {
		return ToErrno(err)
	}
	var atime, mtime time.Time
	if t.SetAtim {
		atime = time.Unix(0, t.Atim)
	}
	if t.SetMtim {
		mtime = time.Unix(0, t.Mtim)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return lutimes(h, t)
	}
	return ToErrno(os.Chtimes(h, atime, mtime))
}
