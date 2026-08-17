// Package proctree resolves process ancestry with identity that survives PID
// reuse. A ProcessRef is a (PID, start time) pair: equal pairs name the same
// live process instance, because a recycled PID always comes with a new start
// time. Start-time units are platform-specific (microseconds since epoch on
// darwin, clock ticks since boot on linux, FILETIME on windows) — refs are
// only ever compared against refs recorded on the same machine, so the unit
// never needs to be interpreted, only compared.
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
	return refOf(pid)
}

// Ancestors returns the current process's ancestors nearest-first (parent,
// grandparent, ...), at most limit entries. The walk stops at the root
// (PID 0/1), on a cycle, or at the first resolution failure — partial
// results are returned, never an error: callers treat ancestry as
// best-effort evidence.
func Ancestors(limit int) []ProcessRef {
	var refs []ProcessRef
	seen := map[int]bool{}
	pid := parentOf(currentPID())
	for pid > 1 && len(refs) < limit && !seen[pid] {
		seen[pid] = true
		ref, err := refOf(pid)
		if err != nil {
			break
		}
		refs = append(refs, ref)
		pid = parentOf(pid)
	}
	return refs
}
