package proctree

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func currentPPID() int { return os.Getppid() }

// procInfo resolves pid's ref and parent PID from one /proc/<pid>/stat read.
//
// comm is parenthesized and may itself contain spaces and parentheses, so the
// only safe parse is from the LAST ')' — everything after it is
// space-separated. In the post-comm fields, index 1 is ppid (stat field 4)
// and index 19 is starttime (stat field 22, clock ticks since boot).
func procInfo(pid int) (ProcessRef, int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ProcessRef{}, 0, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	s := string(data)
	open := strings.IndexByte(s, '(')
	closeIdx := strings.LastIndexByte(s, ')')
	if open < 0 || closeIdx < open || closeIdx+2 > len(s) {
		return ProcessRef{}, 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	comm := s[open+1 : closeIdx]
	fields := strings.Fields(s[closeIdx+2:])
	if len(fields) < 20 {
		return ProcessRef{}, 0, fmt.Errorf("short /proc/%d/stat: %d fields", pid, len(fields))
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return ProcessRef{}, 0, fmt.Errorf("parse ppid for pid %d: %w", pid, err)
	}
	start, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return ProcessRef{}, 0, fmt.Errorf("parse starttime for pid %d: %w", pid, err)
	}
	return ProcessRef{PID: pid, StartTime: start, Exe: comm}, ppid, nil
}
