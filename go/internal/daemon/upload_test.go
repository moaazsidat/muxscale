package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

func upload(t *testing.T, mux *http.ServeMux, session, name string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/upload?session="+session+"&name="+name+"&by=phone", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// An attachment lands inside the session's project, where every agent's file
// tools can reach it, in a directory git never sees.
func TestUploadLandsInTheProject(t *testing.T) {
	d, mux := authDaemon(t)
	cwd := t.TempDir()
	s := d.reg.Upsert("claude-code", "pid:1", func(s *protocol.Session) { s.Cwd = cwd })

	png := []byte("\x89PNG\r\n\x1a\nnot really")
	var paths []string
	for range 2 {
		rec := upload(t, mux, s.ID, "My%20Photo%20(1).png", png)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var got struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Size != int64(len(png)) || filepath.Dir(got.Path) != filepath.Join(cwd, ".muxscale", "uploads") {
			t.Fatalf("saved %+v", got)
		}
		if strings.ContainsAny(filepath.Base(got.Path), " ()") || !strings.HasSuffix(got.Path, "My-Photo-1.png") && !strings.HasSuffix(got.Path, "My-Photo-1-1.png") {
			t.Errorf("name %q", filepath.Base(got.Path))
		}
		if b, _ := os.ReadFile(got.Path); !bytes.Equal(b, png) {
			t.Error("content changed on the way")
		}
		paths = append(paths, got.Path)
	}
	if paths[0] == paths[1] {
		t.Error("a second file with the same name overwrote the first")
	}
	if b, _ := os.ReadFile(filepath.Join(cwd, ".muxscale", ".gitignore")); string(b) != "*\n" {
		t.Errorf(".gitignore = %q", b)
	}
}

func TestUploadWithoutAProjectUsesTheStateDir(t *testing.T) {
	d, mux := authDaemon(t)
	d.stateDir = t.TempDir()
	s := d.reg.Upsert("codex", "pid:2", nil)
	rec := upload(t, mux, s.ID, "../../etc/passwd", []byte("x"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got struct{ Path string }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !strings.HasPrefix(got.Path, filepath.Join(d.stateDir, "uploads")) || !strings.HasSuffix(got.Path, "-passwd") {
		t.Errorf("saved at %q", got.Path)
	}
}

func TestUploadRefusesTooLargeAndUnknown(t *testing.T) {
	d, mux := authDaemon(t)
	s := d.reg.Upsert("codex", "pid:3", func(s *protocol.Session) { s.Cwd = t.TempDir() })
	if rec := upload(t, mux, s.ID, "big.bin", make([]byte, maxUpload+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize -> %d", rec.Code)
	}
	if ents, _ := os.ReadDir(filepath.Join(s.Cwd, ".muxscale", "uploads")); len(ents) != 0 {
		t.Errorf("an oversize upload left %d files", len(ents))
	}
	if rec := upload(t, mux, "nope", "a.png", []byte("x")); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown session -> %d", rec.Code)
	}
}
