# muxscale

Monitor and unblock your coding agents from your phone.

Agents run autonomously for minutes, then block — on a permission prompt, an ambiguous
requirement, a failing test. You're away from the keyboard and the work is stranded behind
an SSH session you can't comfortably reach. muxscale watches every agent session on your
machines, notifies you the moment one blocks, and lets you unblock it with a sentence.

**Status: M0 complete, M1 mostly complete.** Claude Code only, terminal only — no phone app
and no cloud yet. See [the technical spec](docs/20260729-technical-spec.md) for the full
design and [§13 Milestones](docs/20260729-technical-spec.md#13-milestones) for what lands
when. Adopting agents that are *already running* is specified and partly built in
[the adoption spec](docs/20260925-adopting-existing-sessions.md).

## What works today

**Seeing your agents**

- Every Claude Code session on the machine is discovered and tracked, **including ones
  muxscale never launched and ones that started before it was installed** — via process
  scan, tmux enumeration, and Claude Code's own session files.
- Full structured transcripts: turns, tool calls, results, diffs, token usage, subagent
  threads — parsed straight from Claude Code's JSONL, which is verbatim Messages API shape.
- Free metadata: AI-generated session title, git branch, model, agent version.
- Everything persists to SQLite and survives a daemon restart with stable session IDs.

**Knowing when they block**

- Session state is **authoritative, not guessed**, when hooks are installed: `Stop` means
  the turn ended and the agent is waiting on you. Confidence 1.0, no regexes.
- Without hooks, state is inferred from the transcript and capped at 0.7 confidence, with
  the evidence recorded so `debug state` can explain itself.

**Unblocking them**

- **Remote approvals, end to end.** A `PermissionRequest` hook is held open while you're
  asked; your answer becomes the literal `{"behavior":"allow"|"deny"}` returned to Claude
  Code. Structured data, not simulated keystrokes.
- Send new prompts to sessions muxscale owns (`muxscale run`) or has adopted from tmux.
- **Input fencing**: a reply is bound to the exact turn that asked. If the agent moved on,
  the input is refused with an explanation rather than answering the wrong question.

**Adopting agents you already had running**

- A Claude Code session started in iTerm2 *before muxscale existed*, in no multiplexer, is
  **writable from the CLI** — the daemon joins the pane to the process on the tty and types
  through iTerm2's scripting interface. No restart, no re-launch, no shim.
- Those sessions carry a **sampled** screen (polled at ~2 Hz, not a live stream) and say so.
- Its transcript is matched by **process start time**, not "newest file in the project", so
  two agents in one project each get their own conversation.
- A write is refused unless the adopted agent is still the process sitting in that pane —
  otherwise a reply would land on a shell prompt after the agent exited.
- `muxscale adopt` reports what was adopted, what it can reply to, and what a terminal would
  need to be adoptable at all.

**Watching them work**

- A headless VT emulator maintains screen state for wrapped sessions, so clients get a
  grid with row deltas rather than a raw ANSI stream. 18 MB/s throughput; 9µs row diffs.

## Quick start

```bash
cd go && go build -o muxscale ./cmd/muxscale

./muxscale daemon            # in one terminal
./muxscale ls                # in another — your running agents appear immediately,
                             #   with no hooks installed and nothing restarted
```

That much works with zero setup. For blocked-state accuracy and remote approvals, install
the hooks:

```bash
./muxscale doctor             # check your setup
./muxscale hooks install      # shows the exact settings.json diff, then asks
export MUXSCALE_HOOK_TOKEN=…  # printed by hooks install; add to your shell profile

./muxscale watch              # live event stream
./muxscale approvals          # pending approvals; answer with `approvals allow|deny ID`
./muxscale run -- claude      # launch a session muxscale owns, so it accepts new prompts
./muxscale send ID "continue" # reply to a session
./muxscale audit              # who did what, when
```

`hooks install` writes a timestamped backup, preserves every existing hook, and is
idempotent. `hooks uninstall` removes exactly muxscale's tagged entries.

To serve the daemon to your phone, turn the tailnet listener on — **without restarting
anything**, including the daemon:

```bash
./muxscale tailnet on --system          # serve on the machine's existing Tailscale node
./muxscale acl allow you@example.com    # the second gate; Tailscale identity is the first
./muxscale adopt                        # what did it find, and can I reply to it?
./muxscale qr                           # scan this with the phone to connect the app
```

`muxscale qr` encodes `muxscale://connect?url=https://<host>.ts.net:4443` — the address and
nothing else. There is no secret in it to leak: who may connect is decided by Tailscale
identity and the ACL, and it says so when the ACL is still empty. `--link` prints just the
URL, `--json` both.

`muxscale tailnet on` (no `--system`) joins the tailnet as its own node instead, which gives
up less but needs to register with Tailscale's control plane. Restarting the daemon is only
ever necessary for `muxscale run` sessions — those are the ones it holds a PTY for. Sessions
in iTerm2, tmux, or a plain terminal survive and are re-adopted.

### Gotchas

- **`muxscale up` will not give you `--use-system-ts`.** The unit it writes runs
  `muxscale daemon` (or `muxscale daemon --tailnet` with `up --tailnet`), and `--tailnet`
  means *a new tsnet node* (`muxscale-<host>.<tailnet>.ts.net`), not the machine's existing
  Tailscale identity. Nothing writes `--use-system-ts` into a service unit. To serve on the
  system node, start the daemon by hand with that flag, or start it plainly and run
  `muxscale tailnet on --system`.

- **A daemon you started by hand is supervised by nobody.** Nothing restarts it when it
  exits, and nothing replaces the binary when you rebuild — so an old build keeps running
  silently, serving an old API. Check what is actually running, and with which flags, before
  believing a feature exists:

  ```bash
  ps -o pid=,ppid=,lstart=,args= -ax | grep '[m]uxscale.*daemon'
  ```

- **A tailnet *node* being `Running` does not mean the listener is up.** The daemon can fail
  to bind its port (address changed, port taken, certificate trouble) and carry on serving
  loopback only, because monitoring must never depend on the network. `muxscale status` shows
  the node; `muxscale tailnet` shows whether anything is actually listening:

  ```bash
  muxscale tailnet                        # on [system] + URL, or off
  lsof -nP -iTCP:4443 -sTCP:LISTEN        # the same answer, from the OS
  ```

- **Killing the daemon drops any approval it is holding open.** A `PermissionRequest` hook
  waits inside the daemon for up to `--hold`; if the daemon dies first, the hook returns "no
  decision" and Claude Code asks locally instead. Fail-open by design, but the prompt comes
  back in the terminal rather than to your phone.

- **Restarting the daemon kills `muxscale run` sessions and only those.** `muxscale ls` does
  not show the attachment, so check before you kill anything:

  ```bash
  muxscale adopt | grep wrap                            # PTYs the daemon owns
  pgrep -P "$(pgrep -f 'muxscale.*daemon' | head -1)"   # its children; empty = nothing to lose
  ```

> **Heads-up before installing globally.** The decision hooks hold a permission prompt open
> for up to the configured hold window (default 120s) while waiting for an answer. Until the
> phone app exists, answer from `muxscale approvals` or the prompt falls through to Claude
> Code's own after the timeout. Start with `--hold 10s` if that sounds annoying.

## Design commitments

These are constraints, not aspirations. Each has tests.

- **Fail open, always.** Daemon down, bad token, malformed payload, nobody answering — every
  path returns "no decision" and Claude Code asks locally. A monitoring tool that can stall
  the agent it monitors is worse than no monitoring tool.
- **muxscale never decides.** It relays a question to a human and returns that human's
  answer. No policy engine, no auto-approve, no learned allowlists. It shows the *literal*
  tool arguments, never the agent's description of them.
- **Loopback only.** The hook endpoint refuses to bind to anything but 127.0.0.1.
- **Your settings.json is yours.** Never clobbered, always backed up, fully reversible.
- **Claude Code ≥ 2.1.196.** That's where `prompt_id` landed, and `prompt_id` is the fence
  token that stops a reply composed offline from answering a later question. Below the
  floor, muxscale refuses to install hooks rather than ship a weaker safety model.

## Layout

```
proto/      wire protocol (source of truth; M2)
go/         daemon + CLI
  internal/adapter/claudecode/   hooks, transcript parser + tailer, version gate, installer
  internal/attach/               pty wrap, input encoding + fencing, discovery, tmux
  internal/daemon/               hook listener, registry, manager, state machine
  internal/store/                SQLite: sessions, transcripts, approvals, audit
  internal/vt/                   headless VT emulator, grid diffing
  internal/protocol/             shared types
rust/       client core: codec, crypto, grid model (M3)
app/        Expo iOS/Android app (M3)
cloud/      Workers control plane, relay, push notary (M2)
adapters/   signed manifests
fixtures/   shared test data across implementations
docs/       spec + docs site source
```

## Tests

```bash
cd go && go test -race ./...
```

Beyond the unit tests, three things are validated against reality rather than fixtures:

- The transcript parser runs against every real Claude Code session file on the host — 38
  files including a 143 MB / 5,372-event one, parsed in 1.4 s at 38 MB RSS, with tool calls
  and results matching exactly.
- The VT emulator has a randomized property test: 400 rounds of arbitrary escape sequences,
  asserting that applying deltas to a client replica always reproduces the daemon's grid.
  It found a real wide-character bug on its first run.
- tmux adoption is tested against a real tmux server on a private socket, so it never
  touches your own sessions.
