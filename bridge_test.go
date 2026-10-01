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
	if len(prov.calls) != 0 {
		t.Fatalf("forge calls: %v", prov.calls)
	}

	lines := readJSONLines(t, b.inbox)
	if len(lines) != 2 || lines[0]["repo"] != "o/r" || lines[0]["pr"] != float64(9) || lines[0]["commit"] != "abc123" {
		t.Fatalf("inbox = %v", lines)
	}
}
