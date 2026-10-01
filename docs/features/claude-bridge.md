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

- **Claude pane**: a **Claude** tab in the right sidebar lists everything sent on the channel. Each message has Claude's replies under it, and a "waiting for a reply" note until the first one arrives. Typing in the box at the bottom sends a free-form `chat` message.
- **Comment for Claude** (`Alt+K`, the selection bar, or the right-click and gutter menus): attaches the selected lines to the pane's composer. **Add Draft** (Enter) saves it as a draft. **Send Now** (`Mod+Enter`) sends that one comment by itself as a `comment` line.
- **Drafts**: unsent comments are listed above the composer, where you can edit or delete them, and their lines are marked in the editor and diff gutters. They are saved in the workspace session file, so they survive a restart. In a PR review under `-bridge`, that session file is keyed by the PR rather than its temp checkout. **Send N drafts to Claude** sends all of them, plus an optional overall note, as **one** `review` line. One line wakes the Claude session once for the whole batch.
- **PR review**: the `Alt+R` comment composer's main button is **Add Claude Draft** (`Mod+Enter`), with **Send Now** next to it. **Add to GitHub Review** is a separate button that keeps the GitHub draft behaviour; those drafts stay separate from Claude drafts. Every button that publishes says so (**Post Review to GitHub**, **Approve on GitHub**, **Post to GitHub**, **Reply on GitHub**). Posting a review first asks you to confirm, and the dialog says how many drafts it will publish.

## File format

One JSON object per line. px0 writes each inbox line in a single `write` on an `O_APPEND` descriptor.

Inbox (px0 → Claude):

| Field | When | Meaning |
| :--- | :--- | :--- |
| `id`, `ts` | always | message id (`m-…`) and RFC 3339 UTC time |
| `kind` | always | `comment` (one comment, sent now), `chat`, or `review` (a batch of drafts) |
| `text` | always | what you wrote |
| `repo` | always | `owner/repo` in a PR review, else the workspace directory name |
| `pr` | PR review | PR number |
| `path`, `line`, `end_line`, `side` | `comment` | workspace-relative path, line range, and `RIGHT`/`LEFT` on a PR diff |
| `commit` | when known | PR head SHA, or the workspace's `HEAD` |
| `snippet` | `comment` | the lines with up to 3 lines of context, numbered (`12: code`) |
| `comments` | `review` | the drafts: `[{id, path, line, end_line, side, text, snippet}]`; `text` is then the overall note (may be `""`) |

A `review` line looks like this:

```json
{"id":"rv-75104f176d2e","ts":"2026-10-01T10:34:54Z","kind":"review","text":"Overall: check the startup order","repo":"px0","commit":"74bc838",
 "comments":[{"id":"c-5537bd5216f7","path":"bridge.go","line":170,"text":"Why this prefix?","snippet":"167: ...\n170: ..."},
             {"id":"c-850674f99dae","path":"main.go","line":171,"end_line":172,"text":"Order matters here?","snippet":"168: ..."}]}
```

(Shown wrapped here. On disk it is a single line.)

Outbox (Claude → px0): `{"reply_to": "<id>", "text": "<markdown>"}`, where the id is a message's id, or for a review either the review's `rv-…` id (one answer to the whole batch) or a single comment's `c-…` id (an answer to that comment). Both are shown threaded. `id` and `ts` are optional and px0 fills them in. A reply whose `reply_to` doesn't match a message is shown on its own in the timeline. px0 reads the outbox every 400 ms. It holds back a line that has no newline yet, and if the file is truncated it reads it again from the start.
