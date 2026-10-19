// Package builtin wires the adapters compiled into this binary.
//
// This is the one place allowed to import every agent package, and it exists so
// that nothing else has to. If a file outside internal/adapter/<agent> and this
// package imports a specific agent, the seam has leaked -- which is the exact
// failure M3.5 is designed to surface, so it is worth noticing rather than
// working around.
package builtin

import (
	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/bob"
	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/adapter/codex"
	"github.com/muxscale/muxscale/go/internal/adapter/copilot"
	"github.com/muxscale/muxscale/go/internal/adapter/gemini"
	"github.com/muxscale/muxscale/go/internal/adapter/generic"
	"github.com/muxscale/muxscale/go/internal/adapter/hermes"
	"github.com/muxscale/muxscale/go/internal/adapter/kimi"
	"github.com/muxscale/muxscale/go/internal/adapter/kiro"
	"github.com/muxscale/muxscale/go/internal/adapter/omp"
	"github.com/muxscale/muxscale/go/internal/adapter/opencode"
	"github.com/muxscale/muxscale/go/internal/adapter/pi"
)

// DefaultOpenCodeURL is where OpenCode's local server listens. Overridable
// because the port is configurable and a user running two of them is not doing
// anything unreasonable.
const DefaultOpenCodeURL = "http://127.0.0.1:4096"

// Options configures the built-in set.
type Options struct {
	// OpenCodeURL overrides DefaultOpenCodeURL.
	OpenCodeURL string
}

// Registry returns a registry holding every built-in adapter.
func Registry(opt Options) *adapter.Registry {
	if opt.OpenCodeURL == "" {
		opt.OpenCodeURL = DefaultOpenCodeURL
	}
	r := adapter.NewRegistry()
	// MustRegister is right here and only here: these are compiled in, so a
	// duplicate or malformed ID is a programming error that must not ship.
	r.MustRegister(
		claudecode.New(),
		codex.New(),
		hermes.New(),
		omp.New(),
		opencode.New(opt.OpenCodeURL),
		pi.New(),
		gemini.New(),
		copilot.New(),
		kimi.New(),
		kiro.New(),
		bob.New(),
	)
	// Detection-only agents. They are registered rather than left to the
	// binary-name table so that everything muxscale knows how to recognise is
	// described in one place, with its real capabilities attached -- the table
	// could say "this is aider" but not "and there is no transcript for it".
	for _, s := range generic.All() {
		r.MustRegister(generic.New(s))
	}
	return r
}
