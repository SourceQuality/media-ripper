// Loaded before each page's script: an API answer of 401 means the session
// ended, so go to the sign-in page and come back afterwards. Also puts a
// Sign out button in the header when signed in.
(() => {
  const orig = window.fetch.bind(window);
  window.fetch = (...args) => orig(...args).then(r => {
    if (r.status === 401) location.href = '/login.html?next=' + encodeURIComponent(location.pathname + location.hash);
    return r;
  });
  orig('/api/auth/status').then(r => r.json()).then(st => {
    if (!st.enabled || !st.signed_in || st.user === 'api') return;
    const b = document.createElement('button');
    b.textContent = 'Sign out';
    b.title = 'Signed in as ' + st.user;
    b.onclick = () => orig('/api/auth/logout', { method: 'POST' }).then(() => location.href = '/login.html');
    const theme = document.getElementById('theme');
    if (theme) theme.before(b);
  }).catch(() => {});
})();
