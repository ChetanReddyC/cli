package proctree

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func currentPID() int { return os.Getpid() }

func kinfo(pid int) (*unix.KinfoProc, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.proc.pid %d: %w", pid, err)
	}
	return kp, nil
}

func refOf(pid int) (ProcessRef, error) {
	kp, err := kinfo(pid)
	if err != nil {
		return ProcessRef{}, err
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
	exe := strings.ToValidUTF8(string(b), "")
	return ProcessRef{
		PID:       pid,
		StartTime: start.Sec*1_000_000 + int64(start.Usec),
		Exe:       exe,
	}, nil
}

func parentOf(pid int) int {
	kp, err := kinfo(pid)
	if err != nil {
		return 0
	}
	return int(kp.Eproc.Ppid)
}
