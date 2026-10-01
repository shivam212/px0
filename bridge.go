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
	"sort"
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

// bridgeMsg is one inbox line. Kind "review" carries code comments (one or
// many, with Text as an optional overall note); kind "chat" is free text.
type bridgeMsg struct {
	ID       string          `json:"id"`
	TS       string          `json:"ts"`
	Kind     string          `json:"kind"`
	Text     string          `json:"text"`
	Repo     string          `json:"repo,omitempty"`
	PR       int             `json:"pr,omitempty"`
	Commit   string          `json:"commit,omitempty"`
	Comments []bridgeComment `json:"comments,omitempty"`
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
	// InReplyTo is set on a follow-up typed under an earlier comment: the id of
	// the comment it follows up on, so it lands in that thread.
	InReplyTo string `json:"in_reply_to,omitempty"`
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
		var msg bridgeMsg
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID == "" {
			continue
		}
		if msg.Kind == "comment" {
			// Earlier builds wrote one comment per line with the anchor at the
			// top level; read it as a one-comment review.
			var c bridgeComment
			json.Unmarshal(sc.Bytes(), &c)
			msg.Kind, msg.Text, msg.Comments = "review", "", []bridgeComment{c}
		}
		b.sent = append(b.sent, msg)
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
	m.TS = time.Now().UTC().Format(time.RFC3339Nano)
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
			r.TS = time.Now().UTC().Format(time.RFC3339Nano)
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
		"Each line is one JSON message I wrote in px0. "+
		"kind \"review\": my code comments in comments[], each with id, path, line, end_line, side, text and a numbered snippet; text is an optional overall note; "+
		"a comment with in_reply_to is a follow-up in the thread of that earlier comment id. "+
		"kind \"chat\": a free-form message in text. "+
		"Reply by appending one JSON line per answer to %s: {\"reply_to\":\"<comment id>\",\"text\":\"<markdown>\"} for each comment (shown under that comment), "+
		"and reply_to the review or chat id only for an overall answer. "+
		"For example: jq -nc --arg r '<id>' --arg t '<reply>' '{reply_to:$r,text:$t}' >> %s. "+
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

// handleBridge returns the channel as the UI draws it: comment threads
// (inline under their line), the conversation (chat, overall notes, and
// replies to a whole review or chat), and the pending comments.
func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	if !s.bridgeOrFail(w) {
		return
	}
	sent, replies := s.bridge.snapshot()
	threads, conv := bridgeView(sent, replies)
	moved := s.session.Get().BridgeGitHub
	if p := s.pr; p != nil && len(moved) > 0 {
		p.mu.Lock()
		live := make(map[int64]bool, len(p.comments))
		for _, c := range p.comments {
			live[c.ID] = true
		}
		p.mu.Unlock()
		for i := range threads {
			threads[i].GitHub = live[moved[threads[i].ID]]
		}
	}
	writeJSON(w, map[string]any{
		"channel":      s.bridge.channel,
		"inbox":        s.bridge.inbox,
		"outbox":       s.bridge.outbox,
		"threads":      threads,
		"conversation": conv,
		"pending":      s.bridgeDrafts(),
	})
}

// bridgeItem is one message in a thread or the conversation.
type bridgeItem struct {
	Who   string `json:"who"` // "you" or "claude"
	ID    string `json:"id"`
	TS    string `json:"ts"`
	Text  string `json:"text"`
	Kind  string `json:"kind,omitempty"`  // conversation only: "chat", "note" (a review's overall note) or "reply"
	Count int    `json:"count,omitempty"` // "note": how many comments the review carried

	at time.Time // ordering key; a reply never sorts before what it answers
}

func bridgeTime(ts string) time.Time { t, _ := time.Parse(time.RFC3339Nano, ts); return t }

// bridgeThreadView is one code comment with its follow-ups and Claude's
// replies, in order. ID is the first comment's id.
type bridgeThreadView struct {
	bridgeComment
	Items  []bridgeItem `json:"items"`
	GitHub bool         `json:"github,omitempty"` // also added as a GitHub review draft
}

// bridgeView threads the channel. A comment starts a thread unless its
// in_reply_to names a comment already in one. A reply goes to the thread
// holding the comment its reply_to names; a reply to a review or chat id, or
// to nothing known, goes to the conversation.
func bridgeView(sent []bridgeMsg, replies []bridgeReply) ([]bridgeThreadView, []bridgeItem) {
	threads := []bridgeThreadView{}
	conv := []bridgeItem{}
	threadOf := map[string]int{}
	sentAt := map[string]time.Time{} // every message and comment id -> when it was sent
	for _, m := range sent {
		at := bridgeTime(m.TS)
		sentAt[m.ID] = at
		switch m.Kind {
		case "chat":
			conv = append(conv, bridgeItem{Who: "you", ID: m.ID, TS: m.TS, Text: m.Text, Kind: "chat", at: at})
		case "review":
			for _, c := range m.Comments {
				t, ok := threadOf[c.InReplyTo]
				if c.InReplyTo == "" || !ok {
					root := c
					root.InReplyTo = ""
					threads = append(threads, bridgeThreadView{bridgeComment: root, Items: []bridgeItem{}})
					t = len(threads) - 1
				}
				threads[t].Items = append(threads[t].Items, bridgeItem{Who: "you", ID: c.ID, TS: m.TS, Text: c.Text, at: at})
				threadOf[c.ID] = t
				sentAt[c.ID] = at
			}
			if m.Text != "" {
				conv = append(conv, bridgeItem{Who: "you", ID: m.ID, TS: m.TS, Text: m.Text, Kind: "note", Count: len(m.Comments), at: at})
			}
		}
	}
	for _, r := range replies {
		item := bridgeItem{Who: "claude", ID: r.ID, TS: r.TS, Text: r.Text, at: bridgeTime(r.TS)}
		if q, ok := sentAt[r.ReplyTo]; ok && item.at.Before(q) {
			item.at = q // a skewed clock on the writer's side
		}
		if t, ok := threadOf[r.ReplyTo]; ok {
			threads[t].Items = append(threads[t].Items, item)
			continue
		}
		item.Kind = "reply"
		conv = append(conv, item)
	}
	for i := range threads {
		sortBridgeItems(threads[i].Items)
	}
	sortBridgeItems(conv)
	return threads, conv
}

// sortBridgeItems orders by time, keeping arrival order on a tie. px0 writes
// nanosecond times, so its own lines and the replies it stamps never tie.
func sortBridgeItems(items []bridgeItem) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
}

func bridgeFindComment(sent []bridgeMsg, id string) (bridgeComment, bool) {
	for _, m := range sent {
		for _, c := range m.Comments {
			if c.ID == id {
				return c, true
			}
		}
	}
	return bridgeComment{}, false
}

func decodeBridgeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<17)).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func (s *Server) sendBridge(w http.ResponseWriter, m bridgeMsg) (bridgeMsg, bool) {
	s.bridgeOrigin(&m)
	sent, err := s.bridge.Send(m)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return sent, false
	}
	return sent, true
}

// handleBridgeChat sends one free-form message from the Comments panel.
func (s *Server) handleBridgeChat(w http.ResponseWriter, r *http.Request) {
	if !s.bridgeOrFail(w) || !localPost(w, r) {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !decodeBridgeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		fail(w, http.StatusBadRequest, "text is required")
		return
	}
	if sent, ok := s.sendBridge(w, bridgeMsg{Kind: "chat", Text: strings.TrimSpace(body.Text)}); ok {
		writeJSON(w, sent)
	}
}

// handleBridgeReply sends a follow-up typed under a comment thread right
// away, as a one-comment review whose in_reply_to names that comment.
func (s *Server) handleBridgeReply(w http.ResponseWriter, r *http.Request) {
	if !s.bridgeOrFail(w) || !localPost(w, r) {
		return
	}
	var body struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if !decodeBridgeBody(w, r, &body) {
		return
	}
	text := strings.TrimSpace(body.Text)
	sent, _ := s.bridge.snapshot()
	prev, ok := bridgeFindComment(sent, body.ID)
	if !ok || text == "" {
		fail(w, http.StatusBadRequest, "a reply needs text and the id of a comment already sent")
		return
	}
	c, ok := s.bridgeAnchor(prev.Path, prev.Line, prev.EndLine, prev.Side, prev.Snippet)
	if !ok { // the file is gone: keep the anchor as it was sent
		c = prev
	}
	c.ID, c.Text, c.InReplyTo = bridgeID("c-"), text, prev.ID
	if out, ok := s.sendBridge(w, bridgeMsg{Kind: "review", Comments: []bridgeComment{c}}); ok {
		writeJSON(w, out)
	}
}

// handleBridgeToGitHub copies a Claude comment into the GitHub review drafts.
// A pending one leaves the Claude queue; one already sent keeps its thread
// and Claude's replies here, and is marked as also being a GitHub draft.
func (s *Server) handleBridgeToGitHub(w http.ResponseWriter, r *http.Request) {
	if !s.bridgeOrFail(w) || !s.prOrFail(w) || !localPost(w, r) {
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if !decodeBridgeBody(w, r, &body) {
		return
	}
	var pending *bridgeComment
	for _, d := range s.bridgeDrafts() {
		if d.ID == body.ID {
			pending = &d
			break
		}
	}
	if pending != nil {
		gh := s.addPRDraft(pending.Path, pending.Line, pending.Side, pending.Text)
		s.session.Update(func(ws *WorkspaceSession) {
			for i, d := range ws.BridgeDrafts {
				if d.ID == body.ID {
					ws.BridgeDrafts = append(ws.BridgeDrafts[:i:i], ws.BridgeDrafts[i+1:]...)
					break
				}
			}
		})
		writeJSON(w, gh)
		return
	}
	sent, _ := s.bridge.snapshot()
	c, ok := bridgeFindComment(sent, body.ID)
	if !ok {
		fail(w, http.StatusNotFound, "no such comment")
		return
	}
	gh := s.addPRDraft(c.Path, c.Line, c.Side, c.Text)
	s.session.Update(func(ws *WorkspaceSession) {
		if ws.BridgeGitHub == nil {
			ws.BridgeGitHub = map[string]int64{}
		}
		ws.BridgeGitHub[c.ID] = gh.ID
	})
	writeJSON(w, gh)
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

// handleBridgeReview is Ask Claude: it sends every pending comment, plus an
// optional overall note, as one "review" line, so the session is woken once
// for the whole batch. Pending comments are cleared only after the line is written.
func (s *Server) handleBridgeReview(w http.ResponseWriter, r *http.Request) {
	if !s.bridgeOrFail(w) || !localPost(w, r) {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !decodeBridgeBody(w, r, &body) {
		return
	}
	pending := s.bridgeDrafts()
	if len(pending) == 0 {
		fail(w, http.StatusBadRequest, "no pending comments to send")
		return
	}
	sent, ok := s.sendBridge(w, bridgeMsg{Kind: "review", Text: strings.TrimSpace(body.Text), Comments: pending})
	if !ok {
		return
	}
	sentIDs := make(map[string]bool, len(pending))
	for _, d := range pending {
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
