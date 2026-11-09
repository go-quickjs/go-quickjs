package sysfs

import (
	"io"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
)

// osFile is a File of the host's: a file of a mounted directory.
type osFile struct {
	f           *os.File
	read, write bool
	isDir       bool
	append      atomic.Bool
	// appendMu keeps an append's seek to the end and its write together.
	appendMu sync.Mutex
}

func newOSFile(f *os.File, read, write bool) (*osFile, Errno) {
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, ToErrno(err)
	}
	return &osFile{f: f, read: read, write: write, isDir: fi.IsDir()}, 0
}

func (o *osFile) Read(p []byte) (int, Errno) {
	switch {
	case o.isDir:
		return 0, EISDIR
	case !o.read:
		return 0, EBADF
	case len(p) == 0:
		return 0, 0
	}
	n, err := o.f.Read(p)
	return n, ToErrno(err)
}

func (o *osFile) Pread(p []byte, off int64) (int, Errno) {
	switch {
	case o.isDir:
		return 0, EISDIR
	case !o.read:
		return 0, EBADF
	case off < 0:
		return 0, EINVAL
	case len(p) == 0:
		return 0, 0
	}
	n, err := o.f.ReadAt(p, off)
	return n, ToErrno(err)
}

func (o *osFile) Write(p []byte) (int, Errno) {
	if !o.write || o.isDir {
		return 0, EBADF
	}
	if o.append.Load() {
		o.appendMu.Lock()
		defer o.appendMu.Unlock()
		if _, err := o.f.Seek(0, io.SeekEnd); err != nil {
			return 0, ToErrno(err)
		}
	}
	if len(p) == 0 {
		return 0, 0
	}
	n, err := o.f.Write(p)
	return n, ToErrno(err)
}

func (o *osFile) Pwrite(p []byte, off int64) (int, Errno) {
	switch {
	case !o.write || o.isDir:
		return 0, EBADF
	case off < 0:
		return 0, EINVAL
	case len(p) == 0:
		return 0, 0
	}
	n, err := o.f.WriteAt(p, off)
	return n, ToErrno(err)
}

func (o *osFile) Lseek(off int64, whence int) (int64, Errno) {
	if whence < io.SeekStart || whence > io.SeekEnd {
		return 0, EINVAL
	}
	n, err := o.f.Seek(off, whence)
	if err != nil {
		return 0, ToErrno(err)
	}
	return n, 0
}

func (o *osFile) Stat() (Stat, Errno) {
	fi, err := o.f.Stat()
	if err != nil {
		return Stat{}, ToErrno(err)
	}
	st := StatOf(fi)
	fileIDs(o.f, &st)
	return st, 0
}

func (o *osFile) Readdir() ([]Dirent, Errno) {
	if !o.isDir {
		return nil, ENOTDIR
	}
	if ds, ok := readdirIDs(o.f); ok {
		return ds, 0
	}
	if _, err := o.f.Seek(0, io.SeekStart); err != nil {
		return nil, ToErrno(err)
	}
	fis, err := o.f.Readdir(-1)
	if err != nil && err != io.EOF {
		return nil, ToErrno(err)
	}
	ds := make([]Dirent, len(fis))
	for i, fi := range fis {
		ds[i] = direntOf(fi)
	}
	return ds, 0
}

// direntOf returns the directory entry fi describes.
func direntOf(fi fs.FileInfo) Dirent {
	st := StatOf(fi)
	return Dirent{Name: fi.Name(), Ino: st.Ino, Type: fi.Mode().Type()}
}

func (o *osFile) Truncate(size int64) Errno {
	switch {
	case o.isDir:
		return EISDIR
	case !o.write:
		return EBADF
	case size < 0:
		return EINVAL
	}
	return ToErrno(o.f.Truncate(size))
}

func (o *osFile) Sync() Errno {
	if !o.write || o.isDir {
		// Nothing was written through the file.
		return 0
	}
	return ToErrno(o.f.Sync())
}

func (o *osFile) SetAppend(on bool) Errno {
	o.append.Store(on)
	return 0
}

func (o *osFile) Close() Errno { return ToErrno(o.f.Close()) }
