(() => {
  const $ = (s, el = document) => el.querySelector(s);

  // Every config key, grouped as in config.yaml. Types:
  // text, secret, number, float, bool, duration, mode, select, lines (array), map (key = value lines)
  const SCHEMA = [
    { title: 'Drives', fields: [
      { key: 'drives', label: 'Drives', type: 'lines', hint: 'empty = all /dev/sr*' },
      { key: 'poll_interval', label: 'Poll interval', type: 'duration' },
      { key: 'workspace', label: 'Workspace', type: 'text' },
    ]},
    { title: 'Output', fields: [
      { key: 'output.path', label: 'Library path', type: 'text' },
      { key: 'output.movies_subdir', label: 'Movies folder', type: 'text' },
      { key: 'output.tv_subdir', label: 'TV folder', type: 'text' },
      { key: 'output.unidentified_subdir', label: 'Unidentified folder', type: 'text' },
      { key: 'output.movie_template', label: 'Movie template', type: 'text', wide: true },
      { key: 'output.tv_template', label: 'TV template', type: 'text', wide: true },
      { key: 'output.unidentified_template', label: 'Unidentified template', type: 'text', wide: true },
      { key: 'output.overwrite', label: 'Overwrite existing files', type: 'bool' },
      { key: 'output.dir_mode', label: 'Directory mode', type: 'mode' },
      { key: 'output.file_mode', label: 'File mode', type: 'mode' },
      { key: 'output.keep_workspace_on_error', label: 'Keep workspace on error', type: 'bool' },
      { key: 'output.resume', label: 'Resume interrupted discs', type: 'bool' },
      { key: 'output.backup', label: 'Full-disc backup', type: 'select', options: ['off', 'also', 'only'], hint: 'also: titles and a decrypted copy of the disc; only: just the copy' },
      { key: 'output.backup_path', label: 'Backup folder', type: 'text', wide: true, hint: 'Empty: _backups under the output path' },
      { key: 'output.resume_max_age', label: 'Keep partial rips for', type: 'duration' },
    ]},
    { title: 'MakeMKV', fields: [
      { key: 'makemkv.binary', label: 'Binary', type: 'text' },
      { key: 'makemkv.key', label: 'License key', type: 'secret' },
      { key: 'makemkv.min_length', label: 'Minimum title length (s)', type: 'number' },
      { key: 'makemkv.timeout', label: 'Rip timeout', type: 'duration' },
      { key: 'makemkv.scan_timeout', label: 'Scan timeout', type: 'duration' },
      { key: 'makemkv.retries', label: 'Retries', type: 'number' },
      { key: 'makemkv.extra_args', label: 'Extra arguments', type: 'lines' },
      { key: 'makemkv.settings_dir', label: 'Settings directory', type: 'text', hint: 'empty = ~/.MakeMKV' },
      { key: 'makemkv.write_settings', label: 'Write settings.conf', type: 'bool' },
    ]},
    { title: 'Metadata', fields: [
      { key: 'metadata.provider', label: 'Provider', type: 'select', options: ['auto', 'tmdb', 'arr', 'none'] },
      { key: 'metadata.tmdb_api_key', label: 'TMDB API key', type: 'secret' },
      { key: 'metadata.language', label: 'Language', type: 'text' },
      { key: 'metadata.timeout', label: 'Lookup timeout', type: 'duration' },
      { key: 'metadata.label_overrides', label: 'Label overrides', type: 'map', hint: 'LABEL = Title to search', wide: true },
    ]},
    { title: 'TheDiscDB', fields: [
      { key: 'metadata.thediscdb.enabled', label: 'Use TheDiscDB disc maps', type: 'bool', hint: 'Episode numbers, names and extras for catalogued discs' },
      { key: 'metadata.thediscdb.repo', label: 'Catalogue repository', type: 'text', hint: 'GitHub owner/name' },
      { key: 'metadata.thediscdb.api', label: 'Content hash lookup', type: 'text', wide: true, hint: 'Identifies discs with useless labels; needs thediscdb.com reachable' },
      { key: 'metadata.thediscdb.disc_folder', label: 'Keep disc folder for contributing', type: 'select', options: ['unmatched', 'always', 'off'], hint: 'After the rip, reads the disc\'s small metadata files (a few MB) so you can add it on thediscdb.com without the disc' },
    ]},
    { title: 'Title card OCR', fields: [
      { key: 'metadata.ocr.enabled', label: 'Enabled', type: 'bool' },
      { key: 'metadata.ocr.tesseract', label: 'Tesseract binary', type: 'text' },
      { key: 'metadata.ocr.languages', label: 'OCR languages', type: 'text', hint: 'tesseract codes, e.g. eng+deu' },
      { key: 'metadata.ocr.head_minutes', label: 'Minutes from start', type: 'number' },
      { key: 'metadata.ocr.tail_minutes', label: 'Minutes from end', type: 'number' },
      { key: 'metadata.ocr.frames_per_minute', label: 'Frames per minute', type: 'number' },
      { key: 'metadata.ocr.max_candidates', label: 'Candidates to try', type: 'number' },
      { key: 'metadata.ocr.timeout', label: 'Timeout', type: 'duration' },
    ]},
    { title: 'Radarr', fields: [
      { key: 'arr.radarr.enabled', label: 'Enabled', type: 'bool' },
      { key: 'arr.radarr.url', label: 'URL', type: 'text' },
      { key: 'arr.radarr.api_key', label: 'API key', type: 'secret' },
      { key: 'arr.radarr.root_folder', label: 'Root folder', type: 'text' },
      { key: 'arr.radarr.quality_profile', label: 'Quality profile', type: 'text', hint: 'empty = first' },
      { key: 'arr.radarr.add_missing', label: 'Add missing movies', type: 'bool' },
      { key: 'arr.radarr.monitored', label: 'Add as monitored', type: 'bool' },
      { key: 'arr.radarr.import_mode', label: 'Import mode', type: 'select', options: ['move', 'copy'], hint: 'move: staging is emptied once Radarr has the file; copy: the staged file stays' },
      { key: 'arr.radarr.path_map', label: 'Path map', type: 'map', hint: 'local path = path as Radarr sees it', wide: true },
    ]},
    { title: 'Sonarr', fields: [
      { key: 'arr.sonarr.enabled', label: 'Enabled', type: 'bool' },
      { key: 'arr.sonarr.url', label: 'URL', type: 'text' },
      { key: 'arr.sonarr.api_key', label: 'API key', type: 'secret' },
      { key: 'arr.sonarr.root_folder', label: 'Root folder', type: 'text' },
      { key: 'arr.sonarr.quality_profile', label: 'Quality profile', type: 'text', hint: 'empty = first' },
      { key: 'arr.sonarr.add_missing', label: 'Add missing series', type: 'bool' },
      { key: 'arr.sonarr.monitored', label: 'Add as monitored', type: 'bool' },
      { key: 'arr.sonarr.import_mode', label: 'Import mode', type: 'select', options: ['move', 'copy'], hint: 'move: staging is emptied once Sonarr has the file; copy: the staged file stays' },
      { key: 'arr.sonarr.path_map', label: 'Path map', type: 'map', hint: 'local path = path as Sonarr sees it', wide: true },
    ]},
    { title: 'Handoff', fields: [
      { key: 'arr.import_policy', label: 'Import without review', type: 'select', options: ['confident', 'verified', 'always'], hint: 'confident: verified discs and runtime-matched movies; others wait in Review' },
      { key: 'arr.staging_subdir', label: 'Staging folder', type: 'text' },
      { key: 'arr.import_timeout', label: 'Import timeout', type: 'duration' },
    ]},
    { title: 'Selection', fields: [
      { key: 'selection.languages', label: 'Keep languages', type: 'lines', hint: 'empty = all' },
      { key: 'selection.movie_runtime_tolerance', label: 'Movie runtime tolerance', type: 'duration' },
      { key: 'selection.tv_episode_tolerance', label: 'Episode length tolerance', type: 'float' },
      { key: 'selection.min_movie_duration', label: 'Minimum movie length', type: 'duration' },
      { key: 'selection.min_episode_duration', label: 'Minimum episode length', type: 'duration' },
      { key: 'selection.unidentified_strategy', label: 'Unidentified disc', type: 'select', options: ['longest', 'all', 'skip'] },
      { key: 'selection.allow_double_episodes', label: 'Allow double episodes', type: 'bool' },
    ]},
    { title: 'Post-processing', fields: [
      { key: 'postprocess.mode', label: 'Mode', type: 'select', options: ['none', 'remux', 'custom'] },
      { key: 'postprocess.tool', label: 'Tool', type: 'select', options: ['auto', 'mkvmerge', 'ffmpeg'] },
      { key: 'postprocess.set_title', label: 'Set file title', type: 'bool' },
      { key: 'postprocess.custom_command', label: 'Custom command', type: 'lines', hint: 'one argument per line; {input} {output} {title}', wide: true },
      { key: 'postprocess.custom_extension', label: 'Custom output extension', type: 'text' },
      { key: 'postprocess.timeout', label: 'Timeout', type: 'duration' },
    ]},
    { title: 'Eject', fields: [
      { key: 'eject.on_success', label: 'Eject after success', type: 'bool' },
      { key: 'eject.after_rip', label: 'Eject as soon as ripping finishes', type: 'bool' },
      { key: 'eject.on_failure', label: 'Eject after failure', type: 'bool' },
      { key: 'eject.close_tray_on_start', label: 'Close tray on start', type: 'bool' },
      { key: 'eject.rerip_same_disc', label: 'Rip discs again', type: 'bool' },
    ]},
    { title: 'Web', fields: [
      { key: 'web.enabled', label: 'Enabled', type: 'bool' },
      { key: 'web.listen', label: 'Listen address', type: 'text' },
      { key: 'web.tls', label: 'HTTPS', type: 'select', options: ['auto', 'files', 'off'], hint: 'auto: a certificate made on this machine; files: your own' },
      { key: 'web.tls_info', label: 'Certificate in use', type: 'tls-info', wide: true },
      { key: 'web.tls_hosts', label: 'Extra names for the certificate', type: 'lines', hint: 'One per line, e.g. a Tailscale or DNS name; host name and addresses are included' },
      { key: 'web.tls_cert', label: 'Certificate file', type: 'text', wide: true, hint: 'For files: PEM certificate (chain)' },
      { key: 'web.tls_key', label: 'Key file', type: 'text', wide: true, hint: 'For files: PEM private key' },
    ]},
    { title: 'Notifications', fields: [
      { key: 'notify.ntfy_url', label: 'ntfy URL', type: 'text', wide: true },
      { key: 'notify.ntfy_token', label: 'ntfy token', type: 'secret' },
      { key: 'notify.webhook_url', label: 'Webhook URL', type: 'text', wide: true },
    ]},
    { title: 'Discord', fields: [
      { key: 'notify.discord.application_id', label: 'Bot application ID', type: 'text', hint: 'From discord.com/developers/applications: your application\'s General Information page' },
      { key: 'discord.invite', label: 'Invite the bot', type: 'invite', hint: 'Adds the bot to a server with View Channel, Send Messages and Embed Links only. Needs the application ID' },
      { key: 'notify.discord.bot_token', label: 'Bot token', type: 'secret', hint: 'From the application\'s Bot page in the Discord developer portal' },
      { key: 'notify.discord.channel_id', label: 'Channel ID', type: 'text', hint: 'Developer mode on, then right-click the channel → Copy Channel ID' },
      { key: 'notify.discord.webhook_url', label: 'Or: channel webhook URL', type: 'secret', wide: true, hint: 'Channel settings → Integrations → Webhooks. Used when no bot token and channel are set' },
      { key: 'notify.discord.buttons', label: 'Buttons on bot messages', type: 'bool', hint: 'Approve / Discard a review, Cancel a rip, Eject after a failure' },
      { key: 'notify.discord.allowed_users', label: 'Who may press them', type: 'lines', hint: 'Discord user ids, one per line; empty = anyone who can see the channel' },
      { key: 'discord.test', label: 'Check it', type: 'action', action: 'test-notify', text: 'Send test message', hint: 'Uses the saved settings: save first' },
    ]},
    { title: 'Sign-in', fields: [
      { key: 'auth.enabled', label: 'Require sign-in', type: 'bool', hint: 'Only turn this off on a network nobody else can reach' },
      { key: 'auth.username', label: 'User name', type: 'text' },
      { key: 'auth.password', label: 'Change password', type: 'password-change', wide: true },
      { key: 'auth.token', label: 'API token', type: 'api-token', wide: true, hint: 'For Prometheus (/metrics) and scripts: Authorization: Bearer <token>. Shown once.' },
    ]},
    { title: 'Updates', fields: [
      { key: 'updates.check', label: 'Tell me about new releases', type: 'bool' },
      { key: 'updates.repo', label: 'Repository', type: 'text', hint: 'GitHub owner/name' },
      { key: 'updates.token', label: 'GitHub token', type: 'secret', hint: 'Read access; needed while the repository is private' },
    ]},
    { title: 'Logging', fields: [
      { key: 'log.level', label: 'Level', type: 'select', options: ['debug', 'info', 'warn', 'error'] },
      { key: 'log.format', label: 'Format', type: 'select', options: ['text', 'json'] },
    ]},
  ];

  // Same link as notify.DiscordInviteURL: bot scope, View Channel +
  // Send Messages + Embed Links (19456).
  // No link until there is an application to invite.
  const inviteURL = appID => (appID || '').trim() ? `https://discord.com/oauth2/authorize?client_id=${encodeURIComponent(appID.trim())}&scope=bot&permissions=19456` : '';
  const setInvite = (a, appID) => {
    const url = inviteURL(appID);
    if (url) a.href = url; else a.removeAttribute('href');
    a.classList.toggle('disabled', !url);
    a.setAttribute('aria-disabled', String(!url));
  };
  const NON_DATA = new Set(['invite', 'action', 'password-change', 'api-token', 'tls-info']);

  // What most people set; everything else is behind "Show advanced
  // settings". A section with no basic field only appears in advanced.
  const BASIC = new Set([
    'drives', 'output.path', 'output.backup', 'makemkv.key',
    'metadata.tmdb_api_key', 'metadata.thediscdb.enabled',
    'arr.radarr.enabled', 'arr.radarr.url', 'arr.radarr.api_key', 'arr.radarr.root_folder',
    'arr.sonarr.enabled', 'arr.sonarr.url', 'arr.sonarr.api_key', 'arr.sonarr.root_folder',
    'arr.import_policy',
    'notify.discord.application_id', 'discord.invite', 'notify.discord.bot_token', 'notify.discord.channel_id', 'discord.test',
    'auth.username', 'auth.password', 'auth.token',
  ]);
  const slug = t => t.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
  const hasBasic = sec => sec.fields.some(f => BASIC.has(f.key));
  let showAdvanced = false;
  try { showAdvanced = localStorage.getItem('mr-settings-advanced') === '1'; } catch (e) { /* private mode */ }

  const get = (obj, key) => key.split('.').reduce((o, k) => (o == null ? undefined : o[k]), obj);
  const set = (obj, key, val) => { const ks = key.split('.'); let o = obj; for (const k of ks.slice(0, -1)) { o[k] = o[k] || {}; o = o[k]; } o[ks.at(-1)] = val; };
  const id = key => 'f-' + key.replace(/\./g, '-');

  let state = null;       // last server response
  const clearSecrets = new Set();

  const control = (f, value, locked) => {
    const dis = locked ? ' disabled' : '';
    switch (f.type) {
      case 'bool': return `<input type="checkbox" id="${id(f.key)}"${value ? ' checked' : ''}${dis}>`;
      case 'select': return `<select id="${id(f.key)}"${dis}>${f.options.map(o => `<option${o === value ? ' selected' : ''}>${o}</option>`).join('')}</select>`;
      case 'lines': return `<textarea id="${id(f.key)}" rows="${Math.max(2, (value || []).length + 1)}"${dis}>${(value || []).join('\n')}</textarea>`;
      case 'map': { const lines = Object.entries(value || {}).map(([k, v]) => `${k} = ${v}`); return `<textarea id="${id(f.key)}" rows="${Math.max(2, lines.length + 1)}"${dis}>${lines.join('\n')}</textarea>`; }
      case 'secret': { const isSet = state.secrets[f.key]; return `<span class="secret"><input type="password" id="${id(f.key)}" placeholder="${isSet ? '••••••••' : ''}"${dis}>${isSet && !locked ? `<button type="button" class="clear" data-key="${f.key}">Clear</button>` : ''}</span>`; }
      case 'invite': return `<a class="btn" id="${id(f.key)}" target="_blank" rel="noopener">Add to Discord</a>`;
      case 'action': return `<span class="row"><button type="button" id="${id(f.key)}" data-action="${f.action}">${f.text}</button><span class="hint" id="${id(f.key)}-status"></span></span>`;
      case 'tls-info': return state.tls ? `<span class="small">${state.tls.mode === 'auto' ? 'Made on this machine' : 'From your files'}, valid until ${new Date(state.tls.expires).toLocaleDateString()}<br>SHA-256 <code>${state.tls.fingerprint}</code><br><span class="muted">Your browser asks once to trust it; the fingerprint it shows should match this.</span></span>` : '<span class="muted">HTTPS is off</span>';
      case 'password-change': return `<span class="row"><input type="password" id="pw-current" placeholder="Current password" autocomplete="current-password">
        <input type="password" id="pw-new" placeholder="New password (8+ characters)" autocomplete="new-password"><button type="button" id="pw-save">Change</button><span class="hint" id="pw-status"></span></span>`;
      case 'api-token': return `<span class="row"><button type="button" id="tok-new">${state.secrets['auth.api_token_hash'] ? 'Replace token' : 'Create token'}</button>
        ${state.secrets['auth.api_token_hash'] ? '<button type="button" id="tok-revoke">Revoke</button>' : ''}<code id="tok-value"></code></span>`;
      case 'number': return `<input type="number" step="1" id="${id(f.key)}" value="${value ?? ''}"${dis}>`;
      case 'float': return `<input type="number" step="0.01" min="0" max="1" id="${id(f.key)}" value="${value ?? ''}"${dis}>`;
      default: return `<input type="text" id="${id(f.key)}" value="${String(value ?? '').replace(/"/g, '&quot;')}"${dis}>`;
    }
  };

  const render = () => {
    const cfg = state.config;
    const env = new Set(state.env);
    $('#settings').innerHTML = SCHEMA.map(sec => `<section class="card${hasBasic(sec) ? '' : ' adv'}" id="sec-${slug(sec.title)}"><h2>${sec.title}</h2><div class="grid">${sec.fields.map(f => {
      const locked = env.has(f.key);
      return `<label class="field${f.wide ? ' wide' : ''}${f.type === 'bool' ? ' check' : ''}${BASIC.has(f.key) ? '' : ' adv'}" for="${id(f.key)}">
        <span class="name">${f.label}${locked ? ' <span class="tag">env</span>' : ''}${state.restart_required.includes(f.key.split('.')[0]) || state.restart_required.includes(f.key) ? ' <span class="tag warn">restart</span>' : ''}</span>
        ${control(f, get(cfg, f.key), locked)}
        ${f.hint ? `<span class="hint">${f.hint}</span>` : ''}
      </label>`;
    }).join('')}</div></section>`).join('');
    renderNav();
    const appInput = document.getElementById(id('notify.discord.application_id'));
    const invite = document.getElementById(id('discord.invite'));
    if (appInput && invite) {
      setInvite(invite, appInput.value);
      appInput.oninput = () => setInvite(invite, appInput.value);
    }
    document.querySelectorAll('button[data-action="test-notify"]').forEach(b => b.onclick = () => {
      const out = document.getElementById(b.id + '-status');
      b.disabled = true; out.textContent = 'Sending…';
      fetch('/api/notify/test', { method: 'POST' })
        .then(async r => { const j = await r.json(); if (!r.ok) throw new Error(j.error || r.statusText); })
        .then(() => { out.textContent = 'Sent'; })
        .catch(e => { out.textContent = e.message; })
        .finally(() => { b.disabled = false; });
    });
    const post = (url, body, method = 'POST') => fetch(url, { method, headers: { 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined })
      .then(async r => { const j = await r.json(); if (!r.ok) throw new Error(j.error || r.statusText); return j; });
    const pwSave = document.getElementById('pw-save');
    if (pwSave) pwSave.onclick = () => {
      const out = document.getElementById('pw-status');
      post('/api/auth/password', { current: document.getElementById('pw-current').value, password: document.getElementById('pw-new').value })
        .then(() => { out.textContent = 'Changed; other browsers are signed out'; document.getElementById('pw-current').value = ''; document.getElementById('pw-new').value = ''; })
        .catch(e => { out.textContent = e.message; });
    };
    const tokNew = document.getElementById('tok-new');
    if (tokNew) tokNew.onclick = () => {
      if (state.secrets['auth.api_token_hash'] && !confirm('Replace the token? Anything using the old one stops working.')) return;
      post('/api/auth/token').then(j => { document.getElementById('tok-value').textContent = j.token + '  (copy it now; it is not shown again)'; state.secrets['auth.api_token_hash'] = true; })
        .catch(e => { document.getElementById('tok-value').textContent = e.message; });
    };
    const tokRevoke = document.getElementById('tok-revoke');
    if (tokRevoke) tokRevoke.onclick = () => post('/api/auth/token', null, 'DELETE').then(load);
    document.querySelectorAll('button.clear').forEach(b => b.onclick = () => {
      clearSecrets.add(b.dataset.key);
      b.previousElementSibling.placeholder = '';
      b.remove();
    });
    const path = state.path ? state.path : 'not saved to a file yet';
    $('#save-status').textContent = state.restart_required.length ? `Restart to apply: ${state.restart_required.join(', ')}` : path;
  };

  const collect = () => {
    const out = JSON.parse(JSON.stringify(state.config));
    const env = new Set(state.env);
    for (const sec of SCHEMA) for (const f of sec.fields) {
      if (env.has(f.key) || NON_DATA.has(f.type)) continue;
      const el = document.getElementById(id(f.key));
      let v;
      switch (f.type) {
        case 'bool': v = el.checked; break;
        case 'lines': v = el.value.split('\n').map(s => s.trim()).filter(Boolean); break;
        case 'map': v = {}; for (const line of el.value.split('\n')) { const i = line.indexOf('='); if (i > 0) v[line.slice(0, i).trim()] = line.slice(i + 1).trim(); } break;
        case 'number': v = el.value === '' ? 0 : parseInt(el.value, 10); break;
        case 'float': v = el.value === '' ? 0 : parseFloat(el.value); break;
        case 'secret': v = el.value; break;
        default: v = el.value.trim();
      }
      set(out, f.key, v);
    }
    return out;
  };

  // One section at a time, chosen in the sidebar (a menu on phones) and
  // kept in the address so a reload stays put.
  const visible = () => SCHEMA.filter(sec => showAdvanced || hasBasic(sec));
  const current = () => {
    const want = location.hash.slice(1);
    const secs = visible();
    return (secs.find(s => slug(s.title) === want) || secs[0]);
  };
  const renderNav = () => {
    document.body.classList.toggle('show-adv', showAdvanced);
    const cur = current();
    const restart = sec => sec.fields.some(f => state.restart_required.includes(f.key) || state.restart_required.includes(f.key.split('.')[0]));
    $('#settings-nav').innerHTML = `<label class="adv-toggle"><input type="checkbox" id="adv-toggle"${showAdvanced ? ' checked' : ''}> Show advanced settings</label>
      <ul>${visible().map(sec => `<li><a href="#${slug(sec.title)}" class="${sec === cur ? 'sel' : ''}">${sec.title}${restart(sec) ? ' <span class="tag warn">restart</span>' : ''}</a></li>`).join('')}</ul>
      <select id="sec-select" aria-label="Section">${visible().map(sec => `<option value="${slug(sec.title)}"${sec === cur ? ' selected' : ''}>${sec.title}</option>`).join('')}</select>`;
    for (const el of document.querySelectorAll('#settings > section')) el.classList.toggle('active', el.id === 'sec-' + slug(cur.title));
    $('#adv-toggle').onchange = e => {
      showAdvanced = e.target.checked;
      try { localStorage.setItem('mr-settings-advanced', showAdvanced ? '1' : '0'); } catch (err) { /* private mode */ }
      renderNav();
    };
    $('#sec-select').onchange = e => { location.hash = e.target.value; };
  };
  window.addEventListener('hashchange', () => { if (state) renderNav(); });

  const load = () => fetch('/api/config').then(r => r.json()).then(s => { state = s; clearSecrets.clear(); render(); });

  $('#save').onclick = () => {
    const btn = $('#save');
    btn.disabled = true;
    $('#save-status').textContent = '';
    fetch('/api/config', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ config: collect(), clear_secrets: [...clearSecrets] }) })
      .then(async r => { const j = await r.json(); if (!r.ok) throw new Error(j.error || r.statusText); return j; })
      .then(s => { state = s; clearSecrets.clear(); render(); $('#save-status').textContent = (s.restart_required.length ? `Saved. Restart to apply: ${s.restart_required.join(', ')}` : 'Saved'); })
      .catch(e => { $('#save-status').textContent = e.message; $('#save-status').classList.add('error'); setTimeout(() => $('#save-status').classList.remove('error'), 4000); })
      .finally(() => { btn.disabled = false; });
  };
  $('#reload').onclick = load;
  load();
})();
