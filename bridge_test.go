package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestBridge(t *testing.T, channel string) *bridge {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	b, err := newBridge(channel)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line is not one JSON object: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestBridgeChannelLayout(t *testing.T) {
	b := newTestBridge(t, "my repo/../x")
	if b.channel != "my-repo-..-x" {
		t.Fatalf("channel %q is not one safe path segment", b.channel)
	}
	if filepath.Dir(b.inbox) != filepath.Dir(b.outbox) || filepath.Base(b.inbox) != "inbox.jsonl" || filepath.Base(b.outbox) != "outbox.jsonl" {
		t.Fatalf("unexpected files: %s %s", b.inbox, b.outbox)
	}
	if want := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "px0", "bridge", b.channel); filepath.Dir(b.inbox) != want {
		t.Fatalf("dir = %s, want %s", filepath.Dir(b.inbox), want)
	}
	for _, p := range []string{b.inbox, b.outbox} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s not created: %v", p, err)
		}
	}
	for _, bad := range []string{"", "  ", "...", "///"} {
		if _, err := bridgeChannelName(bad); err == nil {
			t.Errorf("bridgeChannelName(%q) accepted", bad)
		}
	}
	if got := defaultBridgeChannel("/x/px0", &PRTarget{Owner: "o", Repo: "r", Number: 7}); got != "o-r-pr7" {
		t.Errorf("PR default channel = %q", got)
	}
	if got := defaultBridgeChannel("/x/px0", nil); got != "px0" {
		t.Errorf("workspace default channel = %q", got)
	}
}

func TestBridgeSendAndReload(t *testing.T) {
	b := newTestBridge(t, "send")
	m, err := b.Send(bridgeMsg{Kind: "review", Comments: []bridgeComment{{ID: "c-1", Path: "a.go", Line: 3, Side: "RIGHT", Text: "why is this\nnil?", Snippet: "3: x := nil"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m.ID, "rv-") || m.TS == "" {
		t.Fatalf("Send did not stamp id/ts: %+v", m)
	}
	if c, _ := b.Send(bridgeMsg{Kind: "chat", Text: "hello"}); !strings.HasPrefix(c.ID, "m-") {
		t.Fatalf("chat id = %q", c.ID)
	}
	lines := readJSONLines(t, b.inbox)
	if len(lines) != 2 || lines[0]["id"] != m.ID || lines[1]["kind"] != "chat" {
		t.Fatalf("inbox = %v", lines)
	}
	if _, ok := lines[1]["comments"]; ok {
		t.Fatalf("chat line carries comments: %v", lines[1])
	}

	// A line from an earlier build (kind "comment", anchor at the top level)
	// reloads as a one-comment review, so old threads keep their place.
	appendFile(t, b.inbox, `{"id":"m-old","ts":"2026-01-01T00:00:00Z","kind":"comment","text":"old q","path":"b.go","line":7}`+"\n")
	again, err := newBridge("send")
	if err != nil {
		t.Fatal(err)
	}
	sent, _ := again.snapshot()
	if len(sent) != 3 || sent[0].ID != m.ID {
		t.Fatalf("history not reloaded: %+v", sent)
	}
	if old := sent[2]; old.Kind != "review" || len(old.Comments) != 1 || old.Comments[0].ID != "m-old" || old.Comments[0].Path != "b.go" || old.Comments[0].Line != 7 || old.Comments[0].Text != "old q" {
		t.Fatalf("legacy line = %+v", old)
	}
}

func TestBridgeSendConcurrentLinesStayWhole(t *testing.T) {
	b := newTestBridge(t, "concurrent")
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Send(bridgeMsg{Kind: "chat", Text: fmt.Sprintf("message %d %s", i, strings.Repeat("x", 2000))}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(readJSONLines(t, b.inbox)); n != 50 {
		t.Fatalf("got %d lines, want 50", n)
	}
}

func TestBridgeOutboxTail(t *testing.T) {
	b := newTestBridge(t, "tail")
	b.Send(bridgeMsg{Kind: "review", Comments: []bridgeComment{{ID: "c-a", Path: "a.go", Line: 1, Text: "q1"}}})
	chat, _ := b.Send(bridgeMsg{Kind: "chat", Text: "q2"})

	appendFile(t, b.outbox, `{"id":"r1","ts":"2026-01-01T00:00:00Z","reply_to":"c-a","text":"**answer** one"}`+"\n")
	appendFile(t, b.outbox, "not json\n\n")
	appendFile(t, b.outbox, `{"reply_to":"`+chat.ID+`","text":"answer two"}`+"\n")
	appendFile(t, b.outbox, `{"reply_to":"c-a","text":"half`) // still being written
	b.poll()

	threads, conv := bridgeView(b.snapshot())
	if len(threads) != 1 || len(threads[0].Items) != 2 || threads[0].Items[1].ID != "r1" || threads[0].Items[1].Who != "claude" {
		t.Fatalf("threads = %+v", threads)
	}
	if len(conv) != 2 || conv[1].Text != "answer two" || conv[1].Kind != "reply" || conv[1].ID == "" || conv[1].TS == "" {
		t.Fatalf("conversation = %+v (id and ts should be filled in)", conv)
	}

	appendFile(t, b.outbox, ` done"}`+"\n"+`{"text":"unprompted note"}`+"\n")
	b.poll()
	threads, conv = bridgeView(b.snapshot())
	if it := threads[0].Items; len(it) != 3 || it[2].Text != "half done" {
		t.Fatalf("partial line not completed: %+v", it)
	}
	if len(conv) != 3 || conv[2].Text != "unprompted note" {
		t.Fatalf("a reply naming nothing should land in the conversation: %+v", conv)
	}

	// Truncating the outbox starts it over rather than reading past the end.
	if err := os.WriteFile(b.outbox, []byte(`{"reply_to":"c-a","text":"fresh"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.poll()
	_, replies := b.snapshot()
	if last := replies[len(replies)-1]; last.Text != "fresh" {
		t.Fatalf("after truncate, last reply = %+v", last)
	}
}

func bridgePost(t *testing.T, s *Server, path string, body any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7777"+path, bytes.NewReader(data))
	req.Header.Set("Origin", "http://"+req.Host)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

// recordingProvider is a GitProvider that fails the test on any call that
// would reach the forge.
type recordingProvider struct {
	t     *testing.T
	mu    sync.Mutex
	calls []string
}

func (p *recordingProvider) hit(name string) {
	p.mu.Lock()
	p.calls = append(p.calls, name)
	p.mu.Unlock()
	p.t.Errorf("unexpected forge call: %s", name)
}

func (p *recordingProvider) Name() string                      { return "github" }
func (p *recordingProvider) MatchURL(string) bool              { return false }
func (p *recordingProvider) ParseURL(string) (PRTarget, error) { return PRTarget{}, errors.New("no") }
func (p *recordingProvider) ResolveToken(settings) (string, string) {
	return "", ""
}
func (p *recordingProvider) FetchPR(context.Context, PRTarget, string) (PRMeta, error) {
	p.hit("FetchPR")
	return PRMeta{}, errors.New("no")
}
func (p *recordingProvider) CheckPushAccess(context.Context, PRTarget, string) bool {
	p.hit("CheckPushAccess")
	return false
}
func (p *recordingProvider) SubmitReview(context.Context, PRTarget, string, string, []prComment, string, string) error {
	p.hit("SubmitReview")
	return errors.New("no")
}
func (p *recordingProvider) FetchComments(context.Context, PRTarget, string) ([]PRComment, []PRComment, error) {
	p.hit("FetchComments")
	return nil, nil, errors.New("no")
}
func (p *recordingProvider) PostIssueComment(context.Context, PRTarget, string, string) (PRComment, error) {
	p.hit("PostIssueComment")
	return PRComment{}, errors.New("no")
}
func (p *recordingProvider) ReplyToReviewComment(context.Context, PRTarget, string, int64, string) (PRComment, error) {
	p.hit("ReplyToReviewComment")
	return PRComment{}, errors.New("no")
}

type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("unexpected HTTP request: %s %s", r.Method, r.URL)
	return nil, errors.New("network disabled in test")
}

func draftOp(t *testing.T, s *Server, body map[string]any) map[string]any {
	t.Helper()
	code, m := bridgePost(t, s, "/api/bridge/drafts", body)
	if code != http.StatusOK {
		t.Fatalf("drafts %v = %d %v", body["op"], code, m)
	}
	return m
}

func TestBridgeHTTP(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s, _ := newTestServer(t)
	if code, _ := get(t, s, "/api/bridge"); code != http.StatusNotFound {
		t.Fatalf("without -bridge, /api/bridge = %d, want 404", code)
	}
	if _, meta := get(t, s, "/api/meta"); meta["bridge"] != nil {
		t.Fatalf("meta carries a bridge without -bridge: %v", meta["bridge"])
	}
	b := newTestBridge(t, "http")
	s.SetBridge(b)

	if code, _ := bridgePost(t, s, "/api/bridge/drafts", map[string]any{"op": "add", "text": "x", "path": "../etc/passwd", "line": 1}); code != http.StatusBadRequest {
		t.Fatalf("path outside the workspace = %d, want 400", code)
	}
	if code, _ := bridgePost(t, s, "/api/bridge/chat", map[string]any{"text": "  "}); code != http.StatusBadRequest {
		t.Fatalf("empty chat = %d, want 400", code)
	}
	code, chat := bridgePost(t, s, "/api/bridge/chat", map[string]any{"text": "what next?"})
	if code != http.StatusOK || chat["kind"] != "chat" {
		t.Fatalf("chat = %d %v", code, chat)
	}
	if lines := readJSONLines(t, b.inbox); len(lines) != 1 || lines[0]["text"] != "what next?" || lines[0]["repo"] == "" {
		t.Fatalf("inbox = %v", lines)
	}
	appendFile(t, b.outbox, `{"reply_to":"`+chat["id"].(string)+`","text":"Tests."}`+"\n")
	b.poll()
	_, view := get(t, s, "/api/bridge")
	conv := view["conversation"].([]any)
	if len(conv) != 2 || conv[1].(map[string]any)["who"] != "claude" || conv[1].(map[string]any)["text"] != "Tests." {
		t.Fatalf("conversation = %v", conv)
	}
}

func TestBridgeCommentsNeverCallGitHub(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	prev := githubHTTPClient.Transport
	githubHTTPClient.Transport = failTransport{t}
	t.Cleanup(func() { githubHTTPClient.Transport = prev })

	s, _ := newTestServer(t)
	prov := &recordingProvider{t: t}
	s.pr = &prSession{
		provider: prov,
		target:   PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 9},
		meta:     PRMeta{Number: 9, HeadSHA: "abc123"},
		token:    "a-real-looking-token",
	}
	b := newTestBridge(t, "local")
	s.SetBridge(b)

	c := draftOp(t, s, map[string]any{"op": "add", "path": "main.go", "line": 4, "side": "RIGHT", "text": "private note"})["draft"].(map[string]any)
	for _, step := range []struct {
		path string
		body map[string]any
	}{
		{"/api/bridge/review", map[string]any{"text": "all of it"}},
		{"/api/bridge/reply", map[string]any{"id": c["id"], "text": "and also"}},
		{"/api/bridge/chat", map[string]any{"text": "what next?"}},
		{"/api/bridge/to-github", map[string]any{"id": c["id"]}}, // a GitHub *draft*: still local
		{"/api/pr/comments", map[string]any{"path": "main.go", "line": 4, "side": "RIGHT", "body": "draft"}},
	} {
		if code, m := bridgePost(t, s, step.path, step.body); code != http.StatusOK {
			t.Fatalf("%s = %d %v", step.path, code, m)
		}
	}
	if len(prov.calls) != 0 {
		t.Fatalf("forge calls: %v", prov.calls)
	}
	lines := readJSONLines(t, b.inbox)
	if len(lines) != 3 || lines[0]["repo"] != "o/r" || lines[0]["pr"] != float64(9) || lines[0]["commit"] != "abc123" {
		t.Fatalf("inbox = %v", lines)
	}
}

func TestBridgeReviewSendsOneLine(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s, _ := newTestServer(t)
	b := newTestBridge(t, "review")
	s.SetBridge(b)

	if code, _ := bridgePost(t, s, "/api/bridge/review", map[string]any{"text": "x"}); code != http.StatusBadRequest {
		t.Fatalf("Ask Claude with nothing pending = %d, want 400", code)
	}
	d1 := draftOp(t, s, map[string]any{"op": "add", "path": "greet.go", "line": 5, "endLine": 6, "side": "RIGHT", "text": "first"})["draft"].(map[string]any)
	d2 := draftOp(t, s, map[string]any{"op": "add", "path": "main.go", "line": 4, "text": "second"})["draft"].(map[string]any)
	d3 := draftOp(t, s, map[string]any{"op": "add", "path": "main.go", "line": 1, "text": "dropped"})["draft"].(map[string]any)
	draftOp(t, s, map[string]any{"op": "update", "id": d2["id"], "text": "second, edited"})
	if left := draftOp(t, s, map[string]any{"op": "delete", "id": d3["id"]})["drafts"].([]any); len(left) != 2 {
		t.Fatalf("after delete, %d pending", len(left))
	}
	if code, _ := bridgePost(t, s, "/api/bridge/drafts", map[string]any{"op": "update", "id": "c-nope", "text": "x"}); code != http.StatusNotFound {
		t.Fatalf("update of a missing comment = %d, want 404", code)
	}
	if n := len(readJSONLines(t, b.inbox)); n != 0 {
		t.Fatalf("pending comments reached the inbox before Ask Claude: %d lines", n)
	}

	code, sent := bridgePost(t, s, "/api/bridge/review", map[string]any{"text": "overall: looks close"})
	if code != http.StatusOK {
		t.Fatalf("review = %d %v", code, sent)
	}
	lines := readJSONLines(t, b.inbox)
	if len(lines) != 1 {
		t.Fatalf("review wrote %d lines, want 1", len(lines))
	}
	rv := lines[0]
	if rv["kind"] != "review" || rv["text"] != "overall: looks close" || rv["id"] != sent["id"] || !strings.HasPrefix(rv["id"].(string), "rv-") || rv["ts"] == "" || rv["repo"] == "" {
		t.Fatalf("review line = %v", rv)
	}
	cs := rv["comments"].([]any)
	if len(cs) != 2 {
		t.Fatalf("comments = %v", cs)
	}
	c1, c2 := cs[0].(map[string]any), cs[1].(map[string]any)
	if c1["id"] != d1["id"] || c1["path"] != "greet.go" || c1["line"] != float64(5) || c1["end_line"] != float64(6) || c1["side"] != "RIGHT" || c1["text"] != "first" {
		t.Fatalf("comment 1 = %v", c1)
	}
	if snip, _ := c1["snippet"].(string); !strings.Contains(snip, "6: \tfmt.Println(s)") || !strings.HasPrefix(snip, "2: ") {
		t.Fatalf("comment 1 snippet = %q", snip)
	}
	if _, ok := c1["in_reply_to"]; ok || c2["id"] != d2["id"] || c2["text"] != "second, edited" {
		t.Fatalf("comment 2 = %v (and no in_reply_to on a first comment)", c2)
	}
	if _, view := get(t, s, "/api/bridge"); len(view["pending"].([]any)) != 0 {
		t.Fatalf("pending not cleared after sending: %v", view["pending"])
	}
}

func TestBridgeThreadsAndFollowUps(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s, _ := newTestServer(t)
	b := newTestBridge(t, "rthread")
	s.SetBridge(b)
	c1 := draftOp(t, s, map[string]any{"op": "add", "path": "main.go", "line": 4, "text": "a"})["draft"].(map[string]any)
	c2 := draftOp(t, s, map[string]any{"op": "add", "path": "greet.go", "line": 6, "text": "b"})["draft"].(map[string]any)
	_, rv := bridgePost(t, s, "/api/bridge/review", map[string]any{"text": "both"})

	appendFile(t, b.outbox, `{"reply_to":"`+rv["id"].(string)+`","text":"combined answer"}`+"\n")
	appendFile(t, b.outbox, `{"reply_to":"`+c2["id"].(string)+`","text":"about b"}`+"\n")
	b.poll()

	// A follow-up under b is one review line with one comment that names b.
	if code, _ := bridgePost(t, s, "/api/bridge/reply", map[string]any{"id": "c-unknown", "text": "x"}); code != http.StatusBadRequest {
		t.Fatalf("reply to an unknown comment = %d, want 400", code)
	}
	code, fu := bridgePost(t, s, "/api/bridge/reply", map[string]any{"id": c2["id"], "text": "why not 7?"})
	if code != http.StatusOK {
		t.Fatalf("reply = %d %v", code, fu)
	}
	line := readJSONLines(t, b.inbox)[1]
	fc := line["comments"].([]any)
	if line["kind"] != "review" || line["text"] != "" || len(fc) != 1 {
		t.Fatalf("follow-up line = %v", line)
	}
	f := fc[0].(map[string]any)
	if f["in_reply_to"] != c2["id"] || f["path"] != "greet.go" || f["line"] != float64(6) || f["text"] != "why not 7?" || f["id"] == c2["id"] {
		t.Fatalf("follow-up comment = %v", f)
	}
	appendFile(t, b.outbox, `{"reply_to":"`+f["id"].(string)+`","text":"7 is the closing brace."}`+"\n")
	b.poll()

	_, view := get(t, s, "/api/bridge")
	threads := view["threads"].([]any)
	if len(threads) != 2 {
		t.Fatalf("threads = %v", threads)
	}
	ta, tb := threads[0].(map[string]any), threads[1].(map[string]any)
	if ta["id"] != c1["id"] || len(ta["items"].([]any)) != 1 {
		t.Fatalf("thread a = %v", ta)
	}
	var got []string
	for _, it := range tb["items"].([]any) {
		m := it.(map[string]any)
		got = append(got, m["who"].(string)+":"+m["text"].(string))
	}
	if want := "you:b|claude:about b|you:why not 7?|claude:7 is the closing brace."; strings.Join(got, "|") != want {
		t.Fatalf("thread b = %q, want %q", strings.Join(got, "|"), want)
	}
	conv := view["conversation"].([]any)
	if len(conv) != 2 || conv[0].(map[string]any)["kind"] != "note" || conv[0].(map[string]any)["count"] != float64(2) || conv[1].(map[string]any)["text"] != "combined answer" {
		t.Fatalf("conversation = %v", conv)
	}
}

func TestBridgeToGitHubKeepsThread(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s, _ := newTestServer(t)
	s.pr = &prSession{target: PRTarget{Owner: "o", Repo: "r", Number: 3}, meta: PRMeta{Number: 3}}
	b := newTestBridge(t, "togh")
	s.SetBridge(b)

	// Pending: becomes a GitHub draft and leaves the Claude queue.
	p := draftOp(t, s, map[string]any{"op": "add", "path": "main.go", "line": 2, "text": "for github"})["draft"].(map[string]any)
	if code, gh := bridgePost(t, s, "/api/bridge/to-github", map[string]any{"id": p["id"]}); code != http.StatusOK || gh["body"] != "for github" {
		t.Fatalf("move pending = %d %v", code, gh)
	}
	if _, view := get(t, s, "/api/bridge"); len(view["pending"].([]any)) != 0 {
		t.Fatalf("pending after move = %v", view["pending"])
	}

	// Sent: copied to a GitHub draft, thread and replies stay.
	c := draftOp(t, s, map[string]any{"op": "add", "path": "main.go", "line": 4, "text": "asked"})["draft"].(map[string]any)
	bridgePost(t, s, "/api/bridge/review", map[string]any{})
	appendFile(t, b.outbox, `{"reply_to":"`+c["id"].(string)+`","text":"answer"}`+"\n")
	b.poll()
	if code, _ := bridgePost(t, s, "/api/bridge/to-github", map[string]any{"id": c["id"]}); code != http.StatusOK {
		t.Fatalf("move sent = %d", code)
	}
	if len(s.pr.comments) != 2 || s.pr.comments[1].Body != "asked" || s.pr.comments[1].Line != 4 {
		t.Fatalf("GitHub drafts = %+v", s.pr.comments)
	}
	_, view := get(t, s, "/api/bridge")
	th := view["threads"].([]any)[0].(map[string]any)
	if th["github"] != true || len(th["items"].([]any)) != 2 {
		t.Fatalf("thread after move = %v", th)
	}
	// Deleting the GitHub draft clears the mark; the Claude thread stays.
	bridgePost(t, s, fmt.Sprintf("/api/pr/comments/delete?id=%d", s.pr.comments[1].ID), map[string]any{})
	_, view = get(t, s, "/api/bridge")
	if th := view["threads"].([]any)[0].(map[string]any); th["github"] != nil || len(th["items"].([]any)) != 2 {
		t.Fatalf("thread after GitHub draft deleted = %v", th)
	}
}

func TestBridgeDraftsPersist(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s, root := newTestServer(t)
	s.SetBridge(newTestBridge(t, "persist"))
	draftOp(t, s, map[string]any{"op": "add", "path": "main.go", "line": 4, "text": "keep me"})

	// A restarted process on the same workspace reads them back.
	ix := NewIndex(root)
	ix.Build()
	again := NewServer(ix, nil)
	again.SetBridge(s.bridge)
	if _, view := get(t, again, "/api/bridge"); len(view["pending"].([]any)) != 1 {
		t.Fatalf("pending after restart = %v", view["pending"])
	}

	// A PR review checks out into a new temp dir each run; its session is keyed
	// by the PR, so pending comments come back there too.
	target := PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 5}
	pr1, _ := newTestServer(t)
	pr1.SetBridge(s.bridge)
	pr1.SetPR(&prSession{target: target, meta: PRMeta{Number: 5}, diffBase: "HEAD"})
	draftOp(t, pr1, map[string]any{"op": "add", "path": "greet.go", "line": 6, "text": "pr note"})

	pr2, _ := newTestServer(t) // different root, same PR
	pr2.SetBridge(s.bridge)
	pr2.SetPR(&prSession{target: target, meta: PRMeta{Number: 5}, diffBase: "HEAD"})
	_, view := get(t, pr2, "/api/bridge")
	if d := view["pending"].([]any); len(d) != 1 || d[0].(map[string]any)["text"] != "pr note" {
		t.Fatalf("PR pending after restart = %v", view["pending"])
	}
}

func TestBridgeInstruction(t *testing.T) {
	b := newTestBridge(t, "instr")
	got := b.Instruction()
	for _, want := range []string{"tail -n0 -F " + b.inbox, b.outbox, `kind "review"`, "in_reply_to", `"reply_to":"<comment id>"`, `kind "chat"`, "Do not post anything to GitHub"} {
		if !strings.Contains(got, want) {
			t.Errorf("instruction lacks %q:\n%s", want, got)
		}
	}
}
