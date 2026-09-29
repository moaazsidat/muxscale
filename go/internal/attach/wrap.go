// Package attach implements the mechanisms by which the daemon reaches a
// session. `wrap` is the highest-fidelity one: the daemon owns the PTY, so it
// sees every byte, controls the window size, knows the exact exit code, and can
// write input.
package attach

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Wrapped is an agent process running under a daemon-owned PTY.
type Wrapped struct {
	Cmd *exec.Cmd
	PTY *os.File

	// cols and rows are the PTY's current size. Unexported, and read through
	// Size, because Resize can change them from an HTTP handler while the
	// drain goroutine is reading them -- which was a data race the moment
	// remote resize existed.
	cols uint16
	rows uint16

	mu       sync.Mutex
	closed   bool
	exitCode int
	exited   chan struct{}
	exitErr  error

	// writeMu serialises writes so two concurrent inputs cannot interleave
	// bytes mid-escape-sequence.
	writeMu sync.Mutex
}

// Size reports the PTY's current dimensions.
func (w *Wrapped) Size() (cols, rows uint16) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cols, w.rows
}

// WrapOptions configures a wrapped launch.
type WrapOptions struct {
	Command []string
	Dir     string
	Env     []string
	Cols    uint16
	Rows    uint16
}

// Wrap starts a command under a new PTY owned by this process.
func Wrap(opt WrapOptions) (*Wrapped, error) {
	if len(opt.Command) == 0 {
		return nil, errors.New("no command given")
	}
	if opt.Cols == 0 {
		opt.Cols = 120
	}
	if opt.Rows == 0 {
		opt.Rows = 40
	}

	cmd := exec.Command(opt.Command[0], opt.Command[1:]...)
	cmd.Dir = opt.Dir
	cmd.Env = opt.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	// Mark the session so an adapter can detect it was launched by muxscale,
	// and so a shim can avoid re-wrapping recursively.
	cmd.Env = append(cmd.Env, "MUXSCALE_WRAPPED=1")

	// Own process group: signals sent to the daemon must not tear down the
	// agent, and the agent must be able to receive job-control signals of its
	// own.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: opt.Cols, Rows: opt.Rows})
	if err != nil {
		return nil, fmt.Errorf("starting %s under a pty: %w", opt.Command[0], err)
	}

	w := &Wrapped{
		Cmd: cmd, PTY: f, cols: opt.Cols, rows: opt.Rows,
		exited: make(chan struct{}), exitCode: -1,
	}
	go w.reap()
	return w, nil
}

func (w *Wrapped) reap() {
	err := w.Cmd.Wait()
	w.mu.Lock()
	w.exitErr = err
	var ee *exec.ExitError
	switch {
	case err == nil:
		w.exitCode = 0
	case errors.As(err, &ee):
		w.exitCode = ee.ExitCode()
	default:
		w.exitCode = -1
	}
	w.mu.Unlock()
	close(w.exited)
	// Closing the PTY unblocks any reader.
	_ = w.PTY.Close()
}

// Read streams raw output. It returns io.EOF once the process exits.
func (w *Wrapped) Read(p []byte) (int, error) {
	n, err := w.PTY.Read(p)
	if err != nil {
		// A closed PTY on macOS surfaces as EIO rather than EOF; normalise it
		// so callers do not have to special-case the platform.
		if errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
			return n, io.EOF
		}
	}
	return n, err
}

// Write sends raw bytes to the agent's stdin.
func (w *Wrapped) Write(p []byte) (int, error) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	select {
	case <-w.exited:
		return 0, errors.New("session has exited")
	default:
	}
	return w.PTY.Write(p)
}

// Resize changes the PTY window size.
//
// Callers must respect the rule that viewing never resizes: a phone rendering
// at 52 columns must not reflow the TUI for the human at the keyboard. This is
// only for an explicit, opted-in resize.
func (w *Wrapped) Resize(cols, rows uint16) error {
	if err := pty.Setsize(w.PTY, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
		return err
	}
	w.mu.Lock()
	w.cols, w.rows = cols, rows
	w.mu.Unlock()
	return nil
}

// Signal sends a signal to the agent's process group.
func (w *Wrapped) Signal(sig syscall.Signal) error {
	if w.Cmd.Process == nil {
		return errors.New("no process")
	}
	// Negative pid targets the group, so a shell wrapper's children go too.
	return syscall.Kill(-w.Cmd.Process.Pid, sig)
}

// Pid is the agent's process id.
func (w *Wrapped) Pid() int {
	if w.Cmd.Process == nil {
		return 0
	}
	return w.Cmd.Process.Pid
}

// Exited returns a channel closed when the process exits.
func (w *Wrapped) Exited() <-chan struct{} { return w.exited }

// ExitCode returns the exit code, or -1 if still running.
func (w *Wrapped) ExitCode() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exitCode
}

// Close terminates the session, escalating from hangup to SIGTERM to SIGKILL.
// Escalation matters: an agent mid-tool-call may ignore a polite request, and a
// hung session that cannot be killed is a support ticket.
func (w *Wrapped) Close(grace time.Duration) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()

	select {
	case <-w.exited:
		return nil
	default:
	}

	// Hang up before signalling. An interactive shell sitting at a prompt
	// *ignores* SIGTERM -- POSIX says so, and it is what stops a stray kill from
	// taking out your login session -- so a SIGTERM-first close waits out the
	// whole grace period and then SIGKILLs, losing the shell's exit hooks and
	// its history. SIGHUP is the signal a terminal emulator's departure produces
	// and the one every shell acts on; measured at well under a millisecond for
	// sh, bash and zsh.
	//
	// Sent explicitly rather than by closing the PTY master, which is the more
	// obvious move and does not work: on Darwin the close does not reliably
	// deliver the hangup, and it would also make Read return EOF before the
	// final output had been drained.
	//
	// Agents are unaffected -- they exit on either -- and now do so on the first.
	_ = w.Signal(syscall.SIGHUP)
	select {
	case <-w.exited:
		return nil
	case <-time.After(hangupGrace(grace)):
	}

	_ = w.Signal(syscall.SIGTERM)
	select {
	case <-w.exited:
		return nil
	case <-time.After(grace):
		_ = w.Signal(syscall.SIGKILL)
		select {
		case <-w.exited:
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("process did not exit after SIGKILL")
		}
	}
}

// hangupGrace is how long to wait for a hangup to work before signalling.
//
// Short by design: this is the path a well-behaved process takes, so it should
// cost almost nothing, and anything that ignores a hangup is not going to
// change its mind in another four seconds. The remaining budget is left for
// SIGTERM, which is where a busy agent legitimately needs time to finish.
func hangupGrace(grace time.Duration) time.Duration {
	const cap = 1500 * time.Millisecond
	if grace/3 < cap {
		return grace / 3
	}
	return cap
}
