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

type loginURLActionTestResult struct {
	action loginURLAction
	err    error
}

func TestReadLoginURLActionFromTTY_SingleKeyAndRestoresTerminal(t *testing.T) {
	t.Parallel()

	ptmx, tty, observer, before := openLoginPromptPTY(t)
	defer ptmx.Close()
	defer observer.Close()

	resultCh := make(chan loginURLActionTestResult, 1)
	go func() {
		action, err := readLoginURLActionFromTTY(context.Background(), tty)
		resultCh <- loginURLActionTestResult{action: action, err: err}
	}()
	waitForLoginPromptTTYRaw(t, observer, before)

	// Alt+c, the arrow sequence, and the obsolete o action must be ignored,
	// while Enter opens immediately. Bubble Tea owns decoding and raw-mode
	// restoration.
	if _, err := ptmx.WriteString("\x1bc\x1b[Co\r"); err != nil {
		t.Fatalf("write key to pty: %v", err)
	}

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("readLoginURLActionFromTTY() error = %v", result.err)
		}
		if result.action != loginURLOpen {
			t.Errorf("action = %v, want loginURLOpen", result.action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("single-key input blocked waiting for a newline")
	}

	assertLoginPromptTTYRestored(t, observer, before)
}

func waitForLoginPromptTTYRaw(t *testing.T, tty *os.File, before *term.State) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, err := term.GetState(int(tty.Fd()))
		if err != nil {
			t.Fatalf("read terminal state: %v", err)
		}
		if !reflect.DeepEqual(state, before) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("terminal did not enter raw mode")
}

func TestReadLoginURLActionFromTTY_UnavailableInputContinuesAndCloses(t *testing.T) {
	t.Parallel()

	notTTY, err := os.CreateTemp(t.TempDir(), "not-a-tty")
	if err != nil {
		t.Fatalf("create non-TTY input: %v", err)
	}

	action, err := readLoginURLActionFromTTY(context.Background(), notTTY)
	if err != nil {
		t.Fatalf("readLoginURLActionFromTTY() error = %v", err)
	}
	if action != loginURLNone {
		t.Errorf("action = %v, want loginURLNone", action)
	}
	if _, err := notTTY.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("input was not closed after fallback: %v", err)
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
