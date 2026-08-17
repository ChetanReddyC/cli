package proctree

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func currentPID() int { return os.Getpid() }

// statFields returns the fields of /proc/<pid>/stat AFTER the comm field.
// comm is parenthesized and may itself contain spaces and parentheses, so the
// only safe parse is from the LAST ')' — everything after it is
// space-separated. Returned slice index 0 is field 3 (state); ppid is field 4
// (index 1); starttime is field 22 (index 19).
func statFields(pid int) (comm string, fields []string, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", nil, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	s := string(data)
	open := strings.IndexByte(s, '(')
	closeIdx := strings.LastIndexByte(s, ')')
	if open < 0 || closeIdx < open || closeIdx+2 > len(s) {
		return "", nil, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	return s[open+1 : closeIdx], strings.Fields(s[closeIdx+2:]), nil
}

func refOf(pid int) (ProcessRef, error) {
	comm, fields, err := statFields(pid)
	if err != nil {
		return ProcessRef{}, err
	}
	if len(fields) < 20 {
		return ProcessRef{}, fmt.Errorf("short /proc/%d/stat: %d fields", pid, len(fields))
	}
	start, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return ProcessRef{}, fmt.Errorf("parse starttime for pid %d: %w", pid, err)
	}
	return ProcessRef{PID: pid, StartTime: start, Exe: comm}, nil
}

func parentOf(pid int) int {
	_, fields, err := statFields(pid)
	if err != nil || len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}
