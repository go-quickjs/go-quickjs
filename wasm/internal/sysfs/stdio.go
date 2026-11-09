package sysfs

import (
	"io"
	"io/fs"
	"sync"
)

// charDevice is the mode of the standard streams.
const charDevice = fs.ModeDevice | fs.ModeCharDevice | 0o660

// stdioFile is a standard stream: a character device that reads from r or
// writes to w.
type stdioFile struct {
	mu sync.Mutex // orders the reads, and the writes
	r  io.Reader
	w  io.Writer
}

// NewReaderFile returns standard input reading from r.
func NewReaderFile(r io.Reader) File { return &stdioFile{r: r} }

// NewWriterFile returns standard output or error writing to w.
func NewWriterFile(w io.Writer) File { return &stdioFile{w: w} }

func (s *stdioFile) Read(p []byte) (int, Errno) {
	if s.r == nil {
		return 0, EBADF
	}
	if len(p) == 0 {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.r.Read(p)
	return n, ToErrno(err)
}

func (s *stdioFile) Write(p []byte) (int, Errno) {
	if s.w == nil {
		return 0, EBADF
	}
	if len(p) == 0 {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	return n, ToErrno(err)
}

func (s *stdioFile) Pread([]byte, int64) (int, Errno)  { return 0, ESPIPE }
func (s *stdioFile) Pwrite([]byte, int64) (int, Errno) { return 0, ESPIPE }
func (s *stdioFile) Lseek(int64, int) (int64, Errno)   { return 0, ESPIPE }
func (s *stdioFile) Stat() (Stat, Errno)               { return Stat{Mode: charDevice, Nlink: 1}, 0 }
func (s *stdioFile) Readdir() ([]Dirent, Errno)        { return nil, ENOTDIR }
func (s *stdioFile) Truncate(int64) Errno              { return EINVAL }
func (s *stdioFile) Sync() Errno                       { return 0 }
func (s *stdioFile) SetAppend(bool) Errno              { return 0 }

// Close leaves the reader or writer open: the embedder owns it.
func (s *stdioFile) Close() Errno { return 0 }
