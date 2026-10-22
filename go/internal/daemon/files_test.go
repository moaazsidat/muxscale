package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

func getFiles(t *testing.T, mux *http.ServeMux, route, session, p string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{"session": {session}, "path": {p}}
	req := httptest.NewRequest(http.MethodGet, route+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A session in a subfolder of a repository browses the whole repository; a
// session outside any repository browses its own folder. A repository at the
// home directory -- dotfiles -- does not make every session a session in ~.
func TestProjectRootIsTheWorkTreeButNeverHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "code", "shop")
	write(t, repo, ".git/HEAD", "ref: refs/heads/main\n")
	write(t, repo, "src/app/x.ts", "")
	loose := filepath.Join(home, "notes", "drafts")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatal(err)
	}

	for cwd, want := range map[string]string{
		filepath.Join(repo, "src", "app"): repo,
		repo:                              repo,
		loose:                             loose,
		home:                              home,
	} {
		got, err := projectRoot(cwd)
		if err != nil || got != want {
			t.Errorf("projectRoot(%s) = %q, %v; want %q", cwd, got, err, want)
		}
	}
	for _, cwd := range []string{"", "/", filepath.Join(home, "missing")} {
		if got, err := projectRoot(cwd); err == nil {
			t.Errorf("projectRoot(%q) = %q, want an error", cwd, got)
		}
	}
}

func TestListingShowsFoldersFirstAndNeverLeavesTheProject(t *testing.T) {
	d, mux := authDaemon(t)
	dir := t.TempDir()
	outside := t.TempDir()
	write(t, outside, "secret.txt", "not yours")
	write(t, dir, "README.md", "# hi\n")
	write(t, dir, "b.go", "package b\n")
	write(t, dir, "src/a.ts", "export {}\n")
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src", filepath.Join(dir, "inside")); err != nil {
		t.Fatal(err)
	}
	s := d.reg.Upsert("claude-code", "pid:1", func(s *protocol.Session) { s.Cwd = dir })

	l := decode[protocol.FileListing](t, getFiles(t, mux, "/v1/files/list", s.ID, ""))
	var names []string
	kinds := map[string]string{}
	for _, e := range l.Entries {
		names = append(names, e.Name)
		kinds[e.Name] = e.Kind
	}
	if got := strings.Join(names, " "); got != "inside src b.go escape README.md" {
		t.Errorf("order = %q", got)
	}
	if kinds["inside"] != "dir" || kinds["escape"] != "link" {
		t.Errorf("links: inside=%s escape=%s", kinds["inside"], kinds["escape"])
	}

	sub := decode[protocol.FileListing](t, getFiles(t, mux, "/v1/files/list", s.ID, "inside"))
	if len(sub.Entries) != 1 || sub.Entries[0].Name != "a.ts" {
		t.Errorf("a link inside the project is followed: %+v", sub.Entries)
	}

	for _, p := range []string{"../", "/etc", "src/../../x", "escape", "escape/secret.txt"} {
		if rec := getFiles(t, mux, "/v1/files/list", s.ID, p); rec.Code == http.StatusOK {
			t.Errorf("list %q: status 200, want a refusal", p)
		}
		if rec := getFiles(t, mux, "/v1/files/read", s.ID, p); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "not yours") {
			t.Errorf("read %q: %d %s", p, rec.Code, rec.Body)
		}
	}
	if rec := getFiles(t, mux, "/v1/files/list", "nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown session: %d", rec.Code)
	}
}

func TestReadingSaysWhyAFileIsNotShown(t *testing.T) {
	d, mux := authDaemon(t)
	dir := t.TempDir()
	write(t, dir, "notes.md", "# Plan\n")
	write(t, dir, "logo.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	write(t, dir, "big.log", strings.Repeat("x", maxTextRead+10))
	s := d.reg.Upsert("claude-code", "pid:1", func(s *protocol.Session) { s.Cwd = dir })

	md := decode[protocol.FileContent](t, getFiles(t, mux, "/v1/files/read", s.ID, "notes.md"))
	if md.Text != "# Plan\n" || md.Binary || md.SHA256 == "" || md.Size != 7 {
		t.Errorf("markdown: %+v", md)
	}
	png := decode[protocol.FileContent](t, getFiles(t, mux, "/v1/files/read", s.ID, "logo.png"))
	if !png.Binary || png.Text != "" || png.MIME != "image/png" {
		t.Errorf("png: %+v", png)
	}
	big := decode[protocol.FileContent](t, getFiles(t, mux, "/v1/files/read", s.ID, "big.log"))
	if !big.TooLarge || big.Text != "" || big.Size != maxTextRead+10 {
		t.Errorf("big: too_large=%v size=%d text=%d", big.TooLarge, big.Size, len(big.Text))
	}
	if rec := getFiles(t, mux, "/v1/files/read", s.ID, "."); rec.Code != http.StatusBadRequest {
		t.Errorf("reading a folder: %d", rec.Code)
	}
}

// A project's own HTML, served from the daemon's origin, would run with the
// phone's access to the API. Only raster images go out as themselves.
func TestRawServesImagesAndNothingThatCouldRun(t *testing.T) {
	d, mux := authDaemon(t)
	dir := t.TempDir()
	write(t, dir, "logo.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	write(t, dir, "page.html", "<script>fetch('/v1/approvals')</script>")
	write(t, dir, "icon.svg", `<svg xmlns="http://www.w3.org/2000/svg"><script>1</script></svg>`)
	s := d.reg.Upsert("claude-code", "pid:1", func(s *protocol.Session) { s.Cwd = dir })

	rec := getFiles(t, mux, "/v1/files/raw", s.ID, "logo.png")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("png: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, p := range []string{"page.html", "icon.svg"} {
		rec := getFiles(t, mux, "/v1/files/raw", s.ID, p)
		h := rec.Header()
		if h.Get("Content-Type") != "application/octet-stream" || h.Get("Content-Disposition") != "attachment" ||
			!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s served as %q, %q, csp %q", p, h.Get("Content-Type"), h.Get("Content-Disposition"), h.Get("Content-Security-Policy"))
		}
	}
}

func TestChangesPutTheAgentsOwnFilesFirst(t *testing.T) {
	d, mux := authDaemon(t)
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, ".gitignore", "dist/\n")
	write(t, dir, "src/api.ts", "v1\n")
	write(t, dir, "README.md", "old\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "init")

	write(t, dir, "src/api.ts", "v2\n")           // the agent edited it
	write(t, dir, "src/limit.ts", "new\n")        // the agent wrote it, via a patch
	write(t, dir, "README.md", "changed by hand") // changed, not by the agent
	write(t, dir, "dist/out.js", "")              // ignored
	s := d.reg.Upsert("codex", "pid:1", func(s *protocol.Session) { s.Cwd = filepath.Join(dir, "src") })

	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	events := []protocol.TranscriptEvent{
		{SessionID: s.ID, Seq: 1, Kind: protocol.EventToolCall, At: t0, ToolName: "Edit",
			ToolInput: map[string]any{"file_path": filepath.Join(dir, "src", "api.ts")}},
		{SessionID: s.ID, Seq: 2, Kind: protocol.EventToolCall, At: t0.Add(time.Minute), ToolName: "apply_patch",
			ToolInput: map[string]any{"input": "*** Begin Patch\n*** Add File: limit.ts\n+new\n*** End Patch"}},
		{SessionID: s.ID, Seq: 3, Kind: protocol.EventToolCall, At: t0.Add(2 * time.Minute), ToolName: "Write",
			ToolInput: map[string]any{"file_path": "/elsewhere/outside.txt"}},
		{SessionID: s.ID, Seq: 4, Kind: protocol.EventToolCall, At: t0.Add(3 * time.Minute), ToolName: "Read",
			ToolInput: map[string]any{"file_path": filepath.Join(dir, "README.md")}},
	}
	if err := d.store.AppendEvents(events); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/files/changes?session="+s.ID, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	mux.ServeHTTP(rec, req)
	got := decode[protocol.FileChanges](t, rec)

	if !got.Git || got.Root != dir {
		t.Errorf("root %q git %v", got.Root, got.Git)
	}
	var rows []string
	for _, c := range got.Changes {
		rows = append(rows, c.Path+":"+c.Status+map[bool]string{true: ":agent"}[c.ByAgent])
	}
	want := "src/limit.ts:untracked:agent src/api.ts:modified:agent README.md:modified"
	if strings.Join(rows, " ") != want {
		t.Errorf("changes =\n  %s\nwant\n  %s", strings.Join(rows, " "), want)
	}
}
