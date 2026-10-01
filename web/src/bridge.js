// web/src/bridge.js
// Comments for an already running Claude Code session, active only under
// `px0 -bridge <name>` (S.meta.bridge; server side in bridge.go).
//
// One flow: Comment (Alt+R) on a line opens a composer under it; the comment
// is Pending until "Ask Claude" sends every pending comment as one inbox line.
// Claude's replies, and follow-ups typed under them, show in a thread card
// under the line: inline in the diff, or in a card docked over the source view
// (opened from the line's gutter dot). Chat and replies to a whole batch live
// in the bottom Comments panel. Nothing here is posted to GitHub.
import { $, S, esc, api, apiPostJson, keyLabel, withKeys, doc_ } from './state.js';
import { emit } from './bus.js';
import { showToast } from './ui.js';
import { render } from './renderer.js';
import { diffview, onDiffSync } from './diff.js';
import { setReviewHandler, setReviewAnywhere, groupAgentActions, SEL_MENU_ITEMS } from './selbar.js';
import { thrMd } from './thread.js';
import { initCommentsPanel, expandCommentsPanel } from './commentspanel.js';

const br = {
  threads: [],       // [{id, path, line, end_line, side, items:[{who, id, ts, text}], github}]
  conv: [],          // [{who, id, ts, text, kind: chat|note|reply, count}]
  pending: [],       // [{id, path, line, end_line, side, text, snippet}]
  composer: null,    // {path, l1, l2, side, text} while a comment is being written
  editing: '',       // pending comment id being edited in place
  dock: null,        // {path, line}: the source-view line whose cards are docked
  seen: null,        // Claude reply ids already shown; null until the first load
  unread: 0,         // replies that arrived while the tab was hidden
  sig: '',
};

export function bridgeOn() { return !!(S.meta && S.meta.bridge); }

/* ---------- small pieces ---------- */

const brEnd = c => c.end_line || c.line;
const brRef = c => c.path + ':' + (c.line === brEnd(c) ? c.line : c.line + '-' + brEnd(c)) + (c.side === 'LEFT' ? ' (base)' : '');
const brTime = ts => { try { return new Date(ts).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }); } catch { return ''; } };
const brOnLine = (c, path, line) => c.path === path && c.side !== 'LEFT' && line >= c.line && line <= brEnd(c);
const brIsNew = it => it.who === 'claude' && br.seen && !br.seen.has(it.id);

function brMsg(who, text, ts, chips = '', isNew = false) {
  const claude = who === 'claude';
  return '<div class="br-msg' + (isNew ? ' br-new' : '') + '"><span class="br-av ' + (claude ? 'br-av-claude' : 'br-av-you') + '">' + (claude ? 'C' : 'Y') + '</span>' +
    '<div class="br-msg-main"><div class="br-msg-head"><b>' + (claude ? 'Claude' : 'You') + '</b>' +
    (ts ? '<span class="br-time">' + esc(brTime(ts)) + '</span>' : '') + chips + '</div>' +
    '<div class="br-msg-body thr-body">' + thrMd(text) + '</div></div></div>';
}

function brPendingHtml(c) {
  const open = '<div class="br-card br-pending" data-kind="pending" data-id="' + esc(c.id) + '">';
  if (br.editing === c.id) {
    return open + '<div class="br-compose"><textarea class="br-input" rows="3" spellcheck="false">' + esc(c.text) + '</textarea>' +
      '<div class="br-foot"><span class="br-hint">' + esc(keyLabel('Mod+Enter')) + ' save · Esc cancel</span>' +
      '<button class="br-btn" data-act="cancel-edit">Cancel</button><button class="br-btn br-primary" data-act="save">Save</button></div></div></div>';
  }
  const acts = '<span class="br-acts"><button class="br-link" data-act="edit">Edit</button><button class="br-link" data-act="delete">Delete</button>' +
    (S.meta.pr ? '<button class="br-link" data-act="github" title="Move to the GitHub review drafts instead">To GitHub draft</button>' : '') + '</span>';
  return open + brMsg('you', c.text, '', '<span class="br-chip br-chip-pending">Pending</span>' + acts) + '</div>';
}

function brThreadHtml(t) {
  const n = t.items.length;
  const asked = n && t.items[n - 1].who === 'you';
  const items = t.items.map((it, i) => {
    let chips = '';
    if (i === 0 && t.github) chips += '<span class="br-chip">GitHub draft</span>';
    if (i === n - 1 && asked) chips += '<span class="br-chip br-chip-asked">Asked…</span>';
    if (i === 0 && S.meta.pr && !t.github) chips += '<span class="br-acts"><button class="br-link" data-act="github" title="Also add this comment to the GitHub review drafts; the Claude thread stays here">Copy to GitHub draft</button></span>';
    return brMsg(it.who, it.text, it.ts, chips, brIsNew(it));
  }).join('');
  return '<div class="br-card" data-kind="thread" data-id="' + esc(t.id) + '">' + items +
    '<div class="br-reply"><textarea class="br-input br-reply-input" rows="1" spellcheck="false" placeholder="Reply…"></textarea>' +
    '<button class="br-btn br-primary" data-act="reply" hidden>Reply</button></div></div>';
}

function brComposerHtml(a) {
  return '<div class="br-card br-composer" data-kind="composer"><div class="br-compose">' +
    '<div class="br-where">Comment on ' + esc(brRef(a)) + '</div>' +
    '<textarea class="br-input" rows="3" spellcheck="false" autocomplete="off" placeholder="Note or question for Claude"></textarea>' +
    '<div class="br-foot"><span class="br-hint">' + esc(keyLabel('Mod+Enter')) + ' comment · ' + esc(keyLabel('Mod+Shift+Enter')) + ' comment and ask Claude · Esc cancel</span>' +
    '<button class="br-btn" data-act="cancel">Cancel</button><button class="br-btn br-primary" data-act="comment">Comment</button></div></div></div>';
}

/* ---------- placing cards ---------- */

// The diff row a comment hangs under: its last line, on its side, in a
// section that is open. A split row hangs the card under the whole pair.
function brDiffHost(c) {
  if (!diffview || diffview.hidden) return null;
  const d = doc_();
  if (!d || d.path !== c.path) return null;
  const line = brEnd(c);
  const sel = c.side === 'LEFT' ? '[data-old-l="' + line + '"]:not([data-l])' : '[data-l="' + line + '"]';
  for (const el of diffview.querySelectorAll(sel)) {
    if (el.closest('.diff-section.collapsed')) continue;
    return el.closest('.diff-row-pair') || el;
  }
  return null;
}

// Repainting on every poll would wipe a half-typed reply, so what is typed
// (and the caret) is carried across the repaint, by card.
function brKeepTyping(root) {
  const kept = [];
  for (const ta of root.querySelectorAll('.br-card textarea')) {
    const card = ta.closest('.br-card');
    kept.push({ kind: card.dataset.kind, id: card.dataset.id || '', value: ta.value, focus: document.activeElement === ta, at: ta.selectionStart });
  }
  return () => {
    for (const k of kept) {
      const ta = root.querySelector('.br-card[data-kind="' + k.kind + '"]' + (k.id ? '[data-id="' + CSS.escape(k.id) + '"]' : '') + ' textarea');
      if (!ta) continue;
      ta.value = k.value;
      if (ta.classList.contains('br-reply-input')) brSizeReply(ta);
      if (k.focus) { ta.focus(); ta.setSelectionRange(k.at, k.at); }
    }
  };
}

function brRenderInline() {
  if (!diffview) return;
  const restore = brKeepTyping(diffview);
  for (const el of diffview.querySelectorAll('.br-slot')) el.remove();
  const d = doc_();
  if (diffview.hidden || !d) return;
  const slots = new Map(); // host row -> html
  const add = (c, html) => {
    const host = brDiffHost(c);
    if (host) slots.set(host, (slots.get(host) || '') + html);
  };
  for (const t of br.threads) if (t.path === d.path) add(t, brThreadHtml(t));
  for (const c of br.pending) if (c.path === d.path) add(c, brPendingHtml(c));
  if (br.composer) add(br.composer, brComposerHtml(br.composer));
  for (const [host, html] of slots) {
    const slot = document.createElement('div');
    slot.className = 'br-slot';
    slot.innerHTML = html;
    host.after(slot);
  }
  restore();
}

// The source view has fixed-height virtual rows, so a line's cards (or the
// composer, when its line is not in the diff on screen) open docked over it.
function brRenderDock() {
  const dock = $('#br-dock');
  const restore = brKeepTyping(dock);
  let html = '';
  if (br.composer && !brDiffHost(br.composer)) {
    html = brComposerHtml(br.composer);
  } else if (br.dock && (!diffview || diffview.hidden)) {
    const { path, line } = br.dock;
    for (const t of br.threads) if (brOnLine(t, path, line)) html += brThreadHtml(t);
    for (const c of br.pending) if (brOnLine(c, path, line)) html += brPendingHtml(c);
    if (html) html = '<div class="br-dock-head"><span>' + esc(path + ':' + line) + '</span><button class="br-link" data-act="close-dock" aria-label="Close">✕</button></div>' + html;
    else br.dock = null;
  }
  dock.hidden = !html;
  dock.innerHTML = html;
  restore();
}

function brRenderDots() {
  const rank = { answered: 1, asked: 2, pending: 3 };
  const lines = {};
  const mark = (c, state) => {
    if (c.side === 'LEFT') return;
    const m = lines[c.path] || (lines[c.path] = {});
    for (let l = c.line; l <= brEnd(c); l++) if (!m[l] || rank[state] > rank[m[l]]) m[l] = state;
  };
  for (const t of br.threads) mark(t, t.items[t.items.length - 1]?.who === 'you' ? 'asked' : 'answered');
  for (const c of br.pending) mark(c, 'pending');
  S.bridgeLines = lines;
  render();
}

function brRenderAsk() {
  const n = br.pending.length;
  $('#br-ask').hidden = !n;
  $('#br-ask-send').textContent = n > 1 ? 'Ask Claude (' + n + ')' : 'Ask Claude';
  if (!n) $('#br-ask-pop').hidden = true;
}

function brRenderConv() {
  const box = $('#br-conv');
  const stick = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  box.innerHTML = br.conv.length
    ? br.conv.map(it => brMsg(it.who, it.text, it.ts,
      it.kind === 'note' ? '<span class="br-chip">note on ' + it.count + (it.count === 1 ? ' comment' : ' comments') + '</span>' : '',
      brIsNew(it))).join('')
    : '<div class="pr-comments-empty">Chat with your Claude Code session here. Its answers to a whole batch of comments show here too.</div>';
  if (stick) box.scrollTop = box.scrollHeight;
  if (!S.meta.pr) $('#pr-comments-count').textContent = br.conv.length ? String(br.conv.length) : '';
}

function brRenderAll() {
  brRenderInline();
  brRenderDock();
  brRenderDots();
  brRenderAsk();
  brRenderConv();
}

/* ---------- data ---------- */

// New Claude replies: a toast saying where, the Comments panel opened when
// the reply is there, and a count in the tab title while it is in the background.
function brNoticeReplies() {
  const claude = br.threads.flatMap(t => t.items.map(it => ({ it, t })))
    .concat(br.conv.map(it => ({ it, t: null })))
    .filter(x => x.it.who === 'claude');
  if (!br.seen) { br.seen = new Set(claude.map(x => x.it.id)); return; }
  const fresh = claude.filter(x => !br.seen.has(x.it.id));
  if (!fresh.length) return;
  showToast('Claude', fresh[0].t ? 'replied on ' + brRef(fresh[0].t) : 'replied in Comments', 3500);
  if (fresh.some(x => !x.t)) expandCommentsPanel();
  if (document.hidden) {
    br.unread += fresh.length;
    document.title = '(' + br.unread + ') ' + S.meta.name + ' - px0';
  }
  // Seen once this paint has flashed them.
  setTimeout(() => { for (const x of fresh) br.seen.add(x.it.id); }, 4000);
}

async function brRefresh() {
  let j;
  try { j = await api('/api/bridge'); } catch { return; } // transient: the next tick tries again
  const sig = JSON.stringify([j.threads, j.conversation, j.pending]);
  if (sig === br.sig) return;
  br.sig = sig;
  br.threads = j.threads || [];
  br.conv = j.conversation || [];
  br.pending = j.pending || [];
  if (br.editing && !br.pending.some(c => c.id === br.editing)) br.editing = '';
  brNoticeReplies();
  brRenderAll();
}

async function brPost(path, body, fail) {
  try {
    const r = await apiPostJson(path, body);
    br.sig = '';
    await brRefresh();
    return r;
  } catch (e) {
    showToast('!', e.message || fail);
    return null;
  }
}

/* ---------- actions ---------- */

// The "Comment" action (Alt+R) under -bridge, in place of pr.js's
// GitHub-only composer: a selection, or the line a gutter button was on.
export function openComposer(info) {
  if (!info || !info.path) return;
  const side = info.side || (info.fromDiff ? 'RIGHT' : '');
  const l1 = side === 'LEFT' ? (info.delL1 || info.l1) : info.l1;
  const l2 = (side === 'LEFT' ? (info.delL2 || info.l2) : info.l2) || l1;
  br.composer = { path: info.path, line: l1, end_line: l2 > l1 ? l2 : 0, side, text: info.text || '' };
  br.dock = null;
  brRenderInline();
  brRenderDock();
  const card = document.querySelector('.br-composer');
  card?.querySelector('textarea')?.focus();
  card?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
}

function brCloseComposer() {
  br.composer = null;
  brRenderInline();
  brRenderDock();
}

async function brComment(card, ask) {
  const a = br.composer;
  const text = card.querySelector('textarea')?.value.trim();
  if (!a || !text) return;
  const added = await brPost('/api/bridge/drafts',
    { op: 'add', text, path: a.path, line: a.line, endLine: brEnd(a), side: a.side, snippet: a.text }, 'Could not add the comment');
  if (!added) return;
  br.composer = null;
  brRenderAll();
  if (ask) await brAsk();
}

async function brAsk() {
  const n = br.pending.length;
  if (!n) return;
  const note = $('#br-ask-note');
  if (!(await brPost('/api/bridge/review', { text: note.value.trim() }, 'Could not ask Claude'))) return;
  note.value = '';
  $('#br-ask-pop').hidden = true;
  showToast('✓', 'Asked Claude about ' + n + (n === 1 ? ' comment' : ' comments'));
}

function brSizeReply(ta) {
  ta.style.height = 'auto';
  ta.style.height = Math.min(160, ta.scrollHeight) + 'px';
  const btn = ta.parentElement?.querySelector('[data-act="reply"]');
  if (btn) btn.hidden = !ta.value.trim();
}

async function brCardAction(card, act) {
  const id = card?.dataset.id;
  if (act === 'comment') return brComment(card, false);
  if (act === 'cancel') return brCloseComposer();
  if (act === 'close-dock') { br.dock = null; return brRenderDock(); }
  if (act === 'edit') {
    br.editing = id;
    brRenderAll();
    document.querySelector('.br-card[data-kind="pending"][data-id="' + CSS.escape(id) + '"] textarea')?.focus();
    return;
  }
  if (act === 'cancel-edit') { br.editing = ''; return brRenderAll(); }
  if (act === 'save') {
    const text = card.querySelector('textarea')?.value.trim();
    if (!text) return;
    br.editing = '';
    return brPost('/api/bridge/drafts', { op: 'update', id, text }, 'Could not save');
  }
  if (act === 'delete') return brPost('/api/bridge/drafts', { op: 'delete', id }, 'Could not delete');
  if (act === 'github') {
    if (await brPost('/api/bridge/to-github', { id }, 'Could not add the GitHub draft')) {
      emit('pr:drafts-changed');
      showToast('✓', 'Added to the GitHub review drafts (not posted)');
    }
    return;
  }
  if (act === 'reply') {
    const ta = card.querySelector('.br-reply-input');
    const text = ta?.value.trim();
    if (!text) return;
    ta.value = '';
    brSizeReply(ta);
    if (!(await brPost('/api/bridge/reply', { id, text }, 'Could not send the reply'))) ta.value = text;
  }
}

function brWireCards(root) {
  // Clicks in a card must not move the editor's caret or selection.
  root.addEventListener('mousedown', e => { if (e.target.closest('.br-card, .br-dock-head')) e.stopPropagation(); });
  root.addEventListener('click', e => {
    const btn = e.target.closest('[data-act]');
    if (!btn || !root.contains(btn)) return;
    e.stopPropagation();
    brCardAction(btn.closest('.br-card'), btn.dataset.act);
  });
  root.addEventListener('input', e => { if (e.target.classList?.contains('br-reply-input')) brSizeReply(e.target); });
  root.addEventListener('keydown', e => {
    const ta = e.target.closest?.('.br-card textarea');
    if (!ta) return;
    e.stopPropagation(); // typing here must not trigger editor shortcuts
    const card = ta.closest('.br-card');
    const kind = card.dataset.kind;
    const mod = e.metaKey || e.ctrlKey;
    if (e.key === 'Escape') {
      e.preventDefault();
      if (kind === 'composer') brCloseComposer();
      else if (kind === 'pending') brCardAction(card, 'cancel-edit');
      else ta.blur();
    } else if (e.key === 'Enter' && mod && kind === 'composer') {
      e.preventDefault();
      brComment(card, e.shiftKey);
    } else if (e.key === 'Enter' && mod && kind === 'pending') {
      e.preventDefault();
      brCardAction(card, 'save');
    } else if (e.key === 'Enter' && kind === 'thread' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      brCardAction(card, 'reply');
    }
  });
}

/* ---------- setup ---------- */

// The coding-harness features start a new agent process rather than talking
// to this Claude session, so under -bridge they stay available but grouped
// as "Agent": one entry in the menus, the Threads tab, the git panel picker.
function brGroupAgent() {
  groupAgentActions();
  const tab = $('#tab-threads');
  if (tab?.firstChild?.nodeType === 3) tab.firstChild.textContent = 'Agent';
  const picker = $('.git-agent-picker');
  const h = $('#git-harness'), m = $('#git-model');
  if (picker && h && m && !$('#git-agent-group')) {
    const det = document.createElement('details');
    det.id = 'git-agent-group';
    det.className = 'br-agent-group';
    det.innerHTML = '<summary>Agent ▸ harness and model</summary>';
    det.append(h, m);
    picker.prepend(det);
  }
}

export function initBridge() {
  if (!bridgeOn()) return;
  document.body.classList.add('bridge-mode');

  setReviewHandler(openComposer);
  setReviewAnywhere(true);
  const at = SEL_MENU_ITEMS.findIndex(i => i.sel === 'review-comment');
  const item = at >= 0 ? SEL_MENU_ITEMS.splice(at, 1)[0] : { sel: 'review-comment', keys: 'Alt+R' };
  SEL_MENU_ITEMS.unshift({ ...item, label: 'Comment' });
  const sel = $('#footer-sel');
  if (sel && !sel.querySelector('[data-sel="review-comment"]')) {
    const btn = document.createElement('button');
    btn.className = 'footer-btn';
    btn.dataset.sel = 'review-comment';
    btn.title = withKeys('Comment on this selection for your Claude Code session ({Alt+R})');
    btn.innerHTML = '<span class="footer-btn-label">Comment</span><kbd class="footer-kbd">' + esc(keyLabel('Alt+R')) + '</kbd>';
    sel.prepend(btn);
  }
  brGroupAgent();

  initCommentsPanel();
  $('#br-conv-section').hidden = false;
  if (!S.meta.pr) {
    $('#pr-comments-list').hidden = true;
    $('#pr-issue-compose').hidden = true;
  }
  const chatIn = $('#br-chat-input');
  const sendChat = async () => {
    const text = chatIn.value.trim();
    if (!text) return;
    chatIn.value = '';
    if (!(await brPost('/api/bridge/chat', { text }, 'Could not send'))) chatIn.value = text;
  };
  $('#br-chat-send').addEventListener('click', sendChat);
  chatIn.addEventListener('keydown', e => {
    e.stopPropagation();
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); sendChat(); }
  });

  $('#br-ask-send').addEventListener('click', brAsk);
  $('#br-ask-more').addEventListener('click', () => {
    const pop = $('#br-ask-pop');
    pop.hidden = !pop.hidden;
    if (!pop.hidden) $('#br-ask-note').focus();
  });
  $('#br-ask-note').addEventListener('keydown', e => {
    e.stopPropagation();
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); brAsk(); }
    else if (e.key === 'Escape') $('#br-ask-pop').hidden = true;
  });

  if (diffview) brWireCards(diffview);
  brWireCards($('#br-dock'));
  onDiffSync(brRenderInline);
  // A gutter dot in the source view opens that line's cards.
  document.addEventListener('mousedown', e => {
    const dot = e.target.closest?.('.br-dot');
    const d = doc_();
    if (!dot || !d) return;
    e.preventDefault();
    e.stopPropagation();
    br.dock = { path: d.path, line: +dot.dataset.l };
    br.composer = null;
    brRenderDock();
  }, true);

  document.addEventListener('visibilitychange', () => {
    if (document.hidden) return;
    br.unread = 0;
    document.title = S.meta.name + ' - px0';
    brRefresh();
  });
  brRefresh();
  setInterval(() => { if (!document.hidden) brRefresh(); }, 1500);
}
