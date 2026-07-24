package cli

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// spinnerFrames matches the bubbles/spinner Dot frames used by the activity
// TUI, so a CLI spinner here visually matches `entire activity`.
var spinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

const (
	spinnerInterval = 100 * time.Millisecond
	// spinnerInitialDelay is how long an operation must run before the
	// spinner appears at all. Faster operations don't get a spinner —
	// avoids flicker on warm runs that complete in under a quarter second.
	spinnerInitialDelay = 250 * time.Millisecond
)

// startSpinner prints msg followed by an animated spinner to w when the
// operation takes longer than spinnerInitialDelay. stop(true) leaves
// "✓ msg" on the line; stop(false) erases the line and writes nothing.
// On non-terminal writers the animation is omitted but stop(true) still
// prints the completion line.
func startSpinner(w io.Writer, msg string) func(success bool) {
	_, stop := startUpdatableSpinner(w, msg)
	return stop
}

// startUpdatableSpinner is startSpinner's variant for an operation whose
// status text changes while it runs (e.g. "session 2/5 · turn 3/10"). update
// replaces the message the next frame draws — or, on a non-terminal writer,
// the message stop's completion line uses. update is safe to call at any
// point, including before the spinner's first frame draws and after stop
// returns. stop behaves exactly like startSpinner's, rendering whichever
// message update last set (or msg, if update was never called).
func startUpdatableSpinner(w io.Writer, msg string) (update func(string), stop func(success bool)) {
	var mu sync.Mutex
	current := msg
	setMsg := func(m string) {
		mu.Lock()
		current = m
		mu.Unlock()
	}
	getMsg := func() string {
		mu.Lock()
		defer mu.Unlock()
		return current
	}

	if !interactive.IsTerminalWriter(w) {
		return setMsg, func(success bool) {
			if success {
				fmt.Fprintf(w, "✓ %s\n", getMsg())
			}
		}
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-done:
			return // operation finished before the spinner would appear
		case <-time.After(spinnerInitialDelay):
		}
		ticker := time.NewTicker(spinnerInterval)
		defer ticker.Stop()
		frame := 0
		draw := func() {
			// \033[K clears the rest of the line so a shorter message
			// (update shrank it) doesn't leave stale trailing characters.
			fmt.Fprintf(w, "\r\033[K%s %s", spinnerFrames[frame], getMsg())
			frame = (frame + 1) % len(spinnerFrames)
		}
		draw()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				draw()
			}
		}
	}()
	return setMsg, func(success bool) {
		close(done)
		<-stopped
		if success {
			fmt.Fprintf(w, "\r\033[K✓ %s\n", getMsg())
			return
		}
		fmt.Fprint(w, "\r\033[K")
	}
}
