package sysfs

import (
	"io"
	"io/fs"
	"sync"
)

// readFS is an fs.FS mounted read-only. It has no symbolic links.
type readFS struct {
	fsys fs.FS
}

// AdaptFS mounts fsys, read-only.
func AdaptFS(fsys fs.FS) FS { return &readFS{fsys: fsys} }

func (r *readFS) Close() error { return nil }

func (r *readFS) OpenFile(p string, fl OpenFlags) (File, Errno) {
	if fl.Write || fl.Create || fl.Trunc {
		return nil, EROFS
	}
	if !fs.ValidPath(p) {
		return nil, ENOENT
	}
	f, err := r.fsys.Open(p)
	if err != nil {
		return nil, ToErrno(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, ToErrno(err)
	}
	if fl.Directory && !fi.IsDir() {
		f.Close()
		return nil, ENOTDIR
	}
	return &readFile{fsys: r.fsys, name: p, f: f, isDir: fi.IsDir()}, 0
}

func (r *readFS) Stat(p string, _ bool) (Stat, Errno) {
	if !fs.ValidPath(p) {
		return Stat{}, ENOENT
	}
	fi, err := fs.Stat(r.fsys, p)
	if err != nil {
		return Stat{}, ToErrno(err)
	}
	return StatOf(fi), 0
}

func (r *readFS) Mkdir(string) Errno          { return EROFS }
func (r *readFS) Rmdir(string) Errno          { return EROFS }
func (r *readFS) Unlink(string) Errno         { return EROFS }
func (r *readFS) Rename(string, string) Errno { return EROFS }
func (r *readFS) Link(string, string) Errno   { return EROFS }
func (r *readFS) Symlink(string, string) Errno {
	return EROFS
}
func (r *readFS) Utimes(string, Times, bool) Errno { return EROFS }

func (r *readFS) Readlink(p string) (string, Errno) {
	if _, e := r.Stat(p, false); e != 0 {
		return "", e
	}
	return "", EINVAL
}

// readFile is a file of an fs.FS.
type readFile struct {
	fsys  fs.FS
	name  string
	isDir bool

	mu  sync.Mutex // guards f's offset
	f   fs.File
	off int64 // the offset, for a file that cannot seek
}

func (f *readFile) Read(p []byte) (int, Errno) {
	if f.isDir {
		return 0, EISDIR
	}
	if len(p) == 0 {
		return 0, 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.f.Read(p)
	f.off += int64(n)
	return n, ToErrno(err)
}

func (f *readFile) Pread(p []byte, off int64) (int, Errno) {
	switch {
	case f.isDir:
		return 0, EISDIR
	case off < 0:
		return 0, EINVAL
	case len(p) == 0:
		return 0, 0
	}
	if ra, ok := f.f.(io.ReaderAt); ok {
		n, err := ra.ReadAt(p, off)
		return n, ToErrno(err)
	}
	s, ok := f.f.(io.Seeker)
	if !ok {
		return 0, ESPIPE
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, ToErrno(err)
	}
	if _, err := s.Seek(off, io.SeekStart); err != nil {
		return 0, ToErrno(err)
	}
	n, err := io.ReadFull(f.f, p)
	if err == io.ErrUnexpectedEOF {
		err = nil
	}
	if _, serr := s.Seek(cur, io.SeekStart); err == nil {
		err = serr
	}
	return n, ToErrno(err)
}

func (f *readFile) Write([]byte) (int, Errno)         { return 0, EBADF }
func (f *readFile) Pwrite([]byte, int64) (int, Errno) { return 0, EBADF }
func (f *readFile) Truncate(int64) Errno              { return EBADF }
func (f *readFile) Sync() Errno                       { return 0 }
func (f *readFile) SetAppend(bool) Errno              { return 0 }
func (f *readFile) Close() Errno                      { return ToErrno(f.f.Close()) }
func (f *readFile) Stat() (Stat, Errno)               { return f.stat() }
func (f *readFile) stat() (Stat, Errno) {
	fi, err := f.f.Stat()
	if err != nil {
		return Stat{}, ToErrno(err)
	}
	return StatOf(fi), 0
}

func (f *readFile) Lseek(off int64, whence int) (int64, Errno) {
	if whence < io.SeekStart || whence > io.SeekEnd {
		return 0, EINVAL
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.f.(io.Seeker); ok {
		n, err := s.Seek(off, whence)
		if err != nil {
			return 0, ToErrno(err)
		}
		f.off = n
		return n, 0
	}
	if whence == io.SeekCurrent && off == 0 {
		return f.off, 0
	}
	return 0, ESPIPE
}

func (f *readFile) Readdir() ([]Dirent, Errno) {
	if !f.isDir {
		return nil, ENOTDIR
	}
	es, err := fs.ReadDir(f.fsys, f.name)
	if err != nil {
		return nil, ToErrno(err)
	}
	ds := make([]Dirent, len(es))
	for i, e := range es {
		ds[i] = Dirent{Name: e.Name(), Type: e.Type()}
		if fi, err := e.Info(); err == nil {
			ds[i].Ino = StatOf(fi).Ino
		}
	}
	return ds, 0
}
