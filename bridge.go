package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// bridge.go connects px0 to a coding session that is already running, such
// as a long-lived Claude Code terminal, through two JSON Lines files in
// <config dir>/bridge/<channel>/ (~/.px0/bridge/<channel>/ by default):
//
//   - inbox.jsonl: px0 appends one bridgeMsg per comment or chat message. The
//     session watches it (Claude Code's Monitor tool on `tail -n0 -F inbox`).
//   - outbox.jsonl: the session appends one bridgeReply per answer, and px0
//     tails it and serves the replies to the UI.
//
// Nothing here talks to a forge: a message sent over the bridge stays on this
// machine. The files are px0 state like settings.json, never inside a workspace.

type bridgeMsg struct {
	ID      string `json:"id"`
	TS      string `json:"ts"`
	Kind    string `json:"kind"` // "comment" (anchored to code), "chat", or "review" (a batch of comments)
	Text    string `json:"text"`
	Repo    string `json:"repo,omitempty"`
	PR      int    `json:"pr,omitempty"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	EndLine int    `json:"end_line,omitempty"`
	Side    string `json:"side,omitempty"` // PR diffs: "RIGHT" (new code) or "LEFT" (base)
	Commit  string `json:"commit,omitempty"`
	Snippet string `json:"snippet,omitempty"`

	Comments []bridgeComment `json:"comments,omitempty"` // kind "review" only
}

// bridgeComment is one code comment: a draft waiting in the session file, or
// one entry of a "review" message once the drafts are sent together. Its id
// stays the same from draft to sent, so Claude can reply to it directly.
type bridgeComment struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line,omitempty"`
	Side    string `json:"side,omitempty"`
	Text    string `json:"text"`
	Snippet string `json:"snippet,omitempty"`
}

type bridgeReply struct {
	ID      string `json:"id"`
	TS      string `json:"ts"`
	ReplyTo string `json:"reply_to,omitempty"`
	Text    string `json:"text"`
}

// bridgeHistoryCap bounds what is kept in memory from a long-lived channel.
const bridgeHistoryCap = 1000

type bridge struct {
	channel string
	inbox   string
	outbox  string

	mu      sync.Mutex
	sent    []bridgeMsg
	replies []bridgeReply
	off     int64  // bytes of outbox already consumed
	partial []byte // an outbox line still being written

	stop chan struct{}
	done chan struct{}
}

var bridgeChannelRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// bridgeChannelName turns a user-supplied or derived name into a safe single
// path segment.
func bridgeChannelName(name string) (string, error) {
	c := strings.Trim(bridgeChannelRe.ReplaceAllString(strings.TrimSpace(name), "-"), "-.")
	if c == "" {
		return "", fmt.Errorf("invalid bridge channel %q", name)
	}
	return c, nil
}

// defaultBridgeChannel is what `-bridge auto` resolves to: owner-repo-pr<N> in a
// PR review, else the workspace directory's name.
func defaultBridgeChannel(root string, target *PRTarget) string {
	if target != nil {
		return fmt.Sprintf("%s-%s-pr%d", target.Owner, target.Repo, target.Number)
	}
	return filepath.Base(root)
}

// bridgeDir mirrors settingsPath: $XDG_CONFIG_HOME/px0/bridge, else ~/.px0/bridge.
func bridgeDir() (string, error) {
	p := settingsPath()
	if p == "" {
		return "", errors.New("no home directory for the bridge files")
	}
	return filepath.Join(filepath.Dir(p), "bridge"), nil
}

// newBridge creates the channel directory and both files (so `tail -F` and
// `>>` work from the first moment), loads the messages already in them, and
// returns a bridge whose tailer is not yet running.
func newBridge(channel string) (*bridge, error) {
	name, err := bridgeChannelName(channel)
	if err != nil {
		return nil, err
	}
	base, err := bridgeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	b := &bridge{
		channel: name,
		inbox:   filepath.Join(dir, "inbox.jsonl"),
		outbox:  filepath.Join(dir, "outbox.jsonl"),
	}
	for _, p := range []string{b.inbox, b.outbox} {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	b.loadInbox()
	b.poll()
	return b, nil
}

func (b *bridge) loadInbox() {
	f, err := os.Open(b.inbox)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var m bridgeMsg
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.ID != "" {
			b.sent = append(b.sent, m)
		}
	}
	b.sent = capTail(b.sent)
}

func capTail[T any](s []T) []T {
	if len(s) > bridgeHistoryCap {
		return append([]T(nil), s[len(s)-bridgeHistoryCap:]...)
	}
	return s
}

func bridgeID(prefix string) string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(buf[:])
}

// Send stamps m with an id and time and appends it to the inbox as one line in
// one write on an O_APPEND descriptor, so a reader never sees half a message.
func (b *bridge) Send(m bridgeMsg) (bridgeMsg, error) {
	prefix := "m-"
	if m.Kind == "review" {
		prefix = "rv-"
	}
	m.ID = bridgeID(prefix)
	m.TS = time.Now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(m)
	if err != nil {
		return m, err
	}
	line = append(line, '\n')

	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := os.OpenFile(b.inbox, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return m, err
	}
	_, err = f.Write(line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return m, err
	}
	b.sent = capTail(append(b.sent, m))
	return m, nil
}

// poll reads whatever the outbox gained since the last call. A shrunk file
// (truncated or replaced) is read again from the start. A trailing line with
// no newline yet is held back until it is finished. Lines that are not a
// reply object are skipped; id and ts are filled in when the writer left them out.
func (b *bridge) poll() {
	f, err := os.Open(b.outbox)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if st.Size() < b.off {
		b.off, b.partial = 0, nil
	}
	if st.Size() == b.off {
		return
	}
	if _, err := f.Seek(b.off, io.SeekStart); err != nil {
		return
	}
	chunk, err := io.ReadAll(io.LimitReader(f, st.Size()-b.off))
	if err != nil {
		return
	}
	start := b.off - int64(len(b.partial))
	b.off += int64(len(chunk))
	data := append(b.partial, chunk...)
	b.partial = nil
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			b.partial = append([]byte(nil), data...)
			break
		}
		line := bytes.TrimSpace(data[:i])
		lineStart := start
		start += int64(i + 1)
		data = data[i+1:]
		var r bridgeReply
		if len(line) == 0 || json.Unmarshal(line, &r) != nil || strings.TrimSpace(r.Text) == "" {
			continue
		}
		if r.ID == "" {
			r.ID = fmt.Sprintf("r-%d", lineStart)
		}
		if r.TS == "" {
			r.TS = time.Now().UTC().Format(time.RFC3339)
		}
		b.replies = append(b.replies, r)
	}
	b.replies = capTail(b.replies)
}

// Start tails the outbox until Close.
func (b *bridge) Start() {
	b.stop, b.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(b.done)
		t := time.NewTicker(400 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-b.stop:
				return
			case <-t.C:
				b.poll()
			}
		}
	}()
}

// Close stops the tailer. Safe on a nil receiver or one never started.
func (b *bridge) Close() {
	if b == nil || b.stop == nil {
		return
	}
	close(b.stop)
	<-b.done
}

func (b *bridge) snapshot() ([]bridgeMsg, []bridgeReply) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bridgeMsg{}, b.sent...), append([]bridgeReply{}, b.replies...)
}

// Instruction is the text to paste into the running session so it starts
// listening on this channel.
func (b *bridge) Instruction() string {
	return fmt.Sprintf("Use the Monitor tool to watch `tail -n0 -F %s` (keep it running). "+
		"Each line is a JSON message I wrote in px0: kind \"comment\" is anchored to path/line/side at commit, with a code snippet; kind \"chat\" is free-form. "+
		"Answer each one by appending exactly one JSON line {\"reply_to\":\"<the message id>\",\"text\":\"<markdown>\"} to %s "+
		"(for example: jq -nc --arg r '<id>' --arg t '<reply>' '{reply_to:$r,text:$t}' >> %s). "+
		"Do not post anything to GitHub unless I ask.", b.inbox, b.outbox, b.outbox)
}

// ---------------------------------------------------------------- HTTP

func (s *Server) SetBridge(b *bridge) { s.bridge = b }

func (s *Server) bridgeOrFail(w http.ResponseWriter) bool {
	if s.bridge == nil {
		fail(w, http.StatusNotFound, "bridge not enabled; start px0 with -bridge <name>")
		return false
	}
	return true
}

// handleBridge returns everything sent on the channel, each message carrying
// the outbox replies whose reply_to names it.
func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	if !s.bridgeOrFail(w) {
		return
	}
	threads, loose := threadBridgeReplies(s.bridge.snapshot())
	writeJSON(w, map[string]any{
		"channel":  s.bridge.channel,
		"inbox":    s.bridge.inbox,
		"outbox":   s.bridge.outbox,
		"messages": threads,
		"loose":    loose,
		"drafts":   s.bridgeDrafts(),
	})
}

type bridgeThread struct {
	bridgeMsg
	Replies        []bridgeReply            `json:"replies"`         // reply_to = this message's id
	CommentReplies map[string][]bridgeReply `json:"comment_replies"` // review only: reply_to = one comment's id
}

// threadBridgeReplies puts each reply under what its reply_to names: a whole
// message (comment, chat, or review), or one comment inside a review.
// Replies with no reply_to, or one naming nothing on this channel, come back
// separately as loose, in arrival order.
func threadBridgeReplies(sent []bridgeMsg, replies []bridgeReply) ([]bridgeThread, []bridgeReply) {
	type ref struct {
		msg     int
		comment string // "" for the message itself
	}
	threads := make([]bridgeThread, len(sent))
	byID := make(map[string]ref, len(sent))
	for i, m := range sent {
		threads[i] = bridgeThread{bridgeMsg: m, Replies: []bridgeReply{}, CommentReplies: map[string][]bridgeReply{}}
		byID[m.ID] = ref{msg: i}
		for _, c := range m.Comments {
			byID[c.ID] = ref{msg: i, comment: c.ID}
		}
	}
	loose := []bridgeReply{}
	for _, r := range replies {
		at, ok := byID[r.ReplyTo]
		switch {
		case !ok:
			loose = append(loose, r)
		case at.comment == "":
			threads[at.msg].Replies = append(threads[at.msg].Replies, r)
		default:
			t := &threads[at.msg]
			t.CommentReplies[at.comment] = append(t.CommentReplies[at.comment], r)
		}
	}
	return threads, loose
}

// handleBridgeSend appends one comment or chat message to the inbox. The
// server fills in where it is from (repo, PR, commit) and the code around it.
func (s *Server) handleBridgeSend(w http.ResponseWriter, r *http.Request) {
	if !s.bridgeOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	var body struct {
		Kind    string `json:"kind"`
		Text    string `json:"text"`
		Path    string `json:"path"`
		Line    int    `json:"line"`
		EndLine int    `json:"endLine"`
		Side    string `json:"side"`
		Snippet string `json:"snippet"` // what the client saw, used when the server can't read it (base side of a diff)
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<17)).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		fail(w, http.StatusBadRequest, "text is required")
		return
	}
	m := bridgeMsg{Kind: body.Kind, Text: strings.TrimSpace(body.Text)}
	if m.Kind == "" {
		m.Kind = "chat"
		if body.Path != "" {
			m.Kind = "comment"
		}
	}
	switch m.Kind {
	case "chat":
	case "comment":
		c, ok := s.bridgeAnchor(body.Path, body.Line, body.EndLine, body.Side, body.Snippet)
		if !ok {
			fail(w, http.StatusBadRequest, "a comment needs a path and line")
			return
		}
		m.Path, m.Line, m.EndLine, m.Side, m.Snippet = c.Path, c.Line, c.EndLine, c.Side, c.Snippet
	default:
		fail(w, http.StatusBadRequest, `kind must be "comment" or "chat"`)
		return
	}
	s.bridgeOrigin(&m)
	sent, err := s.bridge.Send(m)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, sent)
}

// bridgeAnchor validates where a comment points and reads the code around it.
func (s *Server) bridgeAnchor(path string, line, endLine int, side, clientSnippet string) (bridgeComment, bool) {
	_, rel, ok := s.safePath(path)
	if !ok || rel == "" || line <= 0 {
		return bridgeComment{}, false
	}
	c := bridgeComment{Path: filepath.ToSlash(rel), Line: line}
	if endLine > line {
		c.EndLine = endLine
	}
	if sd := strings.ToUpper(side); sd == "LEFT" || sd == "RIGHT" {
		c.Side = sd
	}
	c.Snippet = s.bridgeSnippet(c, clientSnippet)
	return c, true
}

func (s *Server) bridgeDrafts() []bridgeComment {
	d := s.session.Get().BridgeDrafts
	if d == nil {
		return []bridgeComment{}
	}
	return d
}

// handleBridgeDrafts edits the draft list kept in the session file. The body's
// op picks what happens: "add" (path, line, endLine, side, text, snippet),
// "update" (id, text) or "delete" (id). It returns the new list.
func (s *Server) handleBridgeDrafts(w http.ResponseWriter, r *http.Request) {
	if !s.bridgeOrFail(w) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"drafts": s.bridgeDrafts()})
		return
	}
	if !localPost(w, r) {
		return
	}
	var body struct {
		Op      string `json:"op"`
		ID      string `json:"id"`
		Path    string `json:"path"`
		Line    int    `json:"line"`
		EndLine int    `json:"endLine"`
		Side    string `json:"side"`
		Text    string `json:"text"`
		Snippet string `json:"snippet"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<17)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	var c bridgeComment
	switch body.Op {
	case "add":
		var ok bool
		if c, ok = s.bridgeAnchor(body.Path, body.Line, body.EndLine, body.Side, body.Snippet); !ok || text == "" {
			fail(w, http.StatusBadRequest, "a draft needs a path, line and text")
			return
		}
		c.ID, c.Text = bridgeID("c-"), text
	case "update":
		if text == "" {
			fail(w, http.StatusBadRequest, "text is required")
			return
		}
	case "delete":
	default:
		fail(w, http.StatusBadRequest, `op must be "add", "update" or "delete"`)
		return
	}
	found := body.Op == "add"
	ws := s.session.Update(func(ws *WorkspaceSession) {
		switch body.Op {
		case "add":
			ws.BridgeDrafts = append(ws.BridgeDrafts, c)
		case "update":
			for i := range ws.BridgeDrafts {
				if ws.BridgeDrafts[i].ID == body.ID {
					ws.BridgeDrafts[i].Text, found = text, true
				}
			}
		case "delete":
			for i, d := range ws.BridgeDrafts {
				if d.ID == body.ID {
					ws.BridgeDrafts, found = append(ws.BridgeDrafts[:i:i], ws.BridgeDrafts[i+1:]...), true
					break
				}
			}
		}
	})
	if !found {
		fail(w, http.StatusNotFound, "no such draft")
		return
	}
	drafts := ws.BridgeDrafts
	if drafts == nil {
		drafts = []bridgeComment{}
	}
	writeJSON(w, map[string]any{"drafts": drafts, "draft": c})
}

// handleBridgeReview sends every draft, plus an optional overall note, as one
// "review" line, so the session is woken once for the whole batch rather
// than once per comment. The drafts are cleared only after the line is written.
func (s *Server) handleBridgeReview(w http.ResponseWriter, r *http.Request) {
	if !s.bridgeOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<17)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	drafts := s.bridgeDrafts()
	if len(drafts) == 0 {
		fail(w, http.StatusBadRequest, "no drafts to send")
		return
	}
	m := bridgeMsg{Kind: "review", Text: strings.TrimSpace(body.Text), Comments: drafts}
	s.bridgeOrigin(&m)
	sent, err := s.bridge.Send(m)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	sentIDs := make(map[string]bool, len(drafts))
	for _, d := range drafts {
		sentIDs[d.ID] = true
	}
	s.session.Update(func(ws *WorkspaceSession) {
		kept := ws.BridgeDrafts[:0:0]
		for _, d := range ws.BridgeDrafts {
			if !sentIDs[d.ID] { // added while this request ran
				kept = append(kept, d)
			}
		}
		ws.BridgeDrafts = kept
	})
	writeJSON(w, sent)
}

// bridgeOrigin records which repo, PR and commit a message is about. It reads
// only what this session already holds; it never calls the forge.
func (s *Server) bridgeOrigin(m *bridgeMsg) {
	if p := s.pr; p != nil {
		p.mu.Lock()
		m.Repo = p.target.Owner + "/" + p.target.Repo
		m.PR = p.meta.Number
		m.Commit = p.meta.HeadSHA
		p.mu.Unlock()
		return
	}
	m.Repo = filepath.Base(s.ix.Root())
	if gitAvailable(s.ix.Root()) {
		m.Commit = gitHeadCommit(s.ix.Root())
	}
}

const bridgeSnippetContext = 3
const bridgeSnippetMax = 40

// bridgeSnippet returns the commented lines plus a few either side, numbered,
// from the working tree. The base side of a PR diff is not on disk, so there
// the client's copy of the selected lines is used instead.
func (s *Server) bridgeSnippet(m bridgeComment, client string) string {
	if m.Side == "LEFT" {
		return truncateLines(client, bridgeSnippetMax)
	}
	abs, _, ok := s.safePath(m.Path)
	if !ok {
		return truncateLines(client, bridgeSnippetMax)
	}
	data, err := os.ReadFile(abs)
	if err != nil || bytes.IndexByte(data, 0) >= 0 {
		return truncateLines(client, bridgeSnippetMax)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	end := m.Line
	if m.EndLine > end {
		end = m.EndLine
	}
	from := max(1, m.Line-bridgeSnippetContext)
	to := min(min(len(lines), end+bridgeSnippetContext), from+bridgeSnippetMax-1)
	var sb strings.Builder
	for n := from; n <= to; n++ {
		fmt.Fprintf(&sb, "%d: %s\n", n, lines[n-1])
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func truncateLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
