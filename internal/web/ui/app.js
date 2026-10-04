(() => {
  const $ = (s, el = document) => el.querySelector(s);
  const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));

  const post = (url, body) => fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined }).then(r => r.json()).then(j => { if (j.error) alert(j.error); refresh(); });
  window.act = {
    eject: d => post(`/api/drives/${encodeURIComponent(d.replace('/dev/', ''))}/eject`),
    rescan: d => post(`/api/drives/${encodeURIComponent(d.replace('/dev/', ''))}/rescan`),
    cancel: id => post(`/api/jobs/${encodeURIComponent(id)}/cancel`),
    detail: id => fetch(`/api/jobs/${encodeURIComponent(id)}`).then(r => r.json()).then(j => {
      $('#detail-title').textContent = jobTitle(j);
      $('#detail-log').textContent = (j.log || []).length
        ? j.log.map(l => `${new Date(l.time).toLocaleTimeString()}  ${l.message}`).join('\n')
        : summary(j);
      $('#detail').showModal();
    }),
  };
  $('#detail-close').onclick = () => $('#detail').close();

  // From the saved history (a job from before a restart): the outcome
  // without the step-by-step log.
  const summary = j => {
    const lines = [`${j.stage}${j.elapsed ? ' after ' + j.elapsed : ''} · started ${new Date(j.started_at).toLocaleString()}`];
    if (j.error) lines.push('', 'Error: ' + j.error);
    const picks = (j.selection && j.selection.picks) || [];
    if (picks.length) lines.push('', 'Titles:', ...picks.map(p => `  t${String(p.title.id).padStart(2, '0')} ${p.episode ? `S${String(p.season).padStart(2, '0')}E${String(p.episode).padStart(2, '0')} ${p.episode_title || ''}` : ''}  (${p.reason})`));
    if ((j.outputs || []).length) lines.push('', 'Files:', ...j.outputs.map(o => '  ' + o.path));
    lines.push('', 'The detailed log is not kept after a restart.');
    return lines.join('\n');
  };

  const jobTitle = j => {
    const id = j.identity;
    if (id && id.kind !== 'unknown' && id.title) {
      if (id.kind === 'tv') return `${id.title} S${String(id.season || 1).padStart(2, '0')}${id.disc ? ' D' + id.disc : ''}`;
      return id.year ? `${id.title} (${id.year})` : id.title;
    }
    return j.label || 'Disc';
  };
  const stageText = j => {
    if (j.stage === 'ripping' && j.progress >= 0 && !(j.activities || []).length) return `${j.message} · ${Math.floor(j.progress)}%${j.eta ? ' · ' + j.eta : ''}`;
    return j.message || j.stage;
  };

  const fmtBytes = n => n >= 1e9 ? (n / 1e9).toFixed(2) + ' GB' : (n / 1e6).toFixed(0) + ' MB';
  const fmtSpeed = n => n > 0 ? (n / 1e6).toFixed(1) + ' MB/s' : '';
  const fmtETA = s => {
    if (!s || s <= 0) return '';
    const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), sec = s % 60;
    return (h ? `${h}h ${m}m` : m ? `${m}m ${sec}s` : `${sec}s`) + ' left';
  };
  const actName = { rip: 'Ripping', remux: 'Remuxing', copy: 'Copying' };
  const activity = a => {
    const size = a.total > 0 ? `${fmtBytes(a.done)} / ${fmtBytes(a.total)}` : fmtBytes(a.done);
    const nums = [size, fmtSpeed(a.speed), fmtETA(a.eta_seconds)].filter(Boolean).join(' · ');
    const pct = a.percent >= 0 ? a.percent : -1;
    return `<div class="act"><div class="row"><span class="kind">${esc(actName[a.kind] || a.kind)}</span><span>${esc(a.item)}</span>
      <span class="spacer"></span><span class="nums">${esc(nums)}</span></div>
      <div class="bar small ${pct < 0 ? 'indeterminate' : ''}"><div style="width:${pct < 0 ? 0 : pct}%"></div></div></div>`;
  };
  const terminal = s => ['done', 'failed', 'skipped', 'cancelled'].includes(s);

  // The disc in progress, one line per title that stays put while its
  // state moves on: waiting → ripping → ripped → copying → delivered.
  const liveList = j => {
    const picks = (j.selection && j.selection.picks) || [];
    if (!picks.length) return (j.activities || []).map(activity).join('');
    const outs = Object.fromEntries((j.outputs || []).map(o => [o.title_id, o]));
    const acts = Object.fromEntries((j.activities || []).map(a => [`${a.kind}:${a.item}`, a]));
    const movie = j.identity && j.identity.kind === 'movie';
    const ripDone = j.stage !== 'ripping';
    const order = picks.map((p, i) => ({ p, i })).sort((a, b) => (a.p.season - b.p.season) || (a.p.episode - b.p.episode) || (a.i - b.i));
    const lines = order.map(({ p, i }) => {
      const item = p.episode ? `S${pad(p.season)}E${pad(p.episode)}` : `title ${p.title.id}`;
      const o = outs[p.title.id];
      const live = acts[`copy:${item}`] || acts[`remux:${item}`] || acts[`rip:${item}`];
      let cls = 'pending', mark = '·', state = 'waiting', pct = -1;
      if (o) {
        cls = o.import && o.import !== 'imported' ? 'warn' : 'ok';
        mark = cls === 'ok' ? '✓' : '!';
        state = o.import || 'delivered';
      } else if (live) {
        cls = 'active'; mark = '▸'; pct = live.percent;
        const verb = { rip: 'ripping', remux: 'remuxing', copy: 'copying' }[live.kind];
        state = [pct >= 0 ? `${verb} ${Math.floor(pct)}%` : verb, fmtSpeed(live.speed), fmtETA(live.eta_seconds)].filter(Boolean).join(' · ');
      } else if (ripDone || i < (j.current || 1) - 1) {
        mark = '✓'; state = 'ripped, waiting to copy';
      }
      const code = p.episode ? `S${pad(p.season)}E${pad(p.episode)}${p.episode_end > p.episode ? '–E' + pad(p.episode_end) : ''}` : (movie ? 'Movie' : `Title ${p.title.id}`);
      const name = p.episode ? (p.episode_title || '') : (movie ? discHeading(j) : (p.title.source_file || ''));
      return `<li class="${cls}"${pct >= 0 ? ` style="--p:${pct.toFixed(1)}%"` : ''}><span class="mark">${mark}</span><span class="code">${esc(code)}</span><span class="name">${esc(name)}</span>
        <span class="meta">${esc(fmtDur(p.title.duration))}</span><span class="meta">${esc(fmtBytes((o && o.size) || p.title.size_bytes || 0))}</span><span class="state" title="${esc(state)}">${esc(state)}</span></li>`;
    });
    return `<ul class="eps live">${lines.join('')}</ul>`;
  };

  const driveCard = d => {
    const j = d.job && !terminal(d.job.stage) ? d.job : null;
    let body;
    if (j) {
      // Ripping: progress across all titles. Afterwards: titles delivered.
      const pct = j.stage === 'ripping' && j.progress >= 0 ? j.overall
        : ['postprocessing', 'delivering'].includes(j.stage) && j.total ? (j.outputs || []).length / j.total * 100 : -1;
      body = `<div class="row"><span class="title">${esc(jobTitle(j))}</span><span class="stage">${esc(stageText(j))}</span><span class="spacer"></span>
        <button class="danger" onclick="act.cancel('${esc(j.id)}')">Cancel</button></div>
        ${j.verification ? `<div class="sub muted verify">${esc(j.verification)}</div>` : ''}
        <div class="bar ${pct < 0 ? 'indeterminate' : ''}"><div style="width:${pct < 0 ? 0 : pct}%"></div></div>
        ${liveList(j)}`;
    } else {
      const status = d.status === 'disc-ok' ? (d.ignored ? 'Disc already ripped' : (d.label || 'Disc inserted')) : d.status === 'tray-open' ? 'Tray open' : d.status === 'no-disc' ? 'Empty' : d.status;
      const last = d.job ? `<span class="stage ${esc(d.job.stage)}">${esc(jobTitle(d.job))} · ${esc(d.job.stage)}</span>` : '';
      body = `<div class="row"><span class="title">${esc(status)}</span>${last}<span class="spacer"></span>
        ${d.status === 'disc-ok' ? `<button onclick="act.rescan('${esc(d.path)}')">Rip</button><button onclick="act.eject('${esc(d.path)}')">Eject</button>` : ''}</div>
        ${d.last_error ? `<div class="stage failed">${esc(d.last_error)}</div>` : ''}`;
    }
    const head = d.model ? `<span class="model">${esc(d.model)}</span> <span class="drive">${esc(d.path)}</span>` : `<span class="drive">${esc(d.path)}</span>`;
    return `<div class="card"><div class="drive-head">${head}</div>${body}</div>`;
  };

  const fmtWhen = t => { const d = new Date(t); const today = new Date(); return d.toDateString() === today.toDateString() ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); };

  let models = {};
  const driveName = p => models[p] || p;

  // Finished jobs come from the saved history so they survive restarts.
  let history = [];
  const pad = n => String(n).padStart(2, '0');
  const fmtDur = ns => {
    const t = Math.round((ns || 0) / 1e9), h = Math.floor(t / 3600), m = Math.floor(t % 3600 / 60), sec = t % 60;
    return h ? `${h}:${pad(m)}:${pad(sec)}` : `${m}:${pad(sec)}`;
  };
  const fmtElapsed = s => (s || '').replace(/(\d+)h/, '$1h ').replace(/(\d+)m(?!s)/, '$1m ').replace(/\d+s$/, m => s.includes('m') || s.includes('h') ? '' : m).trim();
  const plural = (n, word) => `${n} ${word}${n === 1 ? '' : 's'}`;

  // A heading people read: "The Twilight Zone · Season 1, Disc 2".
  const discHeading = j => {
    const id = j.identity;
    if (id && id.kind === 'tv' && id.title) return `${id.title} · Season ${id.season || 1}${id.disc ? ', Disc ' + id.disc : ''}`;
    if (id && id.kind === 'movie' && id.title) return id.year ? `${id.title} (${id.year})` : id.title;
    return j.label || 'Unidentified disc';
  };
  const matchLine = j => {
    if (j.verification) return j.verification;
    if (j.catalog) return `Titles from ${j.catalog}`;
    // Records saved before the catalogue name was kept still say so in
    // their pick reasons ("TheDiscDB: S01E08").
    if (((j.selection && j.selection.picks) || []).some(p => (p.reason || '').startsWith('TheDiscDB'))) return 'Titles from TheDiscDB';
    const id = j.identity;
    if (id && id.kind !== 'unknown' && id.title) return `Identified via ${id.source || 'lookup'}; titles chosen by length`;
    return j.label ? `Not identified (label ${j.label}); ripped by length` : '';
  };

  // One line per ripped title: what it is and what became of it.
  const pickLines = j => {
    const picks = ((j.selection && j.selection.picks) || []).slice().sort((a, b) => (a.season - b.season) || (a.episode - b.episode));
    const outs = Object.fromEntries((j.outputs || []).map(o => [o.title_id, o]));
    const movie = j.identity && j.identity.kind === 'movie';
    return picks.map(p => {
      const o = outs[p.title.id];
      let mark = '–', state = 'not delivered', cls = 'pending';
      if (o && o.import === 'imported') { mark = '✓'; state = 'imported'; cls = 'ok'; }
      else if (o && o.import) { mark = '!'; state = o.import; cls = 'warn'; }
      else if (o) { mark = '✓'; state = 'delivered'; cls = 'ok'; }
      else if (j.stage === 'done') { mark = '!'; cls = 'warn'; }
      const code = p.episode ? `S${pad(p.season)}E${pad(p.episode)}${p.episode_end > p.episode ? '–E' + pad(p.episode_end) : ''}` : (movie ? 'Movie' : `Title ${p.title.id}`);
      const name = p.episode ? (p.episode_title || '') : (movie ? discHeading(j) : (p.title.source_file || ''));
      const size = o ? o.size : p.title.size_bytes;
      return `<li class="${cls}"><span class="mark">${mark}</span><span class="code">${esc(code)}</span><span class="name">${esc(name)}</span>
        <span class="meta">${esc(fmtDur(p.title.duration))}</span><span class="meta">${size ? esc(fmtBytes(size)) : ''}</span><span class="state" title="${esc(state)}">${esc(state)}</span></li>`;
    }).join('');
  };

  // "Skipped: extra “Tales of Tomorrow”; 2 short titles"
  const skippedLine = j => {
    const skipped = (j.selection && j.selection.skipped) || [];
    if (!skipped.length) return '';
    const named = [], other = [];
    for (const s of skipped) {
      const m = s.reason.match(/(?:extra|trailer|deletedscene|featurette) "([^"]+)"/i);
      if (m) named.push(m[0].replace(/^(\w+) /, (w, k) => k.toLowerCase() + ' ').replace(/"([^"]+)"/, '“$1”'));
      else other.push(s);
    }
    const shortish = other.filter(s => s.duration < 10 * 60e9).length;
    const parts = [...named];
    if (shortish) parts.push(plural(shortish, 'short title'));
    if (other.length - shortish) parts.push(plural(other.length - shortish, 'other title'));
    return `Skipped: ${parts.join('; ')}`;
  };

  const outcomeLine = j => {
    const total = ((j.selection && j.selection.picks) || []).length;
    const done = (j.outputs || []).length;
    const unit = j.identity && j.identity.kind === 'tv' ? 'episode' : 'title';
    switch (j.stage) {
      case 'cancelled': return `Cancelled after ${fmtElapsed(j.elapsed)}${total ? ` · ${done} of ${plural(total, unit)} delivered` : ''}`;
      case 'failed': return `Failed${j.error ? ': ' + j.error : ''}${total ? ` · ${done} of ${plural(total, unit)} delivered` : ''}`;
      case 'skipped': return j.error || 'Skipped';
    }
    return '';
  };

  const icon = { done: '✓', failed: '✕', cancelled: '–', skipped: '↷' };
  const historyCard = j => {
    const warnings = (j.warnings || []).map(w => `<div class="warning">${esc(w)}</div>`).join('');
    const outcome = outcomeLine(j);
    const lines = pickLines(j);
    const skipped = skippedLine(j);
    return `<div class="card history ${esc(j.stage)}">
      <div class="row"><span class="badge ${esc(j.stage)}" title="${esc(j.stage)}">${icon[j.stage] || '•'}</span>
        <span class="title">${esc(discHeading(j))}</span><span class="spacer"></span>
        <span class="muted">${esc(fmtWhen(j.finished_at || j.started_at))}${j.elapsed && j.stage === 'done' ? ' · ' + esc(fmtElapsed(j.elapsed)) : ''}</span></div>
      <div class="sub muted">${esc(matchLine(j))}${j.drive ? ` · ${esc(driveName(j.drive))}` : ''}</div>
      ${outcome ? `<div class="outcome ${esc(j.stage)}">${esc(outcome)}</div>` : ''}
      ${lines ? `<ul class="eps">${lines}</ul>` : ''}
      ${skipped ? `<div class="sub muted">${esc(skipped)}</div>` : ''}
      ${warnings}
      <div class="row end"><button onclick="act.detail('${esc(j.id)}')">Details</button></div>
    </div>`;
  };

  const renderRecent = () => {
    const cards = history.filter(j => terminal(j.stage)).map(historyCard);
    $('#recent').innerHTML = cards.length ? cards.join('') : '<div class="card empty">Nothing yet</div>';
  };

  const render = s => {
    $('#version').textContent = s.version || '';
    models = Object.fromEntries(s.drives.filter(d => d.model).map(d => [d.path, d.model]));
    $('#drives').innerHTML = s.drives.length ? s.drives.map(driveCard).join('') : '<div class="card empty">No optical drives</div>';
    renderRecent();
  };

  let timer;
  const refresh = () => Promise.all([
    fetch('/api/status').then(r => r.json()),
    fetch('/api/history?limit=25').then(r => r.json()).catch(() => history),
  ]).then(([s, h]) => { history = Array.isArray(h) ? h : []; render(s); }).catch(() => {})
    .finally(() => { clearTimeout(timer); timer = setTimeout(refresh, document.hidden ? 10000 : 2000); });
  document.addEventListener('visibilitychange', refresh);
  refresh();
})();
