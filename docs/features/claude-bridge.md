# Claude Session Bridge (`-bridge`)

`px0 -bridge <name>` connects px0 to a Claude Code session you already have open in a terminal. Comments and chat messages you write in px0 go to that session, not to a fresh harness process and not to GitHub, and its replies show up in px0 under the message they answer.

## Why

- `-agent claude` starts a new Claude process for every edit from the UI. It never reaches the conversation you are having in the terminal.
- In a PR review, a review comment ends up on GitHub as soon as the review is submitted. A note meant for your own Claude session should never get there, and a submitted review can't be deleted.

## Starting it

```bash
px0 -bridge pr-review https://github.com/owner/repo/pull/123
px0 -bridge auto .        # channel named after the PR (owner-repo-pr123) or the workspace directory
```

At startup px0 prints the channel, both file paths, and an instruction to paste into the Claude Code session:

```text
  bridge:     pr-review
  inbox:      ~/.px0/bridge/pr-review/inbox.jsonl
  outbox:     ~/.px0/bridge/pr-review/outbox.jsonl

paste into your Claude Code session:
Use the Monitor tool to watch `tail -n0 -F ~/.px0/bridge/pr-review/inbox.jsonl` (keep it running). ...
```

The files live under `$XDG_CONFIG_HOME/px0/bridge/` when that is set, otherwise `~/.px0/bridge/`. They are never in the workspace. Without `-bridge`, px0 behaves as before.

## Using it

- **Comment** (`Alt+R`, the selection bar, or the right-click and gutter menus, on any file): opens a composer under the line, inline in the diff or docked over the source view. **Comment** (`Mod+Enter`) saves it as **Pending**. `Mod+Shift+Enter` saves it and asks Claude straight away.
- **Pending comments** show as cards under their lines, with Edit and Delete on hover, and as a dot in the gutter. Click a source-view dot to open that line's cards. They are saved in the workspace session file, so they survive a restart. In a PR review under `-bridge`, that session file is keyed by the PR rather than its temp checkout.
- **Ask Claude (N)** (bottom right) sends every pending comment as **one** `review` line, so the Claude session wakes once for the whole batch. Its ▾ adds an optional overall note.
- **Threads**: once sent, a comment shows "Asked…" until Claude replies. Replies appear under the comment they answer. Typing in **Reply…** under a thread and pressing Enter sends a follow-up immediately: a `review` line with one comment whose `in_reply_to` is the thread's first comment id.
- **Comments panel** (the bottom panel): its **Claude** section holds chat (free-form `chat` messages), the overall notes sent with a batch, and Claude's answers to a whole batch or chat message. A reply that lands there while the panel is collapsed expands it.
- **PR review**: the GitHub verdict row (summary, Submit Review, Request Changes, Approve) sits behind **GitHub ▾** in the PR bar, and submitting asks for confirmation. A pending card's **To GitHub draft** moves the comment into the GitHub review drafts. A sent thread's **Copy to GitHub draft** also adds it there, and the thread keeps its Claude replies locally with a "GitHub draft" chip. Nothing reaches GitHub until you submit the review.
- **Agent ▸**: Start Agent Thread, Edit Inline, the Agent tab and the harness/model picker are grouped under "Agent" (the menus, the selection bar and the git panel), not removed.

## File format

One JSON object per line. px0 writes each inbox line in a single `write` on an `O_APPEND` descriptor.

Inbox (px0 → Claude):

| Field | When | Meaning |
| :--- | :--- | :--- |
| `id`, `ts` | always | `rv-…` for a review, `m-…` for chat; RFC 3339 UTC time |
| `kind` | always | `review` (code comments) or `chat` |
| `text` | always | chat: the message. review: the optional overall note (may be `""`) |
| `repo` | always | `owner/repo` in a PR review, else the workspace directory name |
| `pr` | PR review | PR number |
| `commit` | when known | PR head SHA, or the workspace's `HEAD` |
| `comments` | `review` | `[{id, path, line, end_line, side, text, snippet, in_reply_to}]` |

Each comment has a `c-…` id, a workspace-relative `path`, `line` and optional `end_line`, `side` (`RIGHT`/`LEFT`, PR diffs only), the `text`, and a `snippet` of the lines with up to 3 lines of context, numbered (`12: code`). `in_reply_to` is set only on a follow-up and names the earlier comment id whose thread it continues.

A batch, then a follow-up:

```json
{"id":"rv-75104f176d2e","ts":"2026-10-01T10:34:54.1Z","kind":"review","text":"Overall: check the startup order","repo":"px0","commit":"74bc838",
 "comments":[{"id":"c-5537bd5216f7","path":"bridge.go","line":170,"text":"Why this prefix?","snippet":"167: ...\n170: ..."},
             {"id":"c-850674f99dae","path":"main.go","line":171,"end_line":172,"text":"Order matters here?","snippet":"168: ..."}]}
{"id":"rv-32b60e034757","ts":"2026-10-01T10:40:02.3Z","kind":"review","text":"","repo":"px0","commit":"74bc838",
 "comments":[{"id":"c-1980dbb7cc1f","path":"bridge.go","line":170,"text":"Why not m- then?","snippet":"167: ...","in_reply_to":"c-5537bd5216f7"}]}
```

(Shown wrapped here. On disk each is a single line.) Older `kind: "comment"` lines in an existing inbox are read as a one-comment review.

Outbox (Claude → px0): `{"reply_to": "<id>", "text": "<markdown>"}`. With a comment's `c-…` id the reply shows under that comment's thread; with a review's `rv-…` id or a chat's `m-…` id it shows in the Comments panel as an answer to the whole message. `id` and `ts` are optional and px0 fills them in. A reply whose `reply_to` matches nothing is shown in the Comments panel on its own. px0 reads the outbox every 400 ms. It holds back a line that has no newline yet, and if the file is truncated it reads it again from the start.

The startup instruction:

```text
Use the Monitor tool to watch `tail -n0 -F <inbox>` (keep it running). Each line is one JSON message I wrote in px0. kind "review": my code comments in comments[], each with id, path, line, end_line, side, text and a numbered snippet; text is an optional overall note; a comment with in_reply_to is a follow-up in the thread of that earlier comment id. kind "chat": a free-form message in text. Reply by appending one JSON line per answer to <outbox>: {"reply_to":"<comment id>","text":"<markdown>"} for each comment (shown under that comment), and reply_to the review or chat id only for an overall answer. For example: jq -nc --arg r '<id>' --arg t '<reply>' '{reply_to:$r,text:$t}' >> <outbox>. Do not post anything to GitHub unless I ask.
```

## HTTP API

| Route | Body | Does |
| :--- | :--- | :--- |
| `GET /api/bridge` | | `{channel, inbox, outbox, threads, conversation, pending}` |
| `POST /api/bridge/drafts` | `{op: add\|update\|delete, ...}` | edits the pending comments |
| `POST /api/bridge/review` | `{text}` | sends all pending comments as one review line (Ask Claude) |
| `POST /api/bridge/reply` | `{id, text}` | sends a follow-up in the thread of comment `id` |
| `POST /api/bridge/chat` | `{text}` | sends a chat line |
| `POST /api/bridge/to-github` | `{id}` | PR only: adds a pending or sent comment to the GitHub review drafts |

None of them call the GitHub API. Without `-bridge` they return 404.
