package push

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

func phoneKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpens(t *testing.T) {
	k := phoneKey(t)
	msg := []byte(`{"v":1,"k":"approval"}`)
	sealed, err := Seal(k.PublicKey().Bytes(), msg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(k, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("opened %q, want %q", got, msg)
	}
	// Fresh ephemeral key each time: the same message never seals the same.
	again, _ := Seal(k.PublicKey().Bytes(), msg)
	if again == sealed {
		t.Error("two seals of one message are identical")
	}
}

func TestSealOnlyOpensForItsPhone(t *testing.T) {
	sealed, _ := Seal(phoneKey(t).PublicKey().Bytes(), []byte("secret"))
	if _, err := Open(phoneKey(t), sealed); err == nil {
		t.Fatal("another phone's key opened it")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(sealed)
	raw[len(raw)-1] ^= 1
	k := phoneKey(t)
	if _, err := Open(k, base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("a tampered seal opened")
	}
}

func TestSealRefusesABadKey(t *testing.T) {
	if _, err := Seal([]byte("short"), []byte("x")); err == nil {
		t.Fatal("sealed to a key that isn't one")
	}
}

// A fixed vector, for the phone's implementation to check itself against:
// the app's tests open this with the same private key and expect the same
// plaintext. Changing the format means changing both, and sealVersion.
func TestSealVector(t *testing.T) {
	phonePriv, _ := hex.DecodeString("a8abababababababababababababababababababababababababababababab6b")
	ephPriv, _ := hex.DecodeString("70707070707070707070707070707070707070707070707070707070707070f0")
	phone, err := ecdh.X25519().NewPrivateKey(phonePriv)
	if err != nil {
		t.Fatal(err)
	}
	eph, err := ecdh.X25519().NewPrivateKey(ephPriv)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := seal(eph, phone.PublicKey().Bytes(), []byte("muxscale"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "AaSetJK_PDnuEjw6p8eo2j_VGsmtBYppol7g9y6h79F2bDN-bIDSr83SL3EM_MqqGMsdkwyfUsCw"
	if sealed != want {
		t.Errorf("sealed = %s\nwant      %s", sealed, want)
	}
	got, err := Open(phone, want)
	if err != nil || string(got) != "muxscale" {
		t.Fatalf("open = %q, %v", got, err)
	}
}

func TestContentFitsTheNotary(t *testing.T) {
	k := phoneKey(t)
	huge := strings.Repeat("rm -rf ./build && ", 2000)
	sess := protocol.Session{ID: "s1", AgentID: "claude-code", Name: strings.Repeat("n", 500)}
	a := protocol.ApprovalRequest{ID: "a1", Kind: "tool_use", ToolName: "Bash", Subject: huge,
		ToolInput: map[string]any{"command": huge}}
	c := ApprovalContent(sess, a, "studio")
	c.Host = "host-1"
	b, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(k.PublicKey().Bytes(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) > 3072 {
		t.Fatalf("sealed is %d characters; the notary takes 3072", len(sealed))
	}
	var back Content
	opened, _ := Open(k, sealed)
	if err := json.Unmarshal(opened, &back); err != nil {
		t.Fatal(err)
	}
	if back.ApprovalID != "a1" || back.Session != "s1" || back.Host != "host-1" || back.Kind != ContentApproval {
		t.Errorf("ids lost in shrinking: %+v", back)
	}
}

func TestQuestionContent(t *testing.T) {
	a := protocol.ApprovalRequest{ID: "q1", Kind: "question", PromptID: "p1", Questions: []protocol.Question{{
		Question: "Keep the old URL?", Options: []protocol.QuestionOption{{Label: "Redirect"}, {Label: "Keep both"}},
	}}}
	c := ApprovalContent(protocol.Session{ID: "s", AgentID: "codex"}, a, "laptop")
	if c.Kind != ContentQuestion || c.Body != "Keep the old URL?" || len(c.Options) != 2 || c.PromptID != "p1" || c.Agent != "codex" {
		t.Errorf("%+v", c)
	}
}

func TestIdsAreOpaque(t *testing.T) {
	id := CollapseID("my-laptop", "session-with-a-name")
	if strings.Contains(id, "laptop") || strings.Contains(id, "name") || len(id) > 64 {
		t.Errorf("collapse id %q leaks or is too long", id)
	}
	if CollapseID("h", "a") == CollapseID("h", "b") || !strings.HasPrefix(CollapseID("h", "a"), ThreadID("h")+".") {
		t.Error("collapse ids should differ per session and start with the thread id")
	}
}

func TestNotarySend(t *testing.T) {
	var got struct {
		auth string
		body map[string]string
	}
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/push/send" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		got.auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(status)
		if status >= 400 {
			w.Write([]byte(`{"error":"rate_limited","message":"too many requests"}`))
		}
	}))
	defer srv.Close()
	n := NewNotary(srv.URL + "/")
	p := Push{Capability: "pc_x", Kind: "approval", CollapseID: "a.b", ThreadID: "a", Sealed: "AQ"}
	if err := n.Send(p); err != nil {
		t.Fatal(err)
	}
	if got.auth != "Capability pc_x" || got.body["kind"] != "approval" || got.body["collapse_id"] != "a.b" ||
		got.body["thread_id"] != "a" || got.body["sealed"] != "AQ" {
		t.Errorf("sent %+v", got)
	}
	status = http.StatusGone
	if err := n.Send(p); err != ErrGone {
		t.Errorf("410 gave %v, want ErrGone", err)
	}
	status = http.StatusTooManyRequests
	if err := n.Send(p); err == nil || !strings.Contains(err.Error(), "too many requests") {
		t.Errorf("429 gave %v", err)
	}
}

func TestContentCarriesTheListRow(t *testing.T) {
	sess := protocol.Session{ID: "s", AgentID: "claude-code", Title: "Fix the upload", Cwd: "/work/site/", GitBranch: "fix-upload"}
	c := WaitingContent(sess, "Done.", "studio")
	if c.Name != "" || c.Title != "Fix the upload" || c.Project != "site" || c.Branch != "fix-upload" || c.Agent != "claude-code" {
		t.Errorf("%+v", c)
	}
	if p := projectName(""); p != "" {
		t.Errorf("projectName(\"\") = %q", p)
	}
}
