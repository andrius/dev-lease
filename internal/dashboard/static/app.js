// The theme toggle and the small interactivity: dialogs, flash and the
// 15 s fragment refresh. Pages work without this file; it only adds
// convenience.

function resolvedTheme() {
  var attr = document.documentElement.getAttribute('data-theme');
  if (attr === 'dark' || attr === 'light') return attr;
  return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function storeTheme(t) {
  document.documentElement.setAttribute('data-theme', t);
  try { localStorage.setItem('dl-theme', t); } catch (e) { /* storage blocked */ }
}

document.addEventListener('DOMContentLoaded', function () {
  var toggle = document.getElementById('theme-toggle');
  if (toggle) {
    toggle.addEventListener('click', function () {
      storeTheme(resolvedTheme() === 'dark' ? 'light' : 'dark');
    });
  }

  // The action links open their <dialog> in place; without JavaScript
  // they navigate to the same dialog rendered as a page. Clicks are
  // bound by delegation on document, so links inside a freshly swapped
  // fragment keep working.
  document.addEventListener('click', function (ev) {
    var link = ev.target.closest('[data-dialog]');
    if (!link) return;
    var dialog = document.getElementById(link.getAttribute('data-dialog'));
    if (!dialog) return;
    ev.preventDefault();
    if (typeof dialog.showModal === 'function') {
      dialog.showModal();
    } else {
      dialog.setAttribute('open', '');
    }
  });
  document.addEventListener('click', function (ev) {
    var link = ev.target.closest('dialog .dialog-cancel');
    if (!link) return;
    var dialog = link.closest('dialog');
    if (!dialog) return;
    // Closed in place: the link's own target (the page without the
    // dialog) would reload it and drop what the user chose.
    ev.preventDefault();
    if (typeof dialog.close === 'function') dialog.close();
    else dialog.removeAttribute('open');
  });

  // The remove button works only while the typed name equals the stack
  // name. The markup stays enabled for pages without JavaScript: the
  // server-side check remains the real protection. The input event is
  // delegated too, so a re-rendered dialog keeps the gating.
  document.querySelectorAll('.rm-confirm').forEach(function (form) {
    var button = form.querySelector('button[type="submit"]');
    if (button) button.disabled = true;
  });
  document.addEventListener('input', function (ev) {
    var input = ev.target.closest('.rm-confirm input[name="confirm"]');
    if (!input) return;
    var button = input.closest('form').querySelector('button[type="submit"]');
    if (button) button.disabled = input.value !== input.getAttribute('placeholder');
  });

  // The flash (an action's result) clears itself.
  var flash = document.querySelector('.flash');
  if (flash) setTimeout(function () { flash.remove(); }, 6000);

  // Poll the fragments so a change on the control socket shows up without
  // a manual reload. Pages have exactly one refresh target each. The stack
  // page's fragment is a set of read-only sections (containers, logs,
  // volumes, the lease card), swapped by id, so the Keep form, the log
  // controls and the dialogs keep whatever the user chose. The all-stacks
  // fragment is one block and replaces the target wholesale. The target is
  // looked up again on every tick: the previous node was replaced with its
  // own outerHTML, so the old reference is detached and useless.
  var busy = false;
  setInterval(function () {
    if (busy || document.hidden) return;
    var target = document.querySelector('[data-refresh-url]');
    if (!target) return;
    busy = true;
    fetch(target.getAttribute('data-refresh-url'), { credentials: 'same-origin' })
      .then(function (resp) {
        // A redirect (the session died while the page sat open), 401,
        // 403 or 404 means the page is stale: reload the whole document
        // instead of swapping fragment markup in place. Any other
        // failure (a slow daemon) keeps the page and tries next tick.
        if (resp.redirected || resp.status === 401 || resp.status === 403 || resp.status === 404) { location.reload(); return null; }
        if (!resp.ok) return null;
        return resp.text();
      })
      .then(function (html) {
        if (html === null) return;
        var tmp = document.createElement('template');
        tmp.innerHTML = html;
        var sections = tmp.content.querySelectorAll('[id]');
        if (sections.length > 0) {
          sections.forEach(function (node) {
            var live = document.getElementById(node.id);
            if (live) live.outerHTML = node.outerHTML;
          });
          return;
        }
        var cur = document.querySelector('[data-refresh-url]');
        if (cur) cur.outerHTML = html;
      })
      .catch(function () { /* keep the old page; try again next tick */ })
      .finally(function () { busy = false; });
  }, 15000);
});
