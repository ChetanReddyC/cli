package proctree

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func currentPPID() int { return os.Getppid() }

// procInfo resolves pid's ref and parent PID with one toolhelp snapshot scan
// plus one OpenProcess for the creation time.
func procInfo(pid int) (ProcessRef, int, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ProcessRef{}, 0, fmt.Errorf("process snapshot: %w", err)
	}
	defer windows.CloseHandle(snap) //nolint:errcheck // read-only snapshot handle
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	found := false
	for err := windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if int(entry.ProcessID) == pid {
			found = true
			break
		}
	}
	if !found {
		return ProcessRef{}, 0, fmt.Errorf("pid %d not in process snapshot", pid)
	}

	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ProcessRef{}, 0, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // query-only handle
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return ProcessRef{}, 0, fmt.Errorf("process times for %d: %w", pid, err)
	}
	ref := ProcessRef{
		PID:       pid,
		StartTime: int64(creation.HighDateTime)<<32 | int64(creation.LowDateTime),
		Exe:       windows.UTF16ToString(entry.ExeFile[:]),
	}
	return ref, int(entry.ParentProcessID), nil
}
