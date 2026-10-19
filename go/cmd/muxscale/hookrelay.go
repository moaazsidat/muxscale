package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/daemon"
)

// cmdHookRelay is the command side of a command-hook transport.
//
// Codex hooks are commands: Codex writes one JSON object to stdin and reads a
// decision from stdout. This reads that object, posts it to the daemon's
// loopback endpoint, and prints the daemon's answer verbatim. Every failure is
// silence and exit 0 -- a monitoring tool must never stall the agent it
// monitors.
func cmdHookRelay(args []string) error {
	agent, addr, tokenFile, quiet := "", defaultAddr, "", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--agent":
			if i+1 < len(args) {
				i++
				agent = args[i]
			}
		case "--addr":
			if i+1 < len(args) {
				i++
				addr = args[i]
			}
		case "--token-file":
			if i+1 < len(args) {
				i++
				tokenFile = args[i]
			}
		case "--quiet":
			// For agents that read a hook's stdout as something else -- Bob
			// adds it to the model's context -- and have no decision to take
			// from it: deliver the event, print nothing.
			quiet = true
		default:
			return fmt.Errorf("unknown option %q for `muxscale hook-relay`", args[i])
		}
	}
	if agent == "" {
		return fmt.Errorf("hook-relay needs --agent")
	}
	if tokenFile == "" {
		dir, err := daemon.DefaultStateDir()
		if err != nil {
			return nil
		}
		tokenFile = filepath.Join(dir, "hook-token")
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil || len(bytes.TrimSpace(token)) == 0 {
		return nil // no credential: fail open, print nothing
	}

	body, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	url := "http://" + addr + "/v1/hook/" + agent
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	// The hook runs as a child of the agent. Telling the daemon the parent's
	// pid is what ties this session id to the process row the scan will find;
	// the relay's own pid would alias a process that is about to exit.
	req.Header.Set("X-Muxscale-Peer-PID", strconv.Itoa(os.Getppid()))

	// Codex allows 600 s for ordinary hooks. Stay above that; the daemon's
	// own hold decides when to give up and return no decision.
	client := &http.Client{Timeout: 11 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || quiet {
		return nil
	}
	_, _ = io.Copy(os.Stdout, resp.Body)
	return nil
}
