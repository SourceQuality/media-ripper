(() => {
  const $ = s => document.querySelector(s);
  const next = () => {
    const n = new URLSearchParams(location.search).get('next') || '/';
    return n.startsWith('/') && !n.startsWith('//') ? n : '/'; // only this site
  };
  let setup = false;
  fetch('/api/auth/status').then(r => r.json()).then(st => {
    if (!st.enabled || st.signed_in) { location.replace(next()); return; }
    setup = st.setup;
    if (setup) {
      $('#heading').textContent = 'Create your account';
      $('#intro').textContent = 'Media Ripper has no account yet. Choose the user name and password for this ripper; you will use them to sign in from now on.';
      $('#password').autocomplete = 'new-password';
      $('#confirm-row').classList.remove('hidden');
      $('#submit').textContent = 'Create account';
    }
  });
  $('#login').onsubmit = e => {
    e.preventDefault();
    const msg = $('#message');
    if (setup && $('#password').value !== $('#confirm').value) { msg.textContent = 'The passwords do not match'; return; }
    $('#submit').disabled = true;
    msg.textContent = setup ? 'Creating…' : 'Signing in…';
    fetch(setup ? '/api/auth/setup' : '/api/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: $('#username').value, password: $('#password').value }) })
      .then(async r => { const j = await r.json(); if (!r.ok) throw new Error(j.error || r.statusText); })
      .then(() => location.replace(next()))
      .catch(err => { msg.textContent = err.message; $('#submit').disabled = false; });
  };
})();
