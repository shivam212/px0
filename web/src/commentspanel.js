// web/src/commentspanel.js
// The bottom Comments panel's frame: showing it, collapsing it from its head,
// and the drag-to-resize handle. Shared by pr.js (GitHub comments) and
// bridge.js (the Claude conversation), which each fill their own part of it.
import { $ } from './state.js';
import { layout, render } from './renderer.js';

let wired = false;

export function initCommentsPanel() {
  const panel = $('#pr-comments-panel');
  if (!panel) return;
  panel.hidden = false;
  if (wired) return;
  wired = true;
  panel.classList.add('collapsed');

  const toggle = () => {
    panel.classList.toggle('collapsed');
    layout(); render();
  };
  $('#pr-comments-collapse')?.addEventListener('click', e => {
    e.stopPropagation();
    toggle();
  });
  $('.pr-comments-panel-head')?.addEventListener('click', e => {
    if (e.target.closest('#pr-comments-collapse')) return;
    toggle();
  });

  const rz = $('#pr-comments-resizer');
  if (!rz) return;
  let dragging = false;
  rz.addEventListener('mousedown', e => {
    dragging = true;
    rz.classList.add('drag');
    panel.classList.remove('collapsed');
    e.preventDefault();
  });
  addEventListener('mousemove', e => {
    if (!dragging) return;
    const bottom = panel.getBoundingClientRect().bottom;
    const h = Math.max(80, Math.min(window.innerHeight * 0.8, bottom - e.clientY));
    panel.style.height = h + 'px';
    layout(); render();
  });
  addEventListener('mouseup', () => {
    if (!dragging) return;
    dragging = false;
    rz.classList.remove('drag');
    layout(); render();
  });
}

export function expandCommentsPanel() {
  const panel = $('#pr-comments-panel');
  if (!panel || !panel.classList.contains('collapsed')) return;
  panel.classList.remove('collapsed');
  layout(); render();
}
