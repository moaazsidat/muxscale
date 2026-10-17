package attach

import (
	"net"
	"os/exec"
	"testing"
)

func TestPeerPIDFindsTheConnectingProcess(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("no lsof")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// curl connects and holds the connection open while the server looks.
	cmd := exec.Command("curl", "-s", "--max-time", "5", "http://"+ln.Addr().String())
	if err := cmd.Start(); err != nil {
		t.Skip("no curl")
	}
	defer cmd.Process.Kill()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	pid, err := PeerPID(conn.RemoteAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if pid != cmd.Process.Pid {
		t.Fatalf("pid = %d, want curl's %d", pid, cmd.Process.Pid)
	}

	if _, err := PeerPID("10.1.2.3:4455"); err == nil {
		t.Fatal("a non-loopback address was resolved")
	}
}
