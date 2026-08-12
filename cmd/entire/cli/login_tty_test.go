//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func TestReadLoginURLActionFromTTY_SingleKeyAndRestoresTerminal(t *testing.T) {
	t.Parallel()

	ptmx, tty, observer, before := openLoginPromptPTY(t)
	defer ptmx.Close()
	defer observer.Close()

	resultCh := make(chan loginURLActionResult, 1)
	go func() {
		action, err := readLoginURLActionFromTTY(context.Background(), tty)
		resultCh <- loginURLActionResult{action: action, err: err}
	}()

	// No newline: c must be handled as soon as the single byte arrives.
	if _, err := ptmx.WriteString("c"); err != nil {
		t.Fatalf("write key to pty: %v", err)
	}

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("readLoginURLActionFromTTY() error = %v", result.err)
		}
		if result.action != loginURLCopy {
			t.Errorf("action = %v, want loginURLCopy", result.action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("single-key input blocked waiting for a newline")
	}

	assertLoginPromptTTYRestored(t, observer, before)
}

func TestReadLoginURLActionFromTTY_CancellationRestoresTerminal(t *testing.T) {
	t.Parallel()

	ptmx, tty, observer, before := openLoginPromptPTY(t)
	defer ptmx.Close()
	defer observer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan loginURLActionResult, 1)
	go func() {
		action, err := readLoginURLActionFromTTY(ctx, tty)
		resultCh <- loginURLActionResult{action: action, err: err}
	}()
	cancel()

	select {
	case result := <-resultCh:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled single-key input did not return")
	}

	assertLoginPromptTTYRestored(t, observer, before)
}

func TestReadLoginURLActionFromTTY_RawModeFailureContinuesAndCloses(t *testing.T) {
	t.Parallel()

	notTTY, err := os.CreateTemp(t.TempDir(), "not-a-tty")
	if err != nil {
		t.Fatalf("create non-TTY input: %v", err)
	}

	action, err := readLoginURLActionFromTTY(context.Background(), notTTY)
	if err != nil {
		t.Fatalf("readLoginURLActionFromTTY() error = %v", err)
	}
	if action != loginURLContinue {
		t.Errorf("action = %v, want loginURLContinue", action)
	}
	if _, err := notTTY.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("terminal was not closed after raw-mode failure: %v", err)
	}
}

func openLoginPromptPTY(t *testing.T) (ptmx, tty, observer *os.File, before *term.State) {
	t.Helper()

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("cannot open a pty here: %v", err)
	}

	observerFD, err := unix.Dup(int(tty.Fd()))
	if err != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		t.Fatalf("duplicate tty descriptor: %v", err)
	}
	observer = os.NewFile(uintptr(observerFD), "login-prompt-tty-observer")

	before, err = term.GetState(int(observer.Fd()))
	if err != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		_ = observer.Close()
		t.Fatalf("read initial terminal state: %v", err)
	}

	return ptmx, tty, observer, before
}

func assertLoginPromptTTYRestored(t *testing.T, tty *os.File, before *term.State) {
	t.Helper()

	after, err := term.GetState(int(tty.Fd()))
	if err != nil {
		t.Fatalf("read restored terminal state: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Error("terminal state was not restored after reading a login action")
	}
}
