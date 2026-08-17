// Package proctree resolves process ancestry with identity that survives PID
// reuse. A ProcessRef is a (PID, start time) pair: equal pairs name the same
// live process instance, because within one boot a recycled PID always comes
// with a new start time. On Linux the unit is boot-relative (clock ticks), so
// a cross-reboot collision is theoretically possible — callers treat ancestry
// as best-effort evidence, never proof. Start-time units are otherwise
// platform-specific (microseconds since epoch on darwin, FILETIME on
// windows); refs are only ever compared against refs recorded on the same
// machine, so the unit never needs to be interpreted, only compared.
package proctree

// ProcessRef identifies one live process instance.
type ProcessRef struct {
	PID       int    `json:"pid"`
	StartTime int64  `json:"start_time"`
	Exe       string `json:"exe,omitempty"`
}

// SameProcess reports whether both refs name the same process instance:
// same PID and same start time. Exe is informational and not compared.
func (r ProcessRef) SameProcess(o ProcessRef) bool {
	return r.PID == o.PID && r.StartTime == o.StartTime && r.PID != 0
}

// Ref resolves the ProcessRef for pid. Errors when the process does not
// exist or the platform refuses introspection.
func Ref(pid int) (ProcessRef, error) {
	ref, _, err := procInfo(pid)
	return ref, err
}

// Ancestors returns the current process's ancestors nearest-first (parent,
// grandparent, ...), at most limit entries. The walk stops at the root
// (PID 0/1), on a cycle, or at the first resolution failure — partial
// results are returned, never an error: callers treat ancestry as
// best-effort evidence. Each level costs one platform introspection call
// (procInfo returns the ref and its parent PID together).
func Ancestors(limit int) []ProcessRef {
	var refs []ProcessRef
	seen := map[int]bool{}
	pid := currentPPID()
	for pid > 1 && len(refs) < limit && !seen[pid] {
		seen[pid] = true
		ref, ppid, err := procInfo(pid)
		if err != nil {
			break
		}
		refs = append(refs, ref)
		pid = ppid
	}
	return refs
}
