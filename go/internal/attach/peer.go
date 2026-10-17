package attach

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// PeerPID finds the local process on the other end of a loopback connection,
// from the address the server saw it come from.
//
// This is how a hook is tied to the process that sent it. Claude Code makes
// its hook requests itself, and its payload names its own session but not its
// process -- while the process scan knows the process and its terminal but
// not, for certain, its session. The connection is the one thing both sides
// share.
func PeerPID(remoteAddr string) (int, error) {
	host, port, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return 0, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return 0, fmt.Errorf("%s is not a local connection", remoteAddr)
	}
	// Every process with a TCP socket on that port: the sender, whose local
	// port it is, and this process, whose remote port it is.
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:ESTABLISHED", "-Fp").Output()
	if err != nil {
		return 0, fmt.Errorf("lsof: %w", err)
	}
	self := os.Getpid()
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "p") {
			continue
		}
		pid, err := strconv.Atoi(line[1:])
		if err == nil && pid != self {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("no process owns %s", remoteAddr)
}
