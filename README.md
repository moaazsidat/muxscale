# muxscale

Monitor and unblock your coding agents from your phone.

Agents run autonomously for minutes, then block — on a permission prompt, an ambiguous
requirement, a failing test. You're away from the keyboard and the work is stranded behind
an SSH session you can't comfortably reach. muxscale watches every agent session on your
machines, notifies you the moment one blocks, and lets you unblock it with a sentence.

**Status: M0.** Claude Code only, terminal only. No phone app, no cloud, no terminal
streaming yet. See [the technical spec](docs/20260729-technical-spec.md) for the full design
and [§13 Milestones](docs/20260729-technical-spec.md#13-milestones) for what lands when.

## What works today

- Claude Code sessions are discovered and tracked through Claude Code's own HTTP hooks —
  including sessions muxscale never launched.
- Session state is **authoritative, not guessed**: `Stop` means the turn ended, and the
  session is waiting on you. No prompt-regex heuristics.
- **Remote approvals work end to end.** A `PermissionRequest` hook is held open while you're
  asked; your answer becomes the literal `{"behavior":"allow"|"deny"}` returned to Claude
  Code. Structured data, not simulated keystrokes.
- Transcripts parse straight from Claude Code's JSONL into structured turns and tool calls.

## Quick start

```bash
cd go && go build -o muxscale ./cmd/muxscale

./muxscale doctor            # check your setup
./muxscale hooks install     # shows the exact settings.json diff, then asks
export MUXSCALE_HOOK_TOKEN=…  # printed by hooks install; add to your shell profile
./muxscale daemon            # in one terminal
./muxscale ls                # in another; start a Claude Code session and watch
./muxscale watch             # live event stream
./muxscale approvals         # pending approvals; answer with `approvals allow|deny ID`
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
  internal/adapter/claudecode/   hooks, transcript parser, version gate, installer
  internal/daemon/               hook listener, session registry, state machine
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

The parser is also validated against every real Claude Code transcript on the host machine —
38 files, including a 143 MB / 5,372-event session, parsed in 1.4 s at 38 MB RSS with tool
calls and results matching exactly.
