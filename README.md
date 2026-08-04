# muxscale

Monitor and unblock your coding agents from your phone.

Agents run autonomously for minutes, then block — on a permission prompt, an ambiguous
requirement, a failing test. You're away from the keyboard and the work is stranded behind
an SSH session you can't comfortably reach. muxscale watches every agent session on your
machines, notifies you the moment one blocks, and lets you unblock it with a sentence.

**Status: M0 complete, M1 mostly complete.** Claude Code only, terminal only — no phone app
and no cloud yet. See [the technical spec](docs/20260729-technical-spec.md) for the full
design and [§13 Milestones](docs/20260729-technical-spec.md#13-milestones) for what lands
when.

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
