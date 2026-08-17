package proctree

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func currentPID() int { return os.Getpid() }

// snapshotEntry finds pid's row in a toolhelp process snapshot.
func snapshotEntry(pid int) (*windows.ProcessEntry32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	defer windows.CloseHandle(snap) //nolint:errcheck // read-only snapshot handle
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err := windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if int(entry.ProcessID) == pid {
			e := entry
			return &e, nil
		}
	}
	return nil, fmt.Errorf("pid %d not in process snapshot", pid)
}

func refOf(pid int) (ProcessRef, error) {
	entry, err := snapshotEntry(pid)
	if err != nil {
		return ProcessRef{}, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ProcessRef{}, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // query-only handle
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return ProcessRef{}, fmt.Errorf("process times for %d: %w", pid, err)
	}
	return ProcessRef{
		PID:       pid,
		StartTime: int64(creation.HighDateTime)<<32 | int64(creation.LowDateTime),
		Exe:       windows.UTF16ToString(entry.ExeFile[:]),
	}, nil
}

func parentOf(pid int) int {
	entry, err := snapshotEntry(pid)
	if err != nil {
		return 0
	}
	return int(entry.ParentProcessID)
}
