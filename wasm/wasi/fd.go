package wasi

import (
	"io/fs"
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/internal/sysfs"
)

// The file types.
const (
	filetypeUnknown         = 0
	filetypeBlockDevice     = 1
	filetypeCharacterDevice = 2
	filetypeDirectory       = 3
	filetypeRegularFile     = 4
	filetypeSymbolicLink    = 7
)

// The fdflags.
const (
	fdflagAppend   = 1 << 0
	fdflagDsync    = 1 << 1
	fdflagNonblock = 1 << 2
	fdflagRsync    = 1 << 3
	fdflagSync     = 1 << 4
	fdflagsAll     = fdflagAppend | fdflagDsync | fdflagNonblock | fdflagRsync | fdflagSync
)

// The rights.
const (
	rightFdDatasync = 1 << iota
	rightFdRead
	rightFdSeek
	rightFdFdstatSetFlags
	rightFdSync
	rightFdTell
	rightFdWrite
	rightFdAdvise
	rightFdAllocate
	rightPathCreateDirectory
	rightPathCreateFile
	rightPathLinkSource
	rightPathLinkTarget
	rightPathOpen
	rightFdReaddir
	rightPathReadlink
	rightPathRenameSource
	rightPathRenameTarget
	rightPathFilestatGet
	rightPathFilestatSetSize
	rightPathFilestatSetTimes
	rightFdFilestatGet
	rightFdFilestatSetSize
	rightFdFilestatSetTimes
	rightPathSymlink
	rightPathRemoveDirectory
	rightPathUnlinkFile
	rightPollFdReadwrite
	rightSockShutdown
	rightSockAccept
)

// fileRights are the rights of a regular file.
const fileRights = rightFdDatasync | rightFdRead | rightFdSeek | rightFdFdstatSetFlags |
	rightFdSync | rightFdTell | rightFdWrite | rightFdAdvise | rightFdAllocate |
	rightFdFilestatGet | rightFdFilestatSetSize | rightFdFilestatSetTimes | rightPollFdReadwrite

// dirRights are the rights of a directory.
const dirRights = rightFdDatasync | rightFdFdstatSetFlags | rightFdSync |
	rightPathCreateDirectory | rightPathCreateFile | rightPathLinkSource |
	rightPathLinkTarget | rightPathOpen | rightFdReaddir | rightPathReadlink |
	rightPathRenameSource | rightPathRenameTarget | rightPathFilestatGet |
	rightPathFilestatSetSize | rightPathFilestatSetTimes | rightFdFilestatGet |
	rightFdFilestatSetTimes | rightPathSymlink | rightPathRemoveDirectory |
	rightPathUnlinkFile

// filetype returns the WASI file type of mode.
func filetype(mode fs.FileMode) uint8 {
	switch {
	case mode.IsRegular():
		return filetypeRegularFile
	case mode.IsDir():
		return filetypeDirectory
	case mode&fs.ModeSymlink != 0:
		return filetypeSymbolicLink
	case mode&fs.ModeCharDevice != 0:
		return filetypeCharacterDevice
	case mode&fs.ModeDevice != 0:
		return filetypeBlockDevice
	}
	return filetypeUnknown
}

// defaultRights returns the rights of a file of type ft.
func defaultRights(ft uint8) (base, inheriting uint64) {
	switch ft {
	case filetypeDirectory:
		return dirRights, fileRights | dirRights
	case filetypeCharacterDevice:
		return fileRights &^ (rightFdSeek | rightFdTell), 0
	}
	return fileRights, 0
}

// rights returns e's rights, the file being of type ft.
func rights(e *sysfs.Entry, ft uint8) (base, inheriting uint64) {
	if e.RightsSet {
		return e.RightsBase, e.RightsInheriting
	}
	return defaultRights(ft)
}

func fdAdvise(c *call, p []uint64) sysfs.Errno {
	if _, errno := c.fileWith(p[0], rightFdAdvise); errno != 0 {
		return errno
	}
	if uint32(p[3]) > 5 {
		return sysfs.EINVAL
	}
	return 0
}

func fdAllocate(c *call, p []uint64) sysfs.Errno {
	e, errno := c.fileWith(p[0], rightFdAllocate)
	if errno != 0 {
		return errno
	}
	off, n := p[1], p[2]
	if off > math.MaxInt64 || n > math.MaxInt64-off {
		return sysfs.EINVAL
	}
	if e.IsDir {
		return sysfs.EBADF
	}
	st, errno := e.File.Stat()
	if errno != 0 {
		return errno
	}
	if end := int64(off + n); st.Size < end {
		return e.File.Truncate(end)
	}
	return 0
}

func fdClose(c *call, p []uint64) sysfs.Errno {
	return c.sc.Files.CloseFD(uint32(p[0]))
}

func fdSync(c *call, p []uint64) sysfs.Errno {
	e, errno := c.fileWith(p[0], rightFdSync)
	if errno != 0 {
		return errno
	}
	return e.File.Sync()
}

func fdDatasync(c *call, p []uint64) sysfs.Errno {
	e, errno := c.fileWith(p[0], rightFdDatasync)
	if errno != 0 {
		return errno
	}
	return e.File.Sync()
}

func fdFdstatGet(c *call, p []uint64) sysfs.Errno {
	e, errno := c.file(p[0])
	if errno != 0 {
		return errno
	}
	st, errno := e.File.Stat()
	if errno != 0 {
		return errno
	}
	b, ok := c.bytes(uint32(p[1]), 24)
	if !ok {
		return sysfs.EFAULT
	}
	ft := filetype(st.Mode)
	e.Lock()
	flags := e.Flags
	base, inheriting := rights(e, ft)
	e.Unlock()
	clear(b)
	b[0] = ft
	le.PutUint16(b[2:], flags)
	le.PutUint64(b[8:], base)
	le.PutUint64(b[16:], inheriting)
	return 0
}

func fdFdstatSetFlags(c *call, p []uint64) sysfs.Errno {
	e, errno := c.fileWith(p[0], rightFdFdstatSetFlags)
	if errno != 0 {
		return errno
	}
	flags := uint32(p[1])
	if flags&^fdflagsAll != 0 {
		return sysfs.EINVAL
	}
	e.Lock()
	defer e.Unlock()
	if !e.IsDir {
		if errno := e.File.SetAppend(flags&fdflagAppend != 0); errno != 0 {
			return errno
		}
	}
	e.Flags = uint16(flags)
	return 0
}

func fdFdstatSetRights(c *call, p []uint64) sysfs.Errno {
	e, errno := c.file(p[0])
	if errno != 0 {
		return errno
	}
	st, errno := e.File.Stat()
	if errno != 0 {
		return errno
	}
	e.Lock()
	defer e.Unlock()
	base, inheriting := rights(e, filetype(st.Mode))
	if p[1]&^base != 0 || p[2]&^inheriting != 0 {
		return sysfs.ENOTCAPABLE
	}
	e.RightsBase, e.RightsInheriting, e.RightsSet = p[1], p[2], true
	e.Restricted = true
	return 0
}

func fdFilestatGet(c *call, p []uint64) sysfs.Errno {
	e, errno := c.fileWith(p[0], rightFdFilestatGet)
	if errno != 0 {
		return errno
	}
	st, errno := e.File.Stat()
	if errno != 0 {
		return errno
	}
	return c.putFilestat(uint32(p[1]), &st)
}

// putFilestat writes st as a filestat at off.
func (c *call) putFilestat(off uint32, st *sysfs.Stat) sysfs.Errno {
	b, ok := c.bytes(off, 64)
	if !ok {
		return sysfs.EFAULT
	}
	le.PutUint64(b[0:], st.Dev)
	le.PutUint64(b[8:], st.Ino)
	le.PutUint64(b[16:], uint64(filetype(st.Mode)))
	le.PutUint64(b[24:], st.Nlink)
	le.PutUint64(b[32:], uint64(st.Size))
	le.PutUint64(b[40:], uint64(st.Atim))
	le.PutUint64(b[48:], uint64(st.Mtim))
	le.PutUint64(b[56:], uint64(st.Ctim))
	return 0
}

func fdFilestatSetSize(c *call, p []uint64) sysfs.Errno {
	e, errno := c.fileWith(p[0], rightFdFilestatSetSize)
	if errno != 0 {
		return errno
	}
	if p[1] > math.MaxInt64 {
		return sysfs.EINVAL
	}
	return e.File.Truncate(int64(p[1]))
}

// The fstflags.
const (
	fstAtim    = 1 << 0
	fstAtimNow = 1 << 1
	fstMtim    = 1 << 2
	fstMtimNow = 1 << 3
)

// times returns the times fstflags flags ask to set.
func (c *call) times(atim, mtim uint64, flags uint32) (sysfs.Times, sysfs.Errno) {
	if flags&^(fstAtim|fstAtimNow|fstMtim|fstMtimNow) != 0 ||
		flags&(fstAtim|fstAtimNow) == fstAtim|fstAtimNow ||
		flags&(fstMtim|fstMtimNow) == fstMtim|fstMtimNow {
		return sysfs.Times{}, sysfs.EINVAL
	}
	var t sysfs.Times
	var now int64
	if flags&(fstAtimNow|fstMtimNow) != 0 {
		now = c.walltime()
	}
	switch {
	case flags&fstAtim != 0:
		t.Atim, t.SetAtim = int64(atim), true
	case flags&fstAtimNow != 0:
		t.Atim, t.SetAtim = now, true
	}
	switch {
	case flags&fstMtim != 0:
		t.Mtim, t.SetMtim = int64(mtim), true
	case flags&fstMtimNow != 0:
		t.Mtim, t.SetMtim = now, true
	}
	return t, 0
}

func fdFilestatSetTimes(c *call, p []uint64) sysfs.Errno {
	e, errno := c.fileWith(p[0], rightFdFilestatSetTimes)
	if errno != 0 {
		return errno
	}
	t, errno := c.times(p[1], p[2], uint32(p[3]))
	if errno != 0 {
		return errno
	}
	if e.FS == nil {
		return sysfs.ENOTSUP
	}
	if !t.SetAtim && !t.SetMtim {
		return 0
	}
	return e.FS.Utimes(e.Path, t, true)
}

// iovecs returns the count iovecs at off, each a buffer of memory.
func (c *call) iovecs(off, count uint32) ([][]byte, sysfs.Errno) {
	b, ok := c.array(off, count, 8)
	if !ok {
		return nil, sysfs.EFAULT
	}
	bufs := make([][]byte, count)
	for i := range bufs {
		buf, ok := c.bytes(le.Uint32(b[8*i:]), le.Uint32(b[8*i+4:]))
		if !ok {
			return nil, sysfs.EFAULT
		}
		bufs[i] = buf
	}
	return bufs, 0
}

// readv reads into the iovecs at iovs with read, which is given each
// buffer and the count read before it, and writes the count read at
// resultPtr.
func (c *call) readv(fd uint64, iovs, count, resultPtr uint32, read func(f sysfs.File, b []byte, done int64) (int, sysfs.Errno)) sysfs.Errno {
	e, errno := c.fileWith(fd, rightFdRead)
	if errno != 0 {
		return errno
	}
	if e.IsDir {
		return sysfs.EISDIR
	}
	bufs, errno := c.iovecs(iovs, count)
	if errno != 0 {
		return errno
	}
	var total int64
	for _, b := range bufs {
		if len(b) == 0 {
			continue
		}
		n, errno := read(e.File, b, total)
		total += int64(n)
		if errno != 0 {
			if total == 0 {
				return errno
			}
			break
		}
		if n < len(b) {
			break
		}
	}
	if !c.putU32(resultPtr, uint32(total)) {
		return sysfs.EFAULT
	}
	return 0
}

// writev writes the iovecs at iovs with write, as readv reads.
func (c *call) writev(fd uint64, iovs, count, resultPtr uint32, write func(f sysfs.File, b []byte, done int64) (int, sysfs.Errno)) sysfs.Errno {
	e, errno := c.fileWith(fd, rightFdWrite)
	if errno != 0 {
		return errno
	}
	if e.IsDir {
		return sysfs.EBADF
	}
	bufs, errno := c.iovecs(iovs, count)
	if errno != 0 {
		return errno
	}
	var total int64
	for _, b := range bufs {
		if len(b) == 0 {
			continue
		}
		n, errno := write(e.File, b, total)
		total += int64(n)
		if errno != 0 {
			if total == 0 {
				return errno
			}
			break
		}
		if n < len(b) {
			break
		}
	}
	e.Lock()
	sync := e.Flags&(fdflagDsync|fdflagSync) != 0
	e.Unlock()
	if sync && total > 0 {
		if errno := e.File.Sync(); errno != 0 {
			return errno
		}
	}
	if !c.putU32(resultPtr, uint32(total)) {
		return sysfs.EFAULT
	}
	return 0
}

func fdRead(c *call, p []uint64) sysfs.Errno {
	return c.readv(p[0], uint32(p[1]), uint32(p[2]), uint32(p[3]), func(f sysfs.File, b []byte, _ int64) (int, sysfs.Errno) {
		return f.Read(b)
	})
}

func fdPread(c *call, p []uint64) sysfs.Errno {
	off := p[3]
	if off > math.MaxInt64 {
		return sysfs.EINVAL
	}
	return c.readv(p[0], uint32(p[1]), uint32(p[2]), uint32(p[4]), func(f sysfs.File, b []byte, done int64) (int, sysfs.Errno) {
		return f.Pread(b, int64(off)+done)
	})
}

func fdWrite(c *call, p []uint64) sysfs.Errno {
	return c.writev(p[0], uint32(p[1]), uint32(p[2]), uint32(p[3]), func(f sysfs.File, b []byte, _ int64) (int, sysfs.Errno) {
		return f.Write(b)
	})
}

func fdPwrite(c *call, p []uint64) sysfs.Errno {
	off := p[3]
	if off > math.MaxInt64 {
		return sysfs.EINVAL
	}
	return c.writev(p[0], uint32(p[1]), uint32(p[2]), uint32(p[4]), func(f sysfs.File, b []byte, done int64) (int, sysfs.Errno) {
		return f.Pwrite(b, int64(off)+done)
	})
}

func fdPrestatGet(c *call, p []uint64) sysfs.Errno {
	e, errno := c.file(p[0])
	if errno != 0 {
		return errno
	}
	if e.Preopen == "" {
		return sysfs.EBADF
	}
	b, ok := c.bytes(uint32(p[1]), 8)
	if !ok {
		return sysfs.EFAULT
	}
	le.PutUint32(b, 0) // the tag of a directory
	le.PutUint32(b[4:], uint32(len(e.Preopen)))
	return 0
}

func fdPrestatDirName(c *call, p []uint64) sysfs.Errno {
	e, errno := c.file(p[0])
	if errno != 0 {
		return errno
	}
	if e.Preopen == "" {
		return sysfs.EBADF
	}
	if uint32(p[2]) < uint32(len(e.Preopen)) {
		return sysfs.ENAMETOOLONG
	}
	b, ok := c.bytes(uint32(p[1]), uint32(len(e.Preopen)))
	if !ok {
		return sysfs.EFAULT
	}
	copy(b, e.Preopen)
	return 0
}

// direntSize is the size of a dirent before its name.
const direntSize = 24

func fdReaddir(c *call, p []uint64) sysfs.Errno {
	e, errno := c.fileWith(p[0], rightFdReaddir)
	if errno != 0 {
		return errno
	}
	if !e.IsDir {
		return sysfs.ENOTDIR
	}
	buf, ok := c.bytes(uint32(p[1]), uint32(p[2]))
	if !ok {
		return sysfs.EFAULT
	}
	cookie := p[3]
	st, errno := e.File.Stat()
	if errno != 0 {
		return errno
	}
	e.Lock()
	ds, errno := e.Dirents(cookie == 0)
	e.Unlock()
	if errno != 0 {
		return errno
	}
	// The listing begins with "." and "..", which the entry's lacks.
	n := 0
	var ent [direntSize]byte
	for i := cookie; i < uint64(len(ds))+2 && n < len(buf); i++ {
		var d sysfs.Dirent
		switch i {
		case 0:
			d = sysfs.Dirent{Name: ".", Ino: st.Ino, Type: fs.ModeDir}
		case 1:
			d = sysfs.Dirent{Name: "..", Type: fs.ModeDir}
		default:
			d = ds[i-2]
		}
		le.PutUint64(ent[0:], i+1)
		le.PutUint64(ent[8:], d.Ino)
		le.PutUint32(ent[16:], uint32(len(d.Name)))
		ent[20], ent[21], ent[22], ent[23] = filetype(d.Type), 0, 0, 0
		n += copy(buf[n:], ent[:])
		n += copy(buf[n:], d.Name)
	}
	if !c.putU32(uint32(p[4]), uint32(n)) {
		return sysfs.EFAULT
	}
	return 0
}

func fdRenumber(c *call, p []uint64) sysfs.Errno {
	return c.sc.Files.Renumber(uint32(p[0]), uint32(p[1]))
}

func fdSeek(c *call, p []uint64) sysfs.Errno {
	return c.seek(p[0], int64(p[1]), uint32(p[2]), uint32(p[3]))
}

func fdTell(c *call, p []uint64) sysfs.Errno {
	return c.seek(p[0], 0, 1, uint32(p[1]))
}

func (c *call) seek(fd uint64, off int64, whence, resultPtr uint32) sysfs.Errno {
	right := uint64(rightFdSeek)
	if off == 0 && whence == 1 {
		right = rightFdTell
	}
	e, errno := c.fileWith(fd, right)
	if errno != 0 {
		return errno
	}
	if e.IsDir {
		return sysfs.EISDIR
	}
	if whence > 2 {
		return sysfs.EINVAL
	}
	n, errno := e.File.Lseek(off, int(whence))
	if errno != 0 {
		return errno
	}
	if !c.putU64(resultPtr, uint64(n)) {
		return sysfs.EFAULT
	}
	return 0
}
