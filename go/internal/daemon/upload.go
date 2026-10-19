package daemon

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxUpload caps one attached file. A phone photo is a few MB; a PDF or a log
// rarely passes ten. Anything larger is better copied onto the machine some
// other way than over a phone's connection.
const maxUpload = 32 << 20

// uploadDirName is where attachments land inside a session's project. Inside
// it, not beside it: agents confine their file tools to the workspace (Gemini
// CLI refuses paths outside it, Claude Code asks first), and an attachment the
// agent cannot open is no attachment at all.
const uploadDirName = ".muxscale"

// uploadsDir picks where a session's attachments go: <cwd>/.muxscale/uploads,
// made to ignore itself so nothing in it is ever committed; or, for a session
// with no usable project directory, the daemon's own state directory.
func (d *Daemon) uploadsDir(sessionID, cwd string) (string, error) {
	if cwd != "" {
		if fi, err := os.Stat(cwd); err == nil && fi.IsDir() {
			root := filepath.Join(cwd, uploadDirName)
			dir := filepath.Join(root, "uploads")
			if err := os.MkdirAll(dir, 0o700); err == nil {
				// A .gitignore of "*" ignores the directory and itself, so the
				// project's own .gitignore never needs to know muxscale exists.
				ignore := filepath.Join(root, ".gitignore")
				if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
					_ = os.WriteFile(ignore, []byte("*\n"), 0o600)
				}
				return dir, nil
			}
		}
	}
	if d.stateDir == "" {
		return "", errors.New("no directory to save the file in")
	}
	dir := filepath.Join(d.stateDir, "uploads", safeName(sessionID))
	return dir, os.MkdirAll(dir, 0o700)
}

// safeName reduces a file name from a phone to something that survives being
// typed into a terminal: no directories, no spaces, no shell metacharacters.
// The name is shown to the agent as a path, and a path that has to be quoted
// is one an agent will sometimes get wrong.
func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			if !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	s := strings.Trim(b.String(), ".-")
	if ext := filepath.Ext(s); ext != "" {
		s = strings.TrimRight(strings.TrimSuffix(s, ext), "-") + ext
	}
	if s == "" || strings.HasPrefix(s, ".") {
		s = "file" + s
	}
	if len(s) > 80 {
		ext := filepath.Ext(s)
		if len(ext) > 16 {
			ext = ""
		}
		s = s[:80-len(ext)] + ext
	}
	return s
}

// saveUpload writes one attachment and returns its absolute path. Names are
// prefixed with the time so two photos both called image.jpg stay two files,
// and never overwrite: O_EXCL with a counter on the rare collision.
func saveUpload(dir, name string, body io.Reader) (string, int64, error) {
	stamp := time.Now().Format("20060102-150405")
	base := safeName(name)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 0; i < 100; i++ {
		fn := stamp + "-" + base
		if i > 0 {
			fn = fmt.Sprintf("%s-%s-%d%s", stamp, stem, i, ext)
		}
		path := filepath.Join(dir, fn)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", 0, err
		}
		n, err := io.Copy(f, body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
			return "", 0, err
		}
		return path, n, nil
	}
	return "", 0, errors.New("could not find a free file name")
}

// uploadRoute registers POST /v1/sessions/upload?session=ID&name=NAME, whose
// body is the file's bytes as they are -- no multipart, no base64. It returns
// where the file was saved; the client puts that path in the message it sends,
// which is how every agent, whatever its input, comes to see the file: each
// of them can open a file it is told the path of.
func (d *Daemon) uploadRoute(mux *http.ServeMux) {
	d.handle(mux, "/v1/sessions/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		sess, ok := d.reg.Get(q.Get("session"))
		if !ok {
			http.Error(w, "unknown session", http.StatusBadRequest)
			return
		}
		if r.ContentLength > maxUpload {
			http.Error(w, fmt.Sprintf("file is larger than %d MB", maxUpload>>20), http.StatusRequestEntityTooLarge)
			return
		}
		dir, err := d.uploadsDir(sess.ID, sess.Cwd)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		path, n, err := saveUpload(dir, q.Get("name"), http.MaxBytesReader(w, r.Body, maxUpload))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, fmt.Sprintf("file is larger than %d MB", maxUpload>>20), http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		actor := q.Get("by")
		if actor == "" {
			actor = "cli"
		}
		// The name and size, never the content, as with input.
		_ = d.store.Audit(actor, "file_uploaded", sess.ID, fmt.Sprintf("%s bytes=%d", filepath.Base(path), n))
		writeJSON(w, map[string]any{"path": path, "name": filepath.Base(path), "size": n})
	})
}
