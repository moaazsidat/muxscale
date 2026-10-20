# muxscale

Monitor and unblock your coding agents from your phone.

Agents run autonomously for minutes, then block — on a permission prompt, an ambiguous
requirement, a failing test. You're away from the keyboard and the work is stranded behind
an SSH session you can't comfortably reach. muxscale watches every agent session on your
machines, notifies you the moment one blocks, and lets you unblock it with a sentence.

**Status: M0 complete, M1 mostly complete.** Claude Code, omp, and Codex, terminal only —
no phone app and no cloud yet. See [the technical spec](docs/20260729-technical-spec.md) for the full
design, [§13 Milestones](docs/20260729-technical-spec.md#13-milestones) for what lands
when, [the omp adapter spec](docs/20261018-omp-adapter.md), and [the Codex adapter
spec](docs/20261018-codex-adapter.md). Adopting agents that are *already running* is specified and partly built in
[the adoption spec](docs/20260925-adopting-existing-sessions.md). pi, OpenCode, Gemini CLI, GitHub Copilot CLI, Kimi
Code, Kiro CLI and IBM Bob are read too, most of them not yet verified against a real install; see
[more agents](docs/20261019-more-agents.md) for what each one gives and where its format came from.

## What works today

**Seeing your agents**

- Every Claude Code, omp, and Codex session on the machine is discovered and tracked,
  **including ones muxscale never launched and ones that started before it was installed** —
  via process scan, tmux enumeration, terminal breadcrumbs, and the agents' own session
  indexes and files.
  omp liveness is the writer lease it holds, so a session whose process is gone is reported
  ended rather than guessed at from a file's age.
- Full structured transcripts: turns, tool calls, results, diffs, token usage, subagent
  threads — parsed straight from Claude Code's JSONL (verbatim Messages API shape), omp's
  session tree (`toolCall` blocks, `toolResult` messages, thinking, usage, forks), and
  Codex's rollouts (reasoning, `function_call`, patches, token counts).
- Free metadata: AI-generated session title, git branch, model, agent version.
- Everything persists to SQLite and survives a daemon restart with stable session IDs.

**Knowing when they block**

- Session state is **authoritative, not guessed**, when hooks or an extension are
  installed: Claude Code's `Stop`, omp's `agent_end`, and Codex's `Stop` mean the turn ended
  and the agent is waiting on you. Confidence 1.0, no regexes. Without them, state is
  inferred from the transcript, capped below 1.0.
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
- `muxscale status` says how many sessions accept replies, and what a terminal would need
  to make the rest adoptable.

**Watching them work**

- A headless VT emulator maintains screen state for wrapped sessions, so clients get a
  grid with row deltas rather than a raw ANSI stream. 18 MB/s throughput; 9µs row diffs.

## Quick start

```bash
cd go && go build -o muxscale ./cmd/muxscale

./muxscale up                # start muxscale now and at every login
./muxscale connect           # serve it to your phone, and show the QR code to scan
```

That is the whole setup. Your running agents appear in `muxscale ls` within seconds, with
nothing restarted. `muxscale status` says what is running and reachable, and how to fix
what is not; `muxscale down` removes everything muxscale set up.

**Claude Code hooks, the omp extension, and Codex hooks need no step of their own.** The
daemon sets them up each time it starts (blocked-state accuracy and remote approvals): it adds muxscale's entries and the
token they authenticate with to `~/.claude/settings.json`, backing the file up first and
changing it only when something is missing or out of date. Every hook already there is
preserved. Claude Code sessions started after that report their state exactly. For omp it
writes one file, `~/.omp/agent/extensions/muxscale.ts`; omp discovers it with no settings
edit, and removing the file (or `muxscale down`) turns exact state back into inference.
Codex has no HTTP hook transport, so the daemon installs command hooks in
`~/.codex/hooks.json` that run `muxscale hook-relay`; Codex requires non-managed hooks to be
trusted once with `/hooks` before they run. `muxscale status` says when that step is
outstanding. Run the daemon with `--no-hooks` to leave all three alone.

```bash
./muxscale ls                 # sessions, and pending approvals with their IDs
./muxscale ls -w              # follow events live
./muxscale approve ID         # or deny ID
./muxscale send ID "continue" # reply to a session
./muxscale run -- claude      # start a session muxscale owns, so it accepts new prompts
./muxscale audit              # who did what, when
```

**`connect`** serves the daemon on the machine's own Tailscale when it has one, and
otherwise on a node of muxscale's own, which logs in through a link it prints. The daemon
remembers it across restarts; `muxscale connect off` stops it. The person whose Tailscale
login the machine uses is permitted automatically; let anyone else in with
`muxscale acl allow them@example.com`.

The code encodes `muxscale://connect?url=https://<host>.ts.net:4443` — the address and
nothing else. There is no secret in it to leak: who may connect is decided by Tailscale
identity and the ACL. `--link` prints just the URL, `--json` both.

Restarting the daemon is only ever necessary for `muxscale run` sessions — those are the
ones it holds a PTY for. Sessions in iTerm2, tmux, or a plain terminal survive and are
re-adopted.

### Gotchas

- **A daemon you started by hand is supervised by nobody.** Nothing restarts it when it
  exits, and nothing replaces the binary when you rebuild — so an old build keeps running
  silently, serving an old API. `muxscale up` runs it as a service instead. Check what is
  actually running, and with which flags, before believing a feature exists:

  ```bash
  ps -o pid=,ppid=,lstart=,args= -ax | grep '[m]uxscale.*daemon'
  ```

- **A tailnet *node* being `Running` does not mean the listener is up.** The daemon can fail
  to bind its port (address changed, port taken, certificate trouble) and carry on serving
  loopback only, because monitoring must never depend on the network. `muxscale status`
  checks the listener, and the OS can confirm it:

  ```bash
  lsof -nP -iTCP:4443 -sTCP:LISTEN
  ```

- **Killing the daemon drops any approval it is holding open.** A `PermissionRequest` hook
  waits inside the daemon for up to `--hold`; if the daemon dies first, the hook returns "no
  decision" and Claude Code asks locally instead. Fail-open by design, but the prompt comes
  back in the terminal rather than to your phone.

- **Restarting the daemon kills `muxscale run` sessions and only those.** `muxscale ls` does
  not show the attachment, so check before you kill anything:

  ```bash
  muxscale ls --json | grep -o '"attachment":"wrap"'    # PTYs the daemon owns
  pgrep -P "$(pgrep -f 'muxscale.*daemon' | head -1)"   # its children; empty = nothing to lose
  ```

> **Heads-up before installing globally.** The decision hooks hold a permission prompt open
> for up to the configured hold window (default 120s) while waiting for an answer. Until the
> phone app exists, answer from `muxscale ls` and `muxscale approve` or the prompt falls through to Claude
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
- **omp ≥ 18.12.2.** That is where the session tree, writer lease, and extension surface
  were verified. Older sessions still parse; `muxscale status` says what is missing.
- **omp approvals are typed.** omp only asks when `tools.approvalMode` is not `yolo` (its
  default). Its extension reports each prompt, but nothing in its API answers one, so the
  phone's answer is typed into omp's terminal as omp's own keys (`Ctrl+Q` to approve, `Esc`
  to deny). Rebinding the approve key or using Vim mode stops that from landing; the prompt
  then stays answerable at the keyboard.
- **Codex hooks need trust.** The daemon installs them, but Codex skips non-managed hooks
  until you open `/hooks` in Codex and trust them. Until then Codex sessions still appear
  with transcripts and a derived state; `muxscale status` says what is missing.
- **Codex ≥ 0.163.0.** The thread index, rollout shape, and command-hook schema were
  verified there. Older sessions still parse; the hook installer is skipped below the
  floor.

## Layout

```
proto/      wire protocol (source of truth; M2)
go/         daemon + CLI
  internal/adapter/claudecode/   hooks, transcript parser + tailer, version gate, installer
  internal/adapter/omp/          session parser + tailer, writer lease, extension installer
  internal/adapter/codex/        thread index + rollout parser, process join, hook installer
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
  and results matching exactly. The omp parser runs against real 18.12.2 files, and the
  extension has been exercised end to end against a live omp process.
- The VT emulator has a randomized property test: 400 rounds of arbitrary escape sequences,
  asserting that applying deltas to a client replica always reproduces the daemon's grid.
  It found a real wide-character bug on its first run.
- tmux adoption is tested against a real tmux server on a private socket, so it never
  touches your own sessions.
