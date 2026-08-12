package adapter

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
)

// Registry holds the adapters available to a daemon.
//
// Explicitly constructed and passed in, not a package-level global with
// init()-time registration. Tests need a registry containing exactly one
// adapter, or none; and the conformance suite needs to enumerate the real set
// without a test binary's imports deciding what is in it.
type Registry struct {
	mu    sync.RWMutex
	byID  map[string]Adapter
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byID: map[string]Adapter{}}
}

// idRe enforces lowercase kebab-case. The ID appears in config files, on the
// wire, and in storage keys, so it is fixed early rather than discovered to be
// inconsistent once three of them are persisted in users' databases.
var idRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// Register adds an adapter. It returns an error rather than panicking on a
// duplicate or malformed ID so that a manifest-loaded adapter -- which is data,
// and therefore attacker-adjacent -- cannot take the daemon down.
func (r *Registry) Register(a Adapter) error {
	info := a.Info()
	if !idRe.MatchString(info.ID) {
		return fmt.Errorf("adapter ID %q must be lowercase kebab-case", info.ID)
	}
	if info.MinVersion != "" {
		if _, err := ParseVersion(info.MinVersion); err != nil {
			return fmt.Errorf("adapter %s: MinVersion: %w", info.ID, err)
		}
	}
	if info.NewestTested != "" {
		if _, err := ParseVersion(info.NewestTested); err != nil {
			return fmt.Errorf("adapter %s: NewestTested: %w", info.ID, err)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byID[info.ID]; dup {
		return fmt.Errorf("adapter %s is already registered", info.ID)
	}
	r.byID[info.ID] = a
	r.order = append(r.order, info.ID)
	sort.Strings(r.order)
	return nil
}

// MustRegister is for the built-in set wired at startup, where a failure is a
// programming error that should never reach a user.
func (r *Registry) MustRegister(as ...Adapter) {
	for _, a := range as {
		if err := r.Register(a); err != nil {
			panic(err)
		}
	}
}

// Get returns the adapter with the given ID.
func (r *Registry) Get(id string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byID[id]
	return a, ok
}

// All returns every adapter, ordered by ID so that output and iteration are
// deterministic -- a session list whose order changes between runs is a
// diffing nuisance in tests and a flicker in the UI.
func (r *Registry) All() []Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Adapter, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// IDs returns the registered adapter IDs, sorted.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Len reports how many adapters are registered.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}

// Match finds the adapter that claims a process.
//
// Ambiguity is reported rather than resolved. Two adapters matching one process
// means their detect rules overlap, and silently picking the first -- in
// whatever order the map happened to iterate -- would attribute sessions to the
// wrong agent intermittently, which is close to impossible to debug from a bug
// report. The conformance corpus asserts this never happens for the built-in
// set.
func (r *Registry) Match(p Process) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var hits []Adapter
	for _, id := range r.order {
		if r.byID[id].Detect(p) {
			hits = append(hits, r.byID[id])
		}
	}
	switch len(hits) {
	case 0:
		return nil, nil
	case 1:
		return hits[0], nil
	default:
		ids := make([]string, len(hits))
		for i, a := range hits {
			ids[i] = a.Info().ID
		}
		return nil, fmt.Errorf("process %d (%v) matched multiple adapters: %v; "+
			"their detect rules overlap", p.PID, p.Argv, ids)
	}
}
