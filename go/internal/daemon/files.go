package daemon

// Files: a session's project, as the phone browses it.
//
// A session's files are its project -- the git work tree its working directory
// is in, or that directory itself outside git. The client names a session and
// a path relative to that root, never a root of its own, so what can be read
// is decided here, by where the agent was working.
//
// Paths are resolved through os.Root, which refuses ".." and any symlink that
// leads out of the root. A crafted path, or a link an agent or a cloned repo
// planted in the project, cannot reach the rest of the disk.
//
// Read-only for now.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

const (
	// maxListEntries caps one directory listing. A phone scrolling past five
	// thousand names is not browsing; it is a node_modules.
	maxListEntries = 5000
	// maxTextRead is the largest file sent to be shown as text. Past this,
	// a phone's editor is the wrong tool and the shell is the right one.
	maxTextRead = 2 << 20
	// maxRawRead caps a file sent as bytes, for an image.
	maxRawRead = 16 << 20
	// maxChanges caps the changed-files list.
	maxChanges = 500
	// maxToolCalls is how many of a session's tool calls are read for the
	// files it wrote.
	maxToolCalls = 20000
	gitTimeout   = 5 * time.Second
)

var (
	errNoProject = errors.New("this session has no project folder to show")
	errBadPath   = errors.New("path must be relative to the project, inside it")
)

// projectRoot is the folder a session's files are browsed from: the nearest
// enclosing git work tree, or cwd itself.
//
// The search stops below the home directory. A dotfiles repository at ~ would
// otherwise make every session outside a repo of its own a session "in" the
// whole home directory.
func projectRoot(cwd string) (string, error) {
	if cwd == "" {
		return "", errNoProject
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", errNoProject
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", errNoProject
	}
	if abs == filepath.Dir(abs) {
		// The filesystem root is nobody's project.
		return "", errNoProject
	}
	home, _ := os.UserHomeDir()
	for dir := abs; ; {
		if dir == home && dir != abs {
			break
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return abs, nil
}

// cleanRel turns a client's path into one relative to the root, or refuses
// it. os.Root refuses escapes too; this gives the caller a clearer error than
// the one os.Root would.
func cleanRel(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" || p == "." {
		return ".", nil
	}
	if path.IsAbs(p) {
		return "", errBadPath
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", errBadPath
	}
	return c, nil
}

// sessionRoot finds the session a request names and opens its project.
func (d *Daemon) sessionRoot(w http.ResponseWriter, r *http.Request) (*os.Root, string, string, bool) {
	q := r.URL.Query()
	sess, ok := d.reg.Get(q.Get("session"))
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return nil, "", "", false
	}
	dir, err := projectRoot(sess.Cwd)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return nil, "", "", false
	}
	rel, err := cleanRel(q.Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, "", "", false
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		http.Error(w, "the project folder can't be opened", http.StatusNotFound)
		return nil, "", "", false
	}
	return root, dir, rel, true
}

// fileError answers for an error from os.Root.
func fileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "no such file", http.StatusNotFound)
	case errors.Is(err, fs.ErrPermission):
		http.Error(w, "permission denied", http.StatusForbidden)
	default:
		// Escapes through a symlink land here: "path escapes from parent".
		http.Error(w, "that path can't be opened from this project", http.StatusForbidden)
	}
}

func (d *Daemon) filesRoutes(mux *http.ServeMux) {
	// GET /v1/files/list?session=ID&path=REL
	d.handle(mux, "/v1/files/list", func(w http.ResponseWriter, r *http.Request) {
		root, dir, rel, ok := d.sessionRoot(w, r)
		if !ok {
			return
		}
		defer root.Close()
		listing, err := listDir(root, dir, rel)
		if err != nil {
			fileError(w, err)
			return
		}
		writeJSON(w, listing)
	})

	// GET /v1/files/read?session=ID&path=REL
	d.handle(mux, "/v1/files/read", func(w http.ResponseWriter, r *http.Request) {
		root, _, rel, ok := d.sessionRoot(w, r)
		if !ok {
			return
		}
		defer root.Close()
		content, err := readFile(root, rel)
		if err != nil {
			if errors.Is(err, errNotAFile) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fileError(w, err)
			return
		}
		writeJSON(w, content)
	})

	// GET /v1/files/raw?session=ID&path=REL -- the bytes, for an image.
	d.handle(mux, "/v1/files/raw", func(w http.ResponseWriter, r *http.Request) {
		root, _, rel, ok := d.sessionRoot(w, r)
		if !ok {
			return
		}
		defer root.Close()
		serveRaw(w, r, root, rel)
	})

	// GET /v1/files/changes?session=ID
	d.handle(mux, "/v1/files/changes", func(w http.ResponseWriter, r *http.Request) {
		sess, ok := d.reg.Get(r.URL.Query().Get("session"))
		if !ok {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		dir, err := projectRoot(sess.Cwd)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		calls, err := d.store.ToolCalls(sess.ID, maxToolCalls)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, changedFiles(r.Context(), dir, sess.Cwd, calls))
	})
}

func listDir(root *os.Root, dir, rel string) (protocol.FileListing, error) {
	out := protocol.FileListing{Root: dir, Path: rel, Entries: []protocol.FileEntry{}}
	f, err := root.Open(rel)
	if err != nil {
		return out, err
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil {
		return out, err
	}
	if len(des) > maxListEntries {
		sort.Slice(des, func(i, j int) bool { return des[i].Name() < des[j].Name() })
		des, out.Truncated = des[:maxListEntries], true
	}
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue
		}
		e := protocol.FileEntry{Name: de.Name(), Kind: "file", Modified: info.ModTime()}
		switch {
		case de.IsDir():
			e.Kind = "dir"
		case de.Type()&fs.ModeSymlink != 0:
			e.Link = true
			// Followed only inside the root: a link out of it is shown, so the
			// listing is honest, but as something that cannot be opened.
			target, err := root.Stat(path.Join(rel, de.Name()))
			switch {
			case err != nil:
				e.Kind = "link"
			case target.IsDir():
				e.Kind = "dir"
			default:
				e.Size = target.Size()
			}
		case info.Mode().IsRegular():
			e.Size = info.Size()
		default:
			continue // sockets, devices, pipes: nothing to open
		}
		out.Entries = append(out.Entries, e)
	}
	markIgnored(dir, rel, out.Entries)
	sort.SliceStable(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if (a.Kind == "dir") != (b.Kind == "dir") {
			return a.Kind == "dir"
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out, nil
}

// markIgnored asks git which of a listing's entries it ignores, so the phone
// can set build output and dependencies apart. Outside git, or if git is not
// there to ask, nothing is marked.
func markIgnored(dir, rel string, entries []protocol.FileEntry) {
	if len(entries) == 0 || !isGitRoot(dir) {
		return
	}
	var in bytes.Buffer
	byPath := make(map[string]int, len(entries))
	for i, e := range entries {
		if e.Name == ".git" && rel == "." {
			entries[i].Ignored = true
			continue
		}
		p := path.Join(rel, e.Name)
		byPath[p] = i
		in.WriteString(p)
		in.WriteByte(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "check-ignore", "-z", "--stdin")
	cmd.Stdin = &in
	// Exit status 1 means nothing was ignored; the output is what counts.
	out, _ := cmd.Output()
	for _, p := range strings.Split(string(out), "\x00") {
		if i, ok := byPath[strings.TrimSuffix(p, "/")]; ok {
			entries[i].Ignored = true
		}
	}
}

func isGitRoot(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

var errNotAFile = errors.New("not a file")

func readFile(root *os.Root, rel string) (protocol.FileContent, error) {
	f, err := root.Open(rel)
	if err != nil {
		return protocol.FileContent{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return protocol.FileContent{}, err
	}
	if !fi.Mode().IsRegular() {
		return protocol.FileContent{}, errNotAFile
	}
	out := protocol.FileContent{Path: rel, Size: fi.Size(), Modified: fi.ModTime()}

	// Read one byte past the limit: a file that grew since Stat is caught
	// by its content, not by a size that was true a moment ago.
	data, err := io.ReadAll(io.LimitReader(f, maxTextRead+1))
	if err != nil {
		return out, err
	}
	out.MIME = mimeType(rel, data)
	if len(data) > maxTextRead {
		out.TooLarge = true
		out.Binary = looksBinary(data[:8192])
		return out, nil
	}
	sum := sha256.Sum256(data)
	out.SHA256 = hex.EncodeToString(sum[:])
	if looksBinary(data) || !utf8.Valid(data) {
		out.Binary = true
		return out, nil
	}
	out.Text = string(data)
	return out, nil
}

// looksBinary is git's test: a NUL in the first 8 KB.
func looksBinary(b []byte) bool {
	return bytes.IndexByte(b[:min(len(b), 8192)], 0) >= 0
}

func mimeType(name string, head []byte) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return http.DetectContentType(head[:min(len(head), 512)])
}

// rasterImages are the types /v1/files/raw serves as themselves. Everything
// else goes out as an attachment: a project's own .html or .svg, served from
// the daemon's origin as a page, would run its scripts with the phone's
// access to this API.
var rasterImages = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"image/bmp": true, "image/x-icon": true, "image/vnd.microsoft.icon": true,
}

func serveRaw(w http.ResponseWriter, r *http.Request, root *os.Root, rel string) {
	f, err := root.Open(rel)
	if err != nil {
		fileError(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.Error(w, "not a file", http.StatusBadRequest)
		return
	}
	if fi.Size() > maxRawRead {
		http.Error(w, "file too large to send", http.StatusRequestEntityTooLarge)
		return
	}
	var head [512]byte
	n, _ := io.ReadFull(f, head[:])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	// Belt and braces: even a type let through here renders with no script,
	// no plugins and no requests of its own.
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	if t := http.DetectContentType(head[:n]); rasterImages[t] {
		h.Set("Content-Type", t)
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", "attachment")
	}
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// writeTools are the tool names, lowercased, that write a file, across the
// agents muxscale reads.
var writeTools = map[string]bool{
	"write": true, "writefile": true, "write_file": true, "create": true, "create_file": true,
	"edit": true, "multiedit": true, "edit_file": true, "str_replace": true,
	"str_replace_editor": true, "str_replace_based_edit_tool": true, "replace": true,
	"apply_patch": true, "patch": true, "notebookedit": true,
}

// Codex's apply_patch names its files in the patch text.
var patchMarkers = []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "}

// writtenPaths returns the files one tool call wrote (or deleted), as the
// agent named them.
func writtenPaths(e protocol.TranscriptEvent) []string {
	if !writeTools[strings.ToLower(e.ToolName)] {
		return nil
	}
	var out []string
	for _, k := range []string{"file_path", "path", "filename", "file", "notebook_path", "target_file"} {
		if s, ok := e.ToolInput[k].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	for _, v := range e.ToolInput {
		s, ok := v.(string)
		if !ok || !strings.Contains(s, "*** ") {
			continue
		}
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimRight(line, "\r")
			for _, m := range patchMarkers {
				if p, ok := strings.CutPrefix(line, m); ok && strings.TrimSpace(p) != "" {
					out = append(out, strings.TrimSpace(p))
				}
			}
		}
	}
	return out
}

// relToRoot turns a path an agent named into one relative to the project,
// or "" if it is outside it.
func relToRoot(dir, cwd, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	rel, err := filepath.Rel(dir, filepath.Clean(p))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

func changedFiles(ctx context.Context, dir, cwd string, calls []protocol.TranscriptEvent) protocol.FileChanges {
	out := protocol.FileChanges{Root: dir, Git: isGitRoot(dir), Changes: []protocol.FileChange{}}
	byPath := map[string]*protocol.FileChange{}
	get := func(p string) *protocol.FileChange {
		if c, ok := byPath[p]; ok {
			return c
		}
		c := &protocol.FileChange{Path: p}
		byPath[p] = c
		return c
	}

	for _, e := range calls {
		if e.IsError {
			continue
		}
		for _, p := range writtenPaths(e) {
			rel := relToRoot(dir, cwd, p)
			if rel == "" {
				continue
			}
			c := get(rel)
			c.ByAgent = true
			at := e.At
			if c.At == nil || at.After(*c.At) {
				c.At = &at
			}
		}
	}

	if out.Git {
		for p, status := range gitStatus(ctx, dir) {
			get(p).Status = status
		}
	}

	for p, c := range byPath {
		_, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p)))
		c.Exists = err == nil
		out.Changes = append(out.Changes, *c)
	}
	sort.Slice(out.Changes, func(i, j int) bool {
		a, b := out.Changes[i], out.Changes[j]
		if a.ByAgent != b.ByAgent {
			return a.ByAgent
		}
		if a.ByAgent && !a.At.Equal(*b.At) {
			return a.At.After(*b.At)
		}
		return a.Path < b.Path
	})
	if len(out.Changes) > maxChanges {
		out.Changes, out.Truncated = out.Changes[:maxChanges], true
	}
	return out
}

// gitStatus is `git status` for the work tree: path to status. Paths in
// porcelain output are relative to the top of the work tree, which is dir.
func gitStatus(ctx context.Context, dir string) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	raw, err := cmd.Output()
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, rec := range strings.Split(string(raw), "\x00") {
		if len(rec) < 4 {
			continue
		}
		xy, p := rec[:2], rec[3:]
		out[p] = statusWord(xy)
	}
	return out
}

func statusWord(xy string) string {
	switch {
	case xy == "??":
		return "untracked"
	case strings.ContainsRune(xy, 'U') || xy == "AA" || xy == "DD":
		return "conflict"
	case strings.ContainsRune(xy, 'D'):
		return "deleted"
	case xy[0] == 'A':
		return "added"
	default:
		return "modified"
	}
}
