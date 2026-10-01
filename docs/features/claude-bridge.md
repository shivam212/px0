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
- **Send to Claude** (`Alt+K`, the selection bar, or the right-click and gutter menus): attaches the selected lines to the pane's composer. The message is sent as a `comment` with the path, line range and a snippet of the code around it.
- **PR review**: the `Alt+R` comment composer's main button is **Send to Claude** (`Mod+Enter`). **Add to GitHub Review** is a separate button that keeps the old draft behaviour. Every button that publishes says so (**Post Review to GitHub**, **Approve on GitHub**, **Post to GitHub**, **Reply on GitHub**). Posting a review first asks you to confirm, and the dialog says how many drafts it will publish.

## File format

One JSON object per line. px0 writes each inbox line in a single `write` on an `O_APPEND` descriptor.

Inbox (px0 → Claude):

| Field | When | Meaning |
| :--- | :--- | :--- |
| `id`, `ts` | always | message id (`m-…`) and RFC 3339 UTC time |
| `kind` | always | `comment` (anchored to code) or `chat` |
| `text` | always | what you wrote |
| `repo` | always | `owner/repo` in a PR review, else the workspace directory name |
| `pr` | PR review | PR number |
| `path`, `line`, `end_line`, `side` | `comment` | workspace-relative path, line range, and `RIGHT`/`LEFT` on a PR diff |
| `commit` | when known | PR head SHA, or the workspace's `HEAD` |
| `snippet` | `comment` | the lines with up to 3 lines of context, numbered (`12: code`) |

Outbox (Claude → px0): `{"reply_to": "<message id>", "text": "<markdown>"}`. `id` and `ts` are optional and px0 fills them in. A reply whose `reply_to` doesn't match a message is shown on its own in the timeline. px0 reads the outbox every 400 ms. It holds back a line that has no newline yet, and if the file is truncated it reads it again from the start.
