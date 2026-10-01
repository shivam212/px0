// web/src/bridge.js
// The Claude pane, active only under `px0 -bridge <name>` (S.meta.bridge, see
// bridge.go). Comments and chat messages are appended to the channel's
// inbox.jsonl, which an already running Claude Code session watches; its
// replies come back through outbox.jsonl and are shown under the message they
// answer. Everything here stays on this machine: nothing is posted to GitHub.
import { $, S, esc, api, apiPostJson, keyLabel, withKeys } from './state.js';
import { on } from './bus.js';
import { showToast } from './ui.js';
import { openFile } from './tabs.js';
import { setBridgeHandler, SEL_MENU_ITEMS } from './selbar.js';
import { showRightInspector } from './inspector.js';
import { thrMd } from './thread.js';

const br = {
  messages: [],  // sent messages, each with its replies
  loose: [],     // replies with no matching reply_to
  anchor: null,  // {path, l1, l2, side, text} the next message is about, or null for chat
  seen: 0,       // replies counted when the pane was last on screen
  sig: '',       // last rendered snapshot, to skip identical repaints
};
const brEl = {};

const brRef = a => a.path + ':' + (a.l1 === a.l2 || !a.l2 ? a.l1 : a.l1 + '-' + a.l2) + (a.side === 'LEFT' ? ' (base)' : '');
const brReplyCount = () => br.messages.reduce((n, m) => n + m.replies.length, 0) + br.loose.length;
const brVisible = () => !!brEl.pane?.classList.contains('active') && !document.body.classList.contains('right-hidden');

function brTime(ts) {
  try { return new Date(ts).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }); } catch { return ''; }
}

function brReplyHtml(r) {
  return '<div class="thr-msg thr-agent"><div class="thr-who">Claude · ' + esc(brTime(r.ts)) + '</div>' +
    '<div class="thr-body thr-reply">' + thrMd(r.text) + '</div></div>';
}

function brMessageHtml(m) {
  const loc = m.path
    ? '<button class="thr-file br-loc" data-path="' + esc(m.path) + '" data-line="' + (m.line || 1) + '" title="Jump to this code">' +
      esc(brRef({ path: m.path, l1: m.line, l2: m.end_line || m.line, side: m.side })) + '</button>'
    : '';
  const waiting = m.replies.length ? '' : '<div class="thr-who br-waiting">waiting for a reply…</div>';
  return '<div class="br-item"><div class="thr-msg thr-user"><div class="thr-who">You · ' + esc(m.kind) + ' · ' + esc(brTime(m.ts)) + '</div>' +
    loc + '<div class="thr-body">' + thrMd(m.text) + '</div></div>' +
    m.replies.map(brReplyHtml).join('') + waiting + '</div>';
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
      ' (or Send to Claude in a PR review), or type below. Messages go to<br><code>' + esc(S.meta.bridge.inbox) + '</code></div>';
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
  if (!a) return;
  brEl.anchor.innerHTML = '<span class="br-anchor-ref" title="Jump to this code">' + esc(brRef(a)) + '</span>' +
    ' <button class="mini br-anchor-clear" title="Send as a chat message instead" aria-label="Detach from code">✕</button>';
}

async function brRefresh() {
  try {
    const j = await api('/api/bridge');
    br.messages = j.messages || [];
    br.loose = j.loose || [];
    brRender();
  } catch {
    // Transient: the next tick tries again.
  }
}

// Sends one message to the channel. Used by the pane and by pr.js's comment
// composer. Resolves to the stored message, or throws.
export async function sendToBridge({ text, path, l1, l2, side, snippet }) {
  const body = { text, kind: path ? 'comment' : 'chat' };
  if (path) Object.assign(body, { path, line: l1, endLine: l2 || l1, side: side || '', snippet: snippet || '' });
  const m = await apiPostJson('/api/bridge/send', body);
  await brRefresh();
  return m;
}

export function bridgeOn() { return !!(S.meta && S.meta.bridge); }

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

async function brSend() {
  const text = brEl.input.value.trim();
  if (!text || brEl.send.disabled) return;
  brEl.send.disabled = true;
  const a = br.anchor;
  try {
    await sendToBridge({ text, path: a?.path, l1: a?.l1, l2: a?.l2, side: a?.side, snippet: a?.text });
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
  if (!brEl.pane) return;

  $('#tab-bridge').hidden = false;
  $('#br-channel').textContent = 'Claude · ' + S.meta.bridge.channel;
  $('#br-channel').title = 'inbox: ' + S.meta.bridge.inbox + '\noutbox: ' + S.meta.bridge.outbox;

  setBridgeHandler(openBridge);
  if (!SEL_MENU_ITEMS.some(item => item.sel === 'bridge')) {
    SEL_MENU_ITEMS.unshift({ sel: 'bridge', label: 'Send to Claude', keys: 'Alt+K' });
  }
  const sel = $('#footer-sel');
  if (sel && !sel.querySelector('[data-sel="bridge"]')) {
    const btn = document.createElement('button');
    btn.className = 'footer-btn';
    btn.dataset.sel = 'bridge';
    btn.title = withKeys('Send this selection to your Claude Code session ({Alt+K}); never posted to GitHub');
    btn.innerHTML = '<span class="footer-btn-label">Send to Claude</span><kbd class="footer-kbd">' + esc(keyLabel('Alt+K')) + '</kbd>';
    sel.prepend(btn);
  }

  brEl.send.addEventListener('click', brSend);
  brEl.input.addEventListener('keydown', e => {
    e.stopPropagation(); // typing here must not trigger editor shortcuts
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); brSend(); }
    else if (e.key === 'Escape') brEl.input.blur();
  });
  brEl.anchor.addEventListener('click', e => {
    if (e.target.closest('.br-anchor-clear')) { br.anchor = null; brRenderAnchor(); return; }
    if (br.anchor) openFile(br.anchor.path, { line: br.anchor.l1 || 1 });
  });
  brEl.msgs.addEventListener('click', e => {
    const loc = e.target.closest('.br-loc');
    if (loc) openFile(loc.dataset.path, { line: +loc.dataset.line || 1 });
  });
  on('bridge:shown', brBadge);

  brRefresh();
  setInterval(() => { if (!document.hidden) brRefresh(); }, 1500);
}
