package fixedbase

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	ntdll                     = syscall.NewLazyDLL("ntdll.dll")
	ntQueryInformationProcess = ntdll.NewProc("NtQueryInformationProcess")
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	readProcessMemory         = kernel32.NewProc("ReadProcessMemory")
)

// LoadedBase starts the program at path suspended, before any of its code
// runs, reads where Windows mapped its image -- the process environment
// block's ImageBaseAddress, two pointers into it -- and ends it.
func LoadedBase(path string) (uint64, error) {
	const createSuspended = 0x00000004
	cmd := exec.Command(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	const access = 0x0400 | 0x0010 // PROCESS_QUERY_INFORMATION, PROCESS_VM_READ
	h, err := syscall.OpenProcess(access, false, uint32(cmd.Process.Pid))
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(h)
	// PROCESS_BASIC_INFORMATION: ExitStatus, PebBaseAddress,
	// AffinityMask, BasePriority, UniqueProcessId,
	// InheritedFromUniqueProcessId, each a pointer's size.
	var pbi [6]uintptr
	var n uint32
	if st, _, _ := ntQueryInformationProcess.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&pbi)), unsafe.Sizeof(pbi), uintptr(unsafe.Pointer(&n))); st != 0 {
		return 0, fmt.Errorf("NtQueryInformationProcess: status %#x", st)
	}
	var base uintptr
	var read uintptr
	at := pbi[1] + 2*unsafe.Sizeof(uintptr(0)) // PEB.ImageBaseAddress
	if r, _, err := readProcessMemory.Call(uintptr(h), at, uintptr(unsafe.Pointer(&base)), unsafe.Sizeof(base), uintptr(unsafe.Pointer(&read))); r == 0 {
		return 0, fmt.Errorf("ReadProcessMemory: %w", err)
	}
	return uint64(base), nil
}
