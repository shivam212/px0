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

func TestBridgeSendAppendsOneLinePerMessage(t *testing.T) {
	b := newTestBridge(t, "send")
	m, err := b.Send(bridgeMsg{Kind: "comment", Text: "why is this\nnil?", Path: "a.go", Line: 3, Side: "RIGHT", Snippet: "3: x := nil"})
	if err != nil {
		t.Fatal(err)
	}
	if m.ID == "" || m.TS == "" {
		t.Fatalf("Send did not stamp id/ts: %+v", m)
	}
	if _, err := b.Send(bridgeMsg{Kind: "chat", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	lines := readJSONLines(t, b.inbox)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	first := lines[0]
	if first["id"] != m.ID || first["kind"] != "comment" || first["text"] != "why is this\nnil?" || first["path"] != "a.go" || first["line"] != float64(3) || first["side"] != "RIGHT" || first["snippet"] != "3: x := nil" {
		t.Fatalf("first line = %v", first)
	}
	if _, ok := lines[1]["path"]; ok {
		t.Fatalf("chat message carries an empty path: %v", lines[1])
	}

	// A new process on the same channel picks the history back up.
	again, err := newBridge("send")
	if err != nil {
		t.Fatal(err)
	}
	if sent, _ := again.snapshot(); len(sent) != 2 || sent[0].ID != m.ID {
		t.Fatalf("history not reloaded: %+v", sent)
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
	a, _ := b.Send(bridgeMsg{Kind: "comment", Text: "q1", Path: "a.go", Line: 1})
	c, _ := b.Send(bridgeMsg{Kind: "chat", Text: "q2"})

	appendFile(t, b.outbox, `{"id":"r1","ts":"2026-01-01T00:00:00Z","reply_to":"`+a.ID+`","text":"**answer** one"}`+"\n")
	appendFile(t, b.outbox, "not json\n\n")
	appendFile(t, b.outbox, `{"reply_to":"`+c.ID+`","text":"answer two"}`+"\n")
	appendFile(t, b.outbox, `{"reply_to":"`+a.ID+`","text":"half`) // still being written
	b.poll()

	threads, loose := threadBridgeReplies(b.snapshot())
	if len(threads) != 2 || len(loose) != 0 {
		t.Fatalf("threads=%d loose=%d", len(threads), len(loose))
	}
	if r := threads[0].Replies; len(r) != 1 || r[0].ID != "r1" || r[0].Text != "**answer** one" {
		t.Fatalf("comment replies = %+v", r)
	}
	if r := threads[1].Replies; len(r) != 1 || r[0].Text != "answer two" || r[0].ID == "" || r[0].TS == "" {
		t.Fatalf("chat replies = %+v (id and ts should be filled in)", r)
	}

	appendFile(t, b.outbox, ` done"}`+"\n"+`{"text":"unprompted note"}`+"\n"+`{"reply_to":"m-gone","text":"orphan"}`+"\n")
	b.poll()
	threads, loose = threadBridgeReplies(b.snapshot())
	if r := threads[0].Replies; len(r) != 2 || r[1].Text != "half done" {
		t.Fatalf("partial line not completed: %+v", r)
	}
	if len(loose) != 2 || loose[0].Text != "unprompted note" || loose[1].Text != "orphan" {
		t.Fatalf("loose = %+v", loose)
	}

	// Truncating the outbox starts it over rather than reading past the end.
	if err := os.WriteFile(b.outbox, []byte(`{"reply_to":"`+c.ID+`","text":"fresh"}`+"\n"), 0o600); err != nil {
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

func TestBridgeHTTP(t *testing.T) {
	s, _ := newTestServer(t)
	if code, _ := get(t, s, "/api/bridge"); code != http.StatusNotFound {
		t.Fatalf("without -bridge, /api/bridge = %d, want 404", code)
	}
	if _, meta := get(t, s, "/api/meta"); meta["bridge"] != nil {
		t.Fatalf("meta carries a bridge without -bridge: %v", meta["bridge"])
	}

	b := newTestBridge(t, "http")
	s.SetBridge(b)

	code, sent := bridgePost(t, s, "/api/bridge/send", map[string]any{"text": "what does greet print?", "path": "greet.go", "line": 6})
	if code != http.StatusOK {
		t.Fatalf("send = %d %v", code, sent)
	}
	lines := readJSONLines(t, b.inbox)
	if len(lines) != 1 {
		t.Fatalf("inbox has %d lines", len(lines))
	}
	got := lines[0]
	if got["kind"] != "comment" || got["path"] != "greet.go" || got["line"] != float64(6) || got["repo"] == "" {
		t.Fatalf("inbox line = %v", got)
	}
	if snip, _ := got["snippet"].(string); !strings.Contains(snip, "6: \tfmt.Println(s)") || !strings.HasPrefix(snip, "3: ") {
		t.Fatalf("snippet = %q", snip)
	}

	if code, _ := bridgePost(t, s, "/api/bridge/send", map[string]any{"text": "x", "path": "../etc/passwd", "line": 1}); code != http.StatusBadRequest {
		t.Fatalf("path outside the workspace = %d, want 400", code)
	}
	if code, _ := bridgePost(t, s, "/api/bridge/send", map[string]any{"text": "  "}); code != http.StatusBadRequest {
		t.Fatalf("empty text = %d, want 400", code)
	}

	appendFile(t, b.outbox, `{"reply_to":"`+sent["id"].(string)+`","text":"It prints s."}`+"\n")
	b.poll()
	code, view := get(t, s, "/api/bridge")
	if code != http.StatusOK {
		t.Fatalf("/api/bridge = %d", code)
	}
	msgs, _ := view["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", view["messages"])
	}
	replies, _ := msgs[0].(map[string]any)["replies"].([]any)
	if len(replies) != 1 || replies[0].(map[string]any)["text"] != "It prints s." {
		t.Fatalf("replies = %v", replies)
	}
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

	if code, m := bridgePost(t, s, "/api/bridge/send", map[string]any{"kind": "comment", "text": "private note", "path": "main.go", "line": 4, "side": "RIGHT"}); code != http.StatusOK {
		t.Fatalf("comment = %d %v", code, m)
	}
	if code, m := bridgePost(t, s, "/api/bridge/send", map[string]any{"kind": "chat", "text": "what next?"}); code != http.StatusOK {
		t.Fatalf("chat = %d %v", code, m)
	}
	// The GitHub draft is local too until the review is explicitly submitted.
	if code, m := bridgePost(t, s, "/api/pr/comments", map[string]any{"path": "main.go", "line": 4, "side": "RIGHT", "body": "draft"}); code != http.StatusOK {
		t.Fatalf("draft = %d %v", code, m)
	}
	if code, m := bridgePost(t, s, "/api/bridge/drafts", map[string]any{"op": "add", "path": "main.go", "line": 4, "text": "batched"}); code != http.StatusOK {
		t.Fatalf("bridge draft = %d %v", code, m)
	}
	if code, m := bridgePost(t, s, "/api/bridge/review", map[string]any{"text": "all of it"}); code != http.StatusOK {
		t.Fatalf("bridge review = %d %v", code, m)
	}
	if len(prov.calls) != 0 {
		t.Fatalf("forge calls: %v", prov.calls)
	}

	lines := readJSONLines(t, b.inbox)
	if len(lines) != 3 || lines[2]["kind"] != "review" || lines[0]["repo"] != "o/r" || lines[0]["pr"] != float64(9) || lines[0]["commit"] != "abc123" {
		t.Fatalf("inbox = %v", lines)
	}
}

func draftOp(t *testing.T, s *Server, body map[string]any) map[string]any {
	t.Helper()
	code, m := bridgePost(t, s, "/api/bridge/drafts", body)
	if code != http.StatusOK {
		t.Fatalf("drafts %v = %d %v", body["op"], code, m)
	}
	return m
}

func TestBridgeReviewSendsOneLine(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s, _ := newTestServer(t)
	b := newTestBridge(t, "review")
	s.SetBridge(b)

	if code, _ := bridgePost(t, s, "/api/bridge/review", map[string]any{"text": "x"}); code != http.StatusBadRequest {
		t.Fatalf("review with no drafts = %d, want 400", code)
	}
	d1 := draftOp(t, s, map[string]any{"op": "add", "path": "greet.go", "line": 5, "endLine": 6, "side": "RIGHT", "text": "first"})["draft"].(map[string]any)
	d2 := draftOp(t, s, map[string]any{"op": "add", "path": "main.go", "line": 4, "text": "second"})["draft"].(map[string]any)
	d3 := draftOp(t, s, map[string]any{"op": "add", "path": "main.go", "line": 1, "text": "dropped"})["draft"].(map[string]any)
	draftOp(t, s, map[string]any{"op": "update", "id": d2["id"], "text": "second, edited"})
	if left := draftOp(t, s, map[string]any{"op": "delete", "id": d3["id"]})["drafts"].([]any); len(left) != 2 {
		t.Fatalf("after delete, %d drafts", len(left))
	}
	if code, _ := bridgePost(t, s, "/api/bridge/drafts", map[string]any{"op": "update", "id": "c-nope", "text": "x"}); code != http.StatusNotFound {
		t.Fatalf("update of a missing draft = %d, want 404", code)
	}
	if n := len(readJSONLines(t, b.inbox)); n != 0 {
		t.Fatalf("drafts reached the inbox before sending: %d lines", n)
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
	if _, ok := rv["path"]; ok {
		t.Fatalf("review line carries a top-level path: %v", rv)
	}
	cs := rv["comments"].([]any)
	if len(cs) != 2 {
		t.Fatalf("comments = %v", cs)
	}
	c1, c2 := cs[0].(map[string]any), cs[1].(map[string]any)
	if c1["id"] != d1["id"] || c1["path"] != "greet.go" || c1["line"] != float64(5) || c1["end_line"] != float64(6) || c1["side"] != "RIGHT" || c1["text"] != "first" {
		t.Fatalf("comment 1 = %v", c1)
	}
	if snip, _ := c1["snippet"].(string); !strings.Contains(snip, "6: \tfmt.Println(s)") {
		t.Fatalf("comment 1 snippet = %q", snip)
	}
	if c2["id"] != d2["id"] || c2["text"] != "second, edited" {
		t.Fatalf("comment 2 = %v", c2)
	}
	if _, view := get(t, s, "/api/bridge"); len(view["drafts"].([]any)) != 0 {
		t.Fatalf("drafts not cleared after sending: %v", view["drafts"])
	}
}

func TestBridgeReviewReplyThreading(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s, _ := newTestServer(t)
	b := newTestBridge(t, "rthread")
	s.SetBridge(b)
	draftOp(t, s, map[string]any{"op": "add", "path": "main.go", "line": 4, "text": "a"})
	c2 := draftOp(t, s, map[string]any{"op": "add", "path": "greet.go", "line": 6, "text": "b"})["draft"].(map[string]any)
	_, rv := bridgePost(t, s, "/api/bridge/review", map[string]any{})

	appendFile(t, b.outbox, `{"reply_to":"`+rv["id"].(string)+`","text":"combined answer"}`+"\n")
	appendFile(t, b.outbox, `{"reply_to":"`+c2["id"].(string)+`","text":"about b"}`+"\n")
	b.poll()

	_, view := get(t, s, "/api/bridge")
	msgs := view["messages"].([]any)
	if len(msgs) != 1 || len(view["loose"].([]any)) != 0 {
		t.Fatalf("messages=%v loose=%v", msgs, view["loose"])
	}
	m := msgs[0].(map[string]any)
	if r := m["replies"].([]any); len(r) != 1 || r[0].(map[string]any)["text"] != "combined answer" {
		t.Fatalf("review-level replies = %v", r)
	}
	per := m["comment_replies"].(map[string]any)
	if r, _ := per[c2["id"].(string)].([]any); len(r) != 1 || r[0].(map[string]any)["text"] != "about b" || len(per) != 1 {
		t.Fatalf("comment-level replies = %v", per)
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
	if _, view := get(t, again, "/api/bridge"); len(view["drafts"].([]any)) != 1 {
		t.Fatalf("drafts after restart = %v", view["drafts"])
	}

	// A PR review checks out into a new temp dir each run; its session is keyed
	// by the PR, so drafts come back there too.
	target := PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 5}
	pr1, _ := newTestServer(t)
	pr1.SetBridge(s.bridge)
	pr1.SetPR(&prSession{target: target, meta: PRMeta{Number: 5}, diffBase: "HEAD"})
	draftOp(t, pr1, map[string]any{"op": "add", "path": "greet.go", "line": 6, "text": "pr note"})

	pr2, _ := newTestServer(t) // different root, same PR
	pr2.SetBridge(s.bridge)
	pr2.SetPR(&prSession{target: target, meta: PRMeta{Number: 5}, diffBase: "HEAD"})
	_, view := get(t, pr2, "/api/bridge")
	if d := view["drafts"].([]any); len(d) != 1 || d[0].(map[string]any)["text"] != "pr note" {
		t.Fatalf("PR drafts after restart = %v", view["drafts"])
	}
}
