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
      { key: 'arr.radarr.import_mode', label: 'Import mode', type: 'select', options: ['move', 'copy'] },
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
      { key: 'arr.sonarr.import_mode', label: 'Import mode', type: 'select', options: ['move', 'copy'] },
      { key: 'arr.sonarr.path_map', label: 'Path map', type: 'map', hint: 'local path = path as Sonarr sees it', wide: true },
    ]},
    { title: 'Handoff', fields: [
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
    ]},
    { title: 'Notifications', fields: [
      { key: 'notify.ntfy_url', label: 'ntfy URL', type: 'text', wide: true },
      { key: 'notify.ntfy_token', label: 'ntfy token', type: 'secret' },
      { key: 'notify.webhook_url', label: 'Webhook URL', type: 'text', wide: true },
    ]},
    { title: 'Logging', fields: [
      { key: 'log.level', label: 'Level', type: 'select', options: ['debug', 'info', 'warn', 'error'] },
      { key: 'log.format', label: 'Format', type: 'select', options: ['text', 'json'] },
    ]},
  ];

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
      case 'number': return `<input type="number" step="1" id="${id(f.key)}" value="${value ?? ''}"${dis}>`;
      case 'float': return `<input type="number" step="0.01" min="0" max="1" id="${id(f.key)}" value="${value ?? ''}"${dis}>`;
      default: return `<input type="text" id="${id(f.key)}" value="${String(value ?? '').replace(/"/g, '&quot;')}"${dis}>`;
    }
  };

  const render = () => {
    const cfg = state.config;
    const env = new Set(state.env);
    $('#settings').innerHTML = SCHEMA.map(sec => `<section class="card"><h2>${sec.title}</h2><div class="grid">${sec.fields.map(f => {
      const locked = env.has(f.key);
      return `<label class="field${f.wide ? ' wide' : ''}${f.type === 'bool' ? ' check' : ''}" for="${id(f.key)}">
        <span class="name">${f.label}${locked ? ' <span class="tag">env</span>' : ''}${state.restart_required.includes(f.key.split('.')[0]) || state.restart_required.includes(f.key) ? ' <span class="tag warn">restart</span>' : ''}</span>
        ${control(f, get(cfg, f.key), locked)}
        ${f.hint ? `<span class="hint">${f.hint}</span>` : ''}
      </label>`;
    }).join('')}</div></section>`).join('');
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
      if (env.has(f.key)) continue;
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
