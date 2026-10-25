package daemon

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/attach"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// shellWithAgent runs a shell muxscale owns with a long sleep in it standing
// in for an agent started by hand, and returns the shell's session and the
// sleep's pid.
func shellWithAgent(t *testing.T, m *Manager) (*protocol.Session, int) {
	t.Helper()
	sh, err := m.Run(RunOptions{
		Command: []string{"/bin/sh", "-c", "sleep 300; echo back at the prompt; sleep 300"},
		Dir:     t.TempDir(),
		AgentID: ShellAgentID,
		Cols:    80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { waitForShellExit(t, m, sh.ID) })

	m.mu.RLock()
	shellPid := m.wrapped[sh.ID].Pid()
	m.mu.RUnlock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("pgrep", "-P", strconv.Itoa(shellPid), "sleep").Output()
		if f := strings.Fields(string(out)); len(f) > 0 {
			pid, _ := strconv.Atoi(f[0])
			return sh, pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the shell never started its child")
	return nil, 0
}

func TestKillStopsAnAgentStartedInAShellOfOurs(t *testing.T) {
	m, reg, _ := testManager(t)
	sh, agent := shellWithAgent(t, m)
	m.discover = func() ([]attach.Process, error) {
		return []attach.Process{{Pid: agent, Command: "claude", Args: []string{"claude"}}}, nil
	}
	sess := reg.Upsert("claude-code", "pid:"+strconv.Itoa(agent), func(s *protocol.Session) {
		s.Attachment = protocol.AttachNative
		s.State = protocol.StateWaitingInput
	})

	if err := m.Kill(sess.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for alive(agent) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(agent) {
		t.Fatal("the agent is still running")
	}
	for time.Now().Before(deadline) {
		if s, _ := reg.Get(sess.ID); s.State == protocol.StateExited {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s, _ := reg.Get(sess.ID); s.State != protocol.StateExited {
		t.Errorf("session state = %s, want EXITED", s.State)
	}
	// Only the agent: the shell it was in carries on.
	time.Sleep(200 * time.Millisecond)
	if s, _ := reg.Get(sh.ID); s.State.Terminal() {
		t.Errorf("the shell ended too (%s); closing the agent must leave it", s.State)
	}
}

func TestKillLeavesAnAgentInSomeoneElsesTerminal(t *testing.T) {
	m, reg, _ := testManager(t)
	_, _ = shellWithAgent(t, m) // a shell of ours exists, but the agent is not in it

	other := exec.Command("sleep", "300")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	pid := other.Process.Pid
	m.discover = func() ([]attach.Process, error) {
		return []attach.Process{{Pid: pid, Command: "claude", Args: []string{"claude"}}}, nil
	}
	sess := reg.Upsert("claude-code", "pid:"+strconv.Itoa(pid), func(s *protocol.Session) {
		s.Attachment = protocol.AttachTmux
		s.State = protocol.StateWaitingInput
	})

	err := m.Kill(sess.ID)
	if !errors.Is(err, errNotOurs) {
		t.Fatalf("Kill = %v, want errNotOurs", err)
	}
	if !alive(pid) {
		t.Fatal("an agent outside muxscale's shells was stopped")
	}
}
