// The theme before first paint. Blocking, tiny, no DOM dependency:
// reads the dl-theme key and sets data-theme on <html>. Without a stored
// choice the attribute stays unset and app.css decides through
// prefers-color-scheme.
(function () {
  var saved = null;
  try { saved = localStorage.getItem('dl-theme'); } catch (e) { /* storage blocked */ }
  if (saved === 'dark' || saved === 'light') {
    document.documentElement.setAttribute('data-theme', saved);
  }
})();
