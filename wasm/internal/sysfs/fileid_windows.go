package sysfs

import (
	"encoding/binary"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileIDs fills in st's device, inode and link count from f's handle.
func fileIDs(f *os.File, st *Stat) {
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info) != nil {
		return
	}
	st.Dev = uint64(info.VolumeSerialNumber)
	st.Ino = uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	st.Nlink = uint64(info.NumberOfLinks)
}

// pathIDs reports whether a path's Stat needs fileIDs from an open file for
// its device and inode.
const pathIDs = true

// readdirIDs lists directory f with each entry's file ID, which is the
// inode its Stat reports, or returns false where the file system does not
// give the IDs.
func readdirIDs(f *os.File) ([]Dirent, bool) {
	h := windows.Handle(f.Fd())
	buf := make([]uint64, 8192)
	b := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), len(buf)*8)
	var ds []Dirent
	class := uint32(windows.FileIdBothDirectoryRestartInfo)
	for {
		if err := windows.GetFileInformationByHandleEx(h, class, &b[0], uint32(len(b))); err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				return ds, true
			}
			return nil, false
		}
		class = windows.FileIdBothDirectoryInfo
		for off := 0; ; {
			e := b[off:]
			next := binary.LittleEndian.Uint32(e[0:])
			attrs := binary.LittleEndian.Uint32(e[56:])
			nameLen := binary.LittleEndian.Uint32(e[60:])
			tag := binary.LittleEndian.Uint32(e[64:])
			id := binary.LittleEndian.Uint64(e[96:])
			name16 := unsafe.Slice((*uint16)(unsafe.Pointer(&e[104])), nameLen/2)
			name := windows.UTF16ToString(name16)
			if name != "." && name != ".." {
				var typ fs.FileMode
				switch {
				case attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && tag == windows.IO_REPARSE_TAG_SYMLINK:
					typ = fs.ModeSymlink
				case attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && tag == windows.IO_REPARSE_TAG_MOUNT_POINT:
					typ = fs.ModeSymlink
				case attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
					typ = fs.ModeDir
				}
				ds = append(ds, Dirent{Name: name, Ino: id, Type: typ})
			}
			if next == 0 {
				break
			}
			off += int(next)
		}
	}
}
