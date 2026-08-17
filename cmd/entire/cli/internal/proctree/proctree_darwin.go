package proctree

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func currentPPID() int { return os.Getppid() }

// procInfo resolves pid's ref and parent PID in a single sysctl.
func procInfo(pid int) (ProcessRef, int, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ProcessRef{}, 0, fmt.Errorf("sysctl kern.proc.pid %d: %w", pid, err)
	}
	start := kp.Proc.P_starttime
	comm := kp.Proc.P_comm[:]
	n := 0
	for n < len(comm) && comm[n] != 0 {
		n++
	}
	b := make([]byte, n)
	for i := range n {
		b[i] = comm[i]
	}
	ref := ProcessRef{
		PID:       pid,
		StartTime: start.Sec*1_000_000 + int64(start.Usec),
		Exe:       strings.ToValidUTF8(string(b), ""),
	}
	return ref, int(kp.Eproc.Ppid), nil
}
