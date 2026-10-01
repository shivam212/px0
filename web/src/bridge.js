// web/src/bridge.js
// The Claude pane, active only under `px0 -bridge <name>` (S.meta.bridge, see
// bridge.go). Comments on code collect as drafts (kept in the session file)
// and go to the channel's inbox.jsonl together as one "review" line, or one at
// a time with Send Now; chat messages go straight away. An already running
// Claude Code session watches the inbox, and its replies come back through
// outbox.jsonl, shown under the message or the single comment they answer.
// Everything here stays on this machine: nothing is posted to GitHub.
import { $, S, esc, api, apiPostJson, keyLabel, withKeys, doc_ } from './state.js';
import { on, emit } from './bus.js';
import { showToast } from './ui.js';
import { openFile } from './tabs.js';
import { render } from './renderer.js';
import { diffview, setPRSyncHandler } from './diff.js';
import { setBridgeHandler, SEL_MENU_ITEMS } from './selbar.js';
import { showRightInspector } from './inspector.js';
import { thrMd } from './thread.js';

const br = {
  messages: [],  // sent messages, each with its replies (and comment_replies for a review)
  loose: [],     // replies with no matching reply_to
  drafts: [],    // unsent comments: {id, path, line, end_line, side, text, snippet}
  editing: '',   // id of the draft being edited in place
  anchor: null,  // {path, l1, l2, side, text} the next comment is about, or null for chat
  seen: 0,       // replies counted when the pane was last on screen
  sig: '',       // last rendered snapshot, to skip identical repaints
};
const brEl = {};

const brRef = a => a.path + ':' + (a.l1 === a.l2 || !a.l2 ? a.l1 : a.l1 + '-' + a.l2) + (a.side === 'LEFT' ? ' (base)' : '');
const brCommentRef = c => brRef({ path: c.path, l1: c.line, l2: c.end_line || c.line, side: c.side });
const brReplyCount = () => br.messages.reduce((n, m) =>
  n + m.replies.length + Object.values(m.comment_replies || {}).reduce((k, r) => k + r.length, 0), 0) + br.loose.length;
const brVisible = () => !!brEl.pane?.classList.contains('active') && !document.body.classList.contains('right-hidden');

function brTime(ts) {
  try { return new Date(ts).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }); } catch { return ''; }
}

function brLocHtml(c) {
  return '<button class="thr-file br-loc" data-path="' + esc(c.path) + '" data-line="' + (c.line || 1) + '" title="Jump to this code">' + esc(brCommentRef(c)) + '</button>';
}

function brReplyHtml(r) {
  return '<div class="thr-msg thr-agent"><div class="thr-who">Claude · ' + esc(brTime(r.ts)) + '</div>' +
    '<div class="thr-body thr-reply">' + thrMd(r.text) + '</div></div>';
}

function brMessageHtml(m) {
  const per = m.comment_replies || {};
  const comments = m.comments || [];
  const answered = m.replies.length || Object.keys(per).length;
  let h = '<div class="br-item"><div class="thr-msg thr-user"><div class="thr-who">You · ' + esc(m.kind) +
    (m.kind === 'review' ? ' · ' + comments.length + (comments.length === 1 ? ' comment' : ' comments') : '') +
    ' · ' + esc(brTime(m.ts)) + '</div>';
  if (m.path) h += brLocHtml(m);
  if (m.text) h += '<div class="thr-body">' + thrMd(m.text) + '</div>';
  h += '</div>';
  for (const c of comments) {
    h += '<div class="br-comment"><div class="thr-msg thr-user">' + brLocHtml(c) + '<div class="thr-body">' + thrMd(c.text) + '</div></div>' +
      (per[c.id] || []).map(brReplyHtml).join('') + '</div>';
  }
  h += m.replies.map(brReplyHtml).join('');
  if (!answered) h += '<div class="thr-who br-waiting">waiting for a reply…</div>';
  return h + '</div>';
}

function brDraftHtml(d) {
  const body = br.editing === d.id
    ? '<textarea class="agent-input br-draft-edit" rows="3" spellcheck="false">' + esc(d.text) + '</textarea>' +
      '<div class="agent-foot"><span class="grow"></span><button class="agent-cancel br-draft-cancel">Cancel</button><button class="agent-send br-draft-save">Save</button></div>'
    : '<div class="thr-body">' + thrMd(d.text) + '</div>' +
      '<div class="agent-foot"><span class="grow"></span><button class="mini br-draft-edit-btn">Edit</button><button class="mini br-draft-del">Delete</button></div>';
  return '<div class="br-draft-item" data-id="' + esc(d.id) + '">' + brLocHtml(d) + body + '</div>';
}

function brRenderDrafts() {
  const n = br.drafts.length;
  brEl.drafts.hidden = !n;
  $('#br-drafts-count').textContent = n + (n === 1 ? ' draft' : ' drafts');
  brEl.reviewSend.textContent = 'Send ' + n + (n === 1 ? ' draft' : ' drafts') + ' to Claude';
  brEl.draftList.innerHTML = br.drafts.map(brDraftHtml).join('');
  S.bridgeDrafts = br.drafts; // renderer.js marks these lines
  render();
  markBridgeDiff();
  emit('bridge:drafts');
}

function brRender() {
  const sig = br.messages.length + ':' + brReplyCount();
  if (sig === br.sig) return;
  br.sig = sig;
  const box = brEl.msgs;
  const stick = box.scrollHeight - box.scrollTop - box.clientHeight < 60;
  // One timeline: each message with its replies, and loose replies by arrival time.
  const items = br.messages.map(m => ({ ts: m.ts, html: brMessageHtml(m) }))
    .concat(br.loose.map(r => ({ ts: r.ts, html: brReplyHtml(r) })))
    .sort((a, b) => (a.ts || '').localeCompare(b.ts || ''));
  box.innerHTML = items.length ? items.map(i => i.html).join('')
    : '<div class="hint">Nothing sent yet. Comment on code with ' + esc(keyLabel('Alt+K')) +
      ' to collect drafts, then send them together; or type below to chat. Messages go to<br><code>' + esc(S.meta.bridge.inbox) + '</code></div>';
  if (stick) box.scrollTop = box.scrollHeight;
  brBadge();
}

function brBadge() {
  const n = brReplyCount();
  if (brVisible()) br.seen = n;
  const unread = n - br.seen;
  const c = $('#br-tab-count');
  if (!c) return;
  c.hidden = unread <= 0;
  c.textContent = unread > 0 ? String(unread) : '';
}

function brRenderAnchor() {
  const a = br.anchor;
  brEl.anchor.hidden = !a;
  brEl.sendNow.hidden = !a;
  brEl.send.textContent = a ? 'Add Draft' : 'Send to Claude';
  brEl.send.title = a ? 'Add to the drafts; send them all together below' : 'Send to the Claude Code session';
  brEl.input.placeholder = a ? 'Comment on ' + brRef(a) + ' (saved as a draft)...' : 'Message your Claude Code session (local only, never posted to GitHub)...';
  if (!a) return;
  brEl.anchor.innerHTML = '<span class="br-anchor-ref" title="Jump to this code">' + esc(brRef(a)) + '</span>' +
    ' <button class="mini br-anchor-clear" title="Send as a chat message instead" aria-label="Detach from code">✕</button>';
}

function brSetDrafts(drafts) {
  br.drafts = drafts || [];
  if (br.editing && !br.drafts.some(d => d.id === br.editing)) br.editing = '';
  brRenderDrafts();
}

async function brRefresh() {
  try {
    const j = await api('/api/bridge');
    br.messages = j.messages || [];
    br.loose = j.loose || [];
    brRender();
    const drafts = j.drafts || [];
    // Don't repaint the draft list under an edit in progress.
    if (!br.editing && JSON.stringify(drafts) !== JSON.stringify(br.drafts)) brSetDrafts(drafts);
  } catch {
    // Transient: the next tick tries again.
  }
}

export function bridgeOn() { return !!(S.meta && S.meta.bridge); }

// Marks diff rows that carry an unsent draft. pr.js calls this after drawing
// its own markers; outside a PR review, bridge.js owns diff.js's sync hook.
export function markBridgeDiff() {
  if (!bridgeOn() || !diffview || diffview.hidden) return;
  const d = doc_();
  if (!d) return;
  const keys = new Set();
  for (const c of br.drafts) {
    if (c.path !== d.path) continue;
    for (let l = c.line; l <= (c.end_line || c.line); l++) keys.add((c.side || 'RIGHT') + ':' + l);
  }
  for (const el of diffview.querySelectorAll('[data-l], [data-old-l]')) {
    const oldOnly = el.dataset.oldL !== undefined && el.dataset.l === undefined;
    el.classList.toggle('br-draft', keys.has(oldOnly ? 'LEFT:' + el.dataset.oldL : 'RIGHT:' + el.dataset.l));
  }
}

// Sends one message to the channel now (a chat, or a single comment with Send
// Now). Used by the pane and by pr.js's comment composer.
export async function sendToBridge({ text, path, l1, l2, side, snippet }) {
  const body = { text, kind: path ? 'comment' : 'chat' };
  if (path) Object.assign(body, { path, line: l1, endLine: l2 || l1, side: side || '', snippet: snippet || '' });
  const m = await apiPostJson('/api/bridge/send', body);
  await brRefresh();
  return m;
}

// Adds a comment to the drafts. Used by the pane and by pr.js's composer.
export async function addBridgeDraft({ text, path, l1, l2, side, snippet }) {
  const j = await apiPostJson('/api/bridge/drafts', { op: 'add', text, path, line: l1, endLine: l2 || l1, side: side || '', snippet: snippet || '' });
  brSetDrafts(j.drafts);
  return j.draft;
}

async function brDraftOp(body) {
  try {
    const j = await apiPostJson('/api/bridge/drafts', body);
    br.editing = '';
    brSetDrafts(j.drafts);
  } catch (e) {
    showToast('!', e.message || 'Could not update the draft');
  }
}

async function brSendReview() {
  const n = br.drafts.length;
  if (!n || brEl.reviewSend.disabled) return;
  brEl.reviewSend.disabled = true;
  try {
    await apiPostJson('/api/bridge/review', { text: brEl.reviewNote.value.trim() });
    brEl.reviewNote.value = '';
    br.editing = '';
    brSetDrafts([]);
    await brRefresh();
    showToast('✓', 'Sent ' + n + (n === 1 ? ' draft' : ' drafts') + ' to Claude (not posted to GitHub)');
  } catch (e) {
    showToast('!', e.message || 'Could not send the drafts');
  } finally {
    brEl.reviewSend.disabled = false;
  }
}

export function openBridge(info) {
  if (!bridgeOn()) return;
  if (info && info.path) {
    const side = info.side || (info.fromDiff ? 'RIGHT' : '');
    const l1 = side === 'LEFT' ? (info.delL1 || info.l1) : info.l1;
    const l2 = side === 'LEFT' ? (info.delL2 || info.l2) : info.l2;
    br.anchor = { path: info.path, l1, l2, side, text: info.text || '' };
  }
  brRenderAnchor();
  showRightInspector('bridge');
  brEl.input.focus();
}

// Enter on the composer: a comment becomes a draft, a chat goes now.
// now=true (Send Now, or Mod+Enter) sends a comment by itself, skipping the drafts.
async function brSend(now = false) {
  const text = brEl.input.value.trim();
  if (!text || brEl.send.disabled) return;
  brEl.send.disabled = true;
  const a = br.anchor;
  const msg = { text, path: a?.path, l1: a?.l1, l2: a?.l2, side: a?.side, snippet: a?.text };
  try {
    if (a && !now) await addBridgeDraft(msg);
    else await sendToBridge(msg);
    brEl.input.value = '';
    br.anchor = null;
    brRenderAnchor();
  } catch (e) {
    showToast('!', e.message || 'Could not send');
  } finally {
    brEl.send.disabled = false;
  }
}

export function initBridge() {
  if (!bridgeOn()) return;
  brEl.pane = $('#pane-right-bridge');
  brEl.msgs = $('#br-msgs');
  brEl.anchor = $('#br-anchor');
  brEl.input = $('#br-input');
  brEl.send = $('#br-send');
  brEl.sendNow = $('#br-send-now');
  brEl.drafts = $('#br-drafts');
  brEl.draftList = $('#br-drafts-list');
  brEl.reviewNote = $('#br-review-note');
  brEl.reviewSend = $('#br-review-send');
  if (!brEl.pane) return;

  $('#tab-bridge').hidden = false;
  $('#br-channel').textContent = 'Claude · ' + S.meta.bridge.channel;
  $('#br-channel').title = 'inbox: ' + S.meta.bridge.inbox + '\noutbox: ' + S.meta.bridge.outbox;

  setBridgeHandler(openBridge);
  if (!S.meta.pr) setPRSyncHandler(markBridgeDiff);
  if (!SEL_MENU_ITEMS.some(item => item.sel === 'bridge')) {
    SEL_MENU_ITEMS.unshift({ sel: 'bridge', label: 'Comment for Claude', keys: 'Alt+K' });
  }
  const sel = $('#footer-sel');
  if (sel && !sel.querySelector('[data-sel="bridge"]')) {
    const btn = document.createElement('button');
    btn.className = 'footer-btn';
    btn.dataset.sel = 'bridge';
    btn.title = withKeys('Comment on this selection for your Claude Code session ({Alt+K}); never posted to GitHub');
    btn.innerHTML = '<span class="footer-btn-label">Claude</span><kbd class="footer-kbd">' + esc(keyLabel('Alt+K')) + '</kbd>';
    sel.prepend(btn);
  }

  brEl.send.addEventListener('click', () => brSend(false));
  brEl.sendNow.addEventListener('click', () => brSend(true));
  brEl.reviewSend.addEventListener('click', brSendReview);
  for (const ta of [brEl.input, brEl.reviewNote]) {
    ta.addEventListener('keydown', e => e.stopPropagation()); // typing here must not trigger editor shortcuts
  }
  brEl.input.addEventListener('keydown', e => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); brSend(e.metaKey || e.ctrlKey); }
    else if (e.key === 'Escape') brEl.input.blur();
  });
  brEl.anchor.addEventListener('click', e => {
    if (e.target.closest('.br-anchor-clear')) { br.anchor = null; brRenderAnchor(); return; }
    if (br.anchor) openFile(br.anchor.path, { line: br.anchor.l1 || 1 });
  });
  const jump = e => {
    const loc = e.target.closest('.br-loc');
    if (loc) openFile(loc.dataset.path, { line: +loc.dataset.line || 1 });
    return !!loc;
  };
  brEl.msgs.addEventListener('click', jump);
  brEl.draftList.addEventListener('keydown', e => e.stopPropagation());
  brEl.draftList.addEventListener('click', e => {
    if (jump(e)) return;
    const id = e.target.closest('.br-draft-item')?.dataset.id;
    if (!id) return;
    if (e.target.closest('.br-draft-del')) brDraftOp({ op: 'delete', id });
    else if (e.target.closest('.br-draft-edit-btn')) { br.editing = id; brRenderDrafts(); brEl.draftList.querySelector('.br-draft-edit')?.focus(); }
    else if (e.target.closest('.br-draft-cancel')) { br.editing = ''; brRenderDrafts(); }
    else if (e.target.closest('.br-draft-save')) {
      const text = brEl.draftList.querySelector('.br-draft-edit')?.value.trim();
      if (text) brDraftOp({ op: 'update', id, text });
    }
  });
  on('bridge:shown', brBadge);

  brRenderAnchor();
  brRefresh();
  setInterval(() => { if (!document.hidden) brRefresh(); }, 1500);
}
