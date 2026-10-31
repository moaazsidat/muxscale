package push

// What a notification says, sealed to the phone. The phone's notification
// service extension opens it and decides how much shows on the lock screen;
// nothing here is visible to the notary or to Apple, so the daemon doesn't
// hold back.

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// The kinds the notary accepts, which are also the phone's categories.
const (
	ContentApproval = "approval"
	ContentQuestion = "question"
	ContentWaiting  = "waiting"
)

// Content is the sealed notification. Short field names: it shares APNs'
// 4 KB with the rest of the payload, and base64 grows it by a third.
type Content struct {
	V    int    `json:"v"`
	Kind string `json:"k"`
	// Host is the phone's own name for this machine, given when it
	// registered, so an action knows where to go.
	Host    string `json:"h"`
	Machine string `json:"m,omitempty"`
	// The session as the app's list row shows it: the phone builds the same
	// title (name, then title, then project) and line (machine, agent,
	// project, branch) from these, with its own labels.
	Agent   string `json:"a"`
	Session string `json:"s"`
	Name    string `json:"n,omitempty"`
	Title   string `json:"t,omitempty"`
	Project string `json:"pr,omitempty"`
	Branch  string `json:"br,omitempty"`
	// Body: the tool and what it wants to do, the question, or the agent's
	// last words.
	Body       string   `json:"b,omitempty"`
	Tool       string   `json:"tool,omitempty"`
	Detail     string   `json:"d,omitempty"`
	Options    []string `json:"o,omitempty"`
	ApprovalID string   `json:"ap,omitempty"`
	PromptID   string   `json:"p,omitempty"`
}

// plaintextMax keeps the sealed, base64url result under the notary's 3072.
const plaintextMax = 3072*3/4 - 1 - 32 - 16

// ApprovalContent describes an approval or a question an agent is waiting on.
func ApprovalContent(sess protocol.Session, a protocol.ApprovalRequest, machine string) Content {
	c := base(sess, machine)
	c.Kind = ContentApproval
	c.ApprovalID = a.ID
	c.PromptID = a.PromptID
	if a.Kind == "question" && len(a.Questions) > 0 {
		q := a.Questions[0]
		c.Kind = ContentQuestion
		c.Body = q.Question
		for _, o := range q.Options {
			c.Options = append(c.Options, o.Label)
		}
		if len(a.Questions) > 1 {
			c.Detail = "and more"
		}
		return c
	}
	c.Tool = a.ToolName
	c.Body = a.Subject
	c.Detail = formatArgs(a.ToolInput)
	return c
}

// WaitingContent describes a finished turn: the agent wants a reply.
func WaitingContent(sess protocol.Session, last string, machine string) Content {
	c := base(sess, machine)
	c.Kind = ContentWaiting
	c.Body = last
	c.PromptID = sess.PromptID
	return c
}

func base(sess protocol.Session, machine string) Content {
	return Content{
		V: 1, Machine: machine, Agent: sess.AgentID, Session: sess.ID,
		Name: sess.Name, Title: sess.Title, Project: projectName(sess.Cwd), Branch: sess.GitBranch,
	}
}

// Marshal renders the content for sealing, shortening the long fields until
// it fits: the command first, then the body, then the options.
func (c Content) Marshal() ([]byte, error) {
	c.Name = clip(c.Name, 120)
	c.Title = clip(c.Title, 120)
	c.Project = clip(c.Project, 80)
	c.Branch = clip(c.Branch, 80)
	c.Body = clip(c.Body, 600)
	c.Detail = clip(c.Detail, 900)
	for i := range c.Options {
		c.Options[i] = clip(c.Options[i], 80)
	}
	if len(c.Options) > 6 {
		c.Options = c.Options[:6]
	}
	for _, shrink := range []func(){
		func() {},
		func() { c.Detail = clip(c.Detail, 200) },
		func() { c.Body = clip(c.Body, 200) },
		func() { c.Options = nil },
		func() { c.Detail, c.Body = "", clip(c.Body, 80) },
		func() { c.Title, c.Branch = "", "" },
	} {
		shrink()
		b, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		if len(b) <= plaintextMax {
			return b, nil
		}
	}
	// Ids alone are far under the limit; reaching here means a huge id.
	return json.Marshal(Content{V: 1, Kind: c.Kind, Host: clip(c.Host, 64), Session: clip(c.Session, 64)})
}

// projectName is the directory's last part, as the app's list shows it.
func projectName(cwd string) string {
	cwd = strings.TrimRight(cwd, "/")
	return cwd[strings.LastIndexByte(cwd, '/')+1:]
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// ThreadID groups one machine's notifications on the phone; CollapseID makes
// a session's newest notification replace its last. Both are hashes, so the
// notary sees opaque ids, not names.
func ThreadID(host string) string { return opaque(host) }

func CollapseID(host, session string) string { return opaque(host) + "." + opaque(session) }

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func opaque(s string) string {
	sum := sha256.Sum256([]byte(s))
	return strings.ToLower(b32.EncodeToString(sum[:10]))
}
