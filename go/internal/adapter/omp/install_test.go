package omp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureExtensionIsIdempotentAndRemovable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(root, "agent"))

	changed, err := EnsureExtension("http://127.0.0.1:4455", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first install reported no change")
	}
	path, _ := ExtensionPath()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), extensionMarker) || !strings.Contains(string(b), `const MUXSCALE_URL = "http://127.0.0.1:4455"`) {
		t.Fatalf("unexpected source:\n%s", b)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	if !ExtensionInstalled("http://127.0.0.1:4455", "tok") {
		t.Error("installed file not recognised")
	}

	changed, err = EnsureExtension("http://127.0.0.1:4455", "tok")
	if err != nil || changed {
		t.Fatalf("second install = %v, %v", changed, err)
	}
	// A new token is a real change: the extension authenticates with it.
	changed, err = EnsureExtension("http://127.0.0.1:4455", "tok2")
	if err != nil || !changed {
		t.Fatalf("token change = %v, %v", changed, err)
	}

	removed, err := RemoveExtension()
	if err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("extension still present: %v", err)
	}
}

// A file someone else put at muxscale's path is theirs. Refusing is better
// than overwriting it and better than deleting it on uninstall.
func TestForeignExtensionIsLeftAlone(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(root, "agent"))
	path, _ := ExtensionPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("export default function mine() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureExtension("http://127.0.0.1:4455", "tok"); err == nil {
		t.Fatal("overwrote a file it did not write")
	}
	if _, err := RemoveExtension(); err == nil {
		t.Fatal("removed a file it did not write")
	}
}
