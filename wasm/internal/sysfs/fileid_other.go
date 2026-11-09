//go:build !windows

package sysfs

import "os"

// fileIDs does nothing: a file's Stat has its device and inode.
func fileIDs(*os.File, *Stat) {}

// readdirIDs returns false: Readdir's entries have their inodes.
func readdirIDs(*os.File) ([]Dirent, bool) { return nil, false }

// pathIDs reports whether a path's Stat needs fileIDs from an open file for
// its device and inode.
const pathIDs = false
