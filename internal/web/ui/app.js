(() => {
  const $ = (s, el = document) => el.querySelector(s);
  const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));

  const post = (url, body) => fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined }).then(r => r.json()).then(j => { if (j.error) alert(j.error); refresh(); });
  window.act = {
    eject: d => post(`/api/drives/${encodeURIComponent(d.replace('/dev/', ''))}/eject`),
    rescan: d => post(`/api/drives/${encodeURIComponent(d.replace('/dev/', ''))}/rescan`),
    // How the disc in the drive (or the next one) is handled instead of
    // the configured mode: "contribute" (TheDiscDB only: scanned and kept,
    // not ripped), "label" (wait for you to label its titles), "" cancels.
    next: (d, mode) => post(`/api/drives/${encodeURIComponent(d.replace('/dev/', ''))}/next`, { mode }),
    cancel: id => post(`/api/jobs/${encodeURIComponent(id)}/cancel`),
    retry: (id, btn) => {
      btn.disabled = true; btn.textContent = 'Importing…';
      post(`/api/jobs/${encodeURIComponent(id)}/retry-import`).finally(() => { btn.disabled = false; btn.textContent = 'Retry import'; });
    },
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
  const actName = { rip: 'Ripping', remux: 'Remuxing', copy: 'Copying', backup: 'Backing up' };
  const activity = a => {
    const size = a.total > 0 ? `${fmtBytes(a.done)} / ${fmtBytes(a.total)}` : fmtBytes(a.done);
    const nums = [size, fmtSpeed(a.speed), fmtETA(a.eta_seconds)].filter(Boolean).join(' · ');
    const pct = a.percent >= 0 ? a.percent : -1;
    return `<div class="act"><div class="row"><span class="kind">${esc(actName[a.kind] || a.kind)}</span><span>${esc(a.item)}</span>
      <span class="spacer"></span><span class="nums">${esc(nums)}</span></div>
      <div class="bar small ${pct < 0 ? 'indeterminate' : ''}"><div style="width:${pct < 0 ? 0 : pct}%"></div></div></div>`;
  };
  const terminal = s => ['done', 'failed', 'skipped', 'cancelled', 'review'].includes(s);

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
    // Steps not tied to one title, like the full-disc backup.
    const titled = new Set(order.map(({ p }) => p.episode ? `S${pad(p.season)}E${pad(p.episode)}` : `title ${p.title.id}`));
    const other = (j.activities || []).filter(a => !titled.has(a.item)).map(activity).join('');
    return `<ul class="eps live">${lines.join('')}</ul>${other}`;
  };

  const driveCard = d => {
    const j = d.job && !terminal(d.job.stage) ? d.job : null;
    let body;
    if (j) {
      // Ripping: progress across all titles. Afterwards: titles delivered.
      const pct = j.stage === 'ripping' && j.progress >= 0 ? j.overall
        : ['postprocessing', 'delivering'].includes(j.stage) && j.total ? (j.outputs || []).length / j.total * 100 : -1;
      body = `<div class="row"><span class="title">${esc(jobTitle(j))}</span><span class="stage">${esc(stageText(j))}</span><span class="spacer"></span>
        ${j.stage === 'labelling' ? `<a class="btn primary" href="history.html#${encodeURIComponent(j.id)}">Label titles</a>` : `<a class="btn" href="history.html#${encodeURIComponent(j.id)}">Disc details</a>`}<button class="danger" onclick="act.cancel('${esc(j.id)}')">Cancel</button></div>
        ${j.verification ? `<div class="sub muted verify">${esc(j.verification)}</div>` : ''}
        <div class="bar ${pct < 0 ? 'indeterminate' : ''}"><div style="width:${pct < 0 ? 0 : pct}%"></div></div>
        ${j.contribute ? '<div class="sub muted">TheDiscDB only: kept for contributing, not ripped.</div>'
          : j.stage === 'labelling' ? '<div class="sub">The disc waits in the drive for you to label its titles.</div>' : liveList(j)}`;
    } else {
      const status = d.status === 'disc-ok' ? (d.ignored ? 'Disc already ripped' : (d.label || 'Disc inserted')) : d.status === 'tray-open' ? 'Tray open' : d.status === 'no-disc' ? 'Empty' : d.status;
      const last = d.job ? `<span class="stage ${esc(d.job.stage)}">${esc(jobTitle(d.job))} · ${esc(d.job.stage)}</span>` : '';
      const inDrive = d.status === 'disc-ok', nextName = { contribute: 'TheDiscDB only', label: 'label first' };
      const choices = d.next_disc
        ? `<span class="tag">${inDrive ? 'This' : 'Next'} disc: ${esc(nextName[d.next_disc] || d.next_disc)}</span><button onclick="act.next('${esc(d.path)}', '')">Cancel</button>`
        : `<button title="Scan the disc and keep it for TheDiscDB without ripping it" onclick="act.next('${esc(d.path)}', 'contribute')">${inDrive ? 'TheDiscDB only' : 'Next disc: TheDiscDB only'}</button>`
          + (mode !== 'manual' ? `<button title="Scan the disc, then wait for you to label its titles before ripping" onclick="act.next('${esc(d.path)}', 'label')">${inDrive ? 'Label first' : 'Next disc: label first'}</button>` : '');
      body = `<div class="row"><span class="title">${esc(status)}</span>${last}<span class="spacer"></span>
        ${inDrive && !d.next_disc ? `<button onclick="act.rescan('${esc(d.path)}')">${mode === 'manual' ? 'Scan' : 'Rip'}</button>` : ''}${choices}${inDrive ? `<button onclick="act.eject('${esc(d.path)}')">Eject</button>` : ''}</div>
        ${d.last_error ? `<div class="stage failed">${esc(d.last_error)}</div>` : ''}`;
    }
    const head = d.model ? `<span class="model">${esc(d.model)}</span> <span class="drive">${esc(d.path)}</span>` : `<span class="drive">${esc(d.path)}</span>`;
    return `<div class="card"><div class="drive-head">${head}</div>${body}</div>`;
  };

  const fmtWhen = t => { const d = new Date(t); const today = new Date(); return d.toDateString() === today.toDateString() ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); };

  let models = {};
  let mode = 'auto'; // what happens when a disc goes in (Settings)
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
    if (j.contribute && j.stage === 'done') return 'Kept for TheDiscDB; not ripped';
    switch (j.stage) {
      case 'cancelled': return `Cancelled after ${fmtElapsed(j.elapsed)}${total ? ` · ${done} of ${plural(total, unit)} delivered` : ''}`;
      case 'failed': return `Failed${j.error ? ': ' + j.error : ''}${total ? ` · ${done} of ${plural(total, unit)} delivered` : ''}`;
      case 'skipped': return j.error || 'Skipped';
    }
    return '';
  };

  // Files Radarr/Sonarr did not take are still in staging: offer a retry.
  const importFailed = j => j.stage === 'done' && (j.outputs || []).some(o => /^not (imported|placed)/.test(o.import || ''));
  const icon = { done: '✓', failed: '✕', cancelled: '–', skipped: '↷', review: '?' };
  const historyCard = j => {
    const warnings = (j.warnings || []).map(w => `<div class="warning">${esc(w)}</div>`).join('');
    const outcome = outcomeLine(j);
    const lines = j.contribute ? '' : pickLines(j);
    const skipped = skippedLine(j);
    return `<div class="card history ${esc(j.stage)}">
      <div class="row"><span class="badge ${esc(j.stage)}" title="${esc(j.stage)}">${icon[j.stage] || '•'}</span>
        <span class="title">${esc(discHeading(j))}</span><span class="spacer"></span>
        <span class="muted">${esc(fmtWhen(j.finished_at || j.started_at))}${j.elapsed && j.stage === 'done' ? ' · ' + esc(fmtElapsed(j.elapsed)) : ''}</span></div>
      <div class="sub muted">${esc(matchLine(j))}${j.drive ? ` · ${esc(driveName(j.drive))}` : ''}</div>
      ${outcome ? `<div class="outcome ${esc(j.stage)}">${esc(outcome)}</div>` : ''}
      ${lines ? `<ul class="eps">${lines}</ul>` : ''}
      ${skipped ? `<div class="sub muted">${esc(skipped)}</div>` : ''}
      ${j.backup ? `<div class="sub muted">Full-disc backup: ${esc(j.backup.path)} (${esc(fmtBytes(j.backup.size))})</div>` : ''}
      ${warnings}
      <div class="row end">${importFailed(j) ? `<button class="primary" onclick="act.retry('${esc(j.id)}', this)">Retry import</button>` : ''}<button onclick="act.detail('${esc(j.id)}')">Details</button></div>
    </div>`;
  };

  const renderRecent = () => {
    const cards = history.filter(j => terminal(j.stage)).map(historyCard);
    $('#recent').innerHTML = cards.length ? cards.join('') : '<div class="card empty">Nothing yet</div>';
    setCount('recent', cards.length);
  };

  // Banners for things that need a person: storage that stopped answering,
  // a newer release.
  const notices = s => {
    const out = [];
    const st = s.storage;
    if (st && st.state && st.state !== 'ok') {
      const what = st.state === 'slow' ? `is slow (${(st.latency_ms / 1000).toFixed(1)} s to write a test file)` : `is ${st.state}${st.error ? ': ' + st.error : ''}`;
      out.push(`<div class="notice ${st.state === 'slow' ? 'warn' : 'bad'}">Library storage <code>${esc(st.path)}</code> ${esc(what)}. Ripping continues; delivery waits for it.</div>`);
    } else if (st && st.free_bytes && st.free_bytes < 100e9) {
      out.push(`<div class="notice warn">Library storage <code>${esc(st.path)}</code> has only ${esc(fmtBytes(st.free_bytes))} free.</div>`);
    }
    if (s.update) out.push(`<div class="notice info">media-ripper ${esc(s.update.tag)} is available (this is ${esc(s.version)}). <a href="${esc(s.update.url)}" target="_blank" rel="noopener">Release notes</a></div>`);
    return out.join('');
  };

  // Discs held for a person. Only re-rendered when the set of held discs
  // changes, so a half-filled form survives the 2-second refresh.
  let reviewKey = null;
  const renderReviews = list => {
    const key = list.map(j => j.id).join(',');
    if (key === reviewKey) return;
    reviewKey = key;
    $('#reviews').innerHTML = list.length ? list.map(reviewCard).join('') : '<div class="card empty">Nothing is waiting. Discs whose titles could not be verified stop here before import.</div>';
    setCount('review', list.length, list.length > 0);
    for (const card of document.querySelectorAll('.review')) wireReview(card);
  };

  const reviewCard = j => {
    const id = j.identity || {};
    const tv = id.kind !== 'movie';
    const picks = (j.selection && j.selection.picks) || [];
    const outs = Object.fromEntries((j.outputs || []).map(o => [o.title_id, o]));
    const rows = picks.map(p => `<li data-title="${p.title.id}">
        <label class="keep"><input type="checkbox" checked> <span class="code">Title ${p.title.id}</span></label>
        <span class="meta">${esc(fmtDur(p.title.duration))}</span><span class="meta">${esc(fmtBytes((outs[p.title.id] || {}).size || p.title.size_bytes || 0))}</span>
        <span class="name">${p.episode ? esc(`was S${pad(p.season)}E${pad(p.episode)} ${p.episode_title || ''}`) : esc(p.title.source_file || '')}</span>
        <label class="ep ${tv ? '' : 'hidden'}">Episode <input type="number" min="1" value="${p.episode || ''}"></label></li>`).join('');
    return `<div class="card review" data-id="${esc(j.id)}" data-tmdb="${id.tmdb_id || ''}" data-tvdb="${id.tvdb_id || ''}">
      <div class="row"><span class="badge review">?</span><span class="title">${esc(discHeading(j))}</span><span class="spacer"></span><span class="muted">${esc(fmtWhen(j.finished_at || j.started_at))}</span></div>
      <div class="sub muted">${esc(j.verification || '')}</div>
      <div class="form">
        <label>Type <select class="kind"><option value="tv"${tv ? ' selected' : ''}>TV</option><option value="movie"${tv ? '' : ' selected'}>Movie</option></select></label>
        <label class="grow">Title <input class="title-in" value="${esc(id.title || '')}"></label>
        <label>Year <input class="year" type="number" value="${id.year || ''}"></label>
        <label class="season-l ${tv ? '' : 'hidden'}">Season <input class="season" type="number" min="1" value="${id.season || 1}"></label>
        <button type="button" class="search">Search</button>
      </div>
      <div class="results"></div>
      <ul class="eps review-eps">${rows}</ul>
      <div class="row end"><span class="status muted"></span><button type="button" class="discard">Discard</button><button type="button" class="primary approve">Approve &amp; import</button></div>
    </div>`;
  };

  const wireReview = card => {
    const q = s => card.querySelector(s);
    const kind = () => q('.kind').value;
    q('.kind').onchange = () => {
      card.querySelectorAll('.ep, .season-l').forEach(el => el.classList.toggle('hidden', kind() !== 'tv'));
    };
    q('.search').onclick = () => {
      const out = q('.results');
      out.textContent = 'Searching…';
      fetch(`/api/lookup?kind=${kind()}&q=${encodeURIComponent(q('.title-in').value)}`).then(async r => { const j = await r.json(); if (!r.ok) throw new Error(j.error); return j; })
        .then(list => {
          out.innerHTML = list.length ? list.map((x, i) => `<button type="button" data-i="${i}">${esc(x.title)}${x.year ? ' (' + x.year + ')' : ''}</button>`).join('') : 'Nothing found';
          out.querySelectorAll('button').forEach(b => b.onclick = () => {
            const x = list[+b.dataset.i];
            q('.title-in').value = x.title; q('.year').value = x.year || '';
            card.dataset.tmdb = x.tmdb_id || ''; card.dataset.tvdb = x.tvdb_id || '';
            out.innerHTML = '';
          });
        }).catch(e => { out.textContent = e.message; });
    };
    // Typing a new title forgets the ids of the old one.
    q('.title-in').oninput = () => { card.dataset.tmdb = ''; card.dataset.tvdb = ''; };
    const status = q('.status');
    q('.approve').onclick = () => {
      const episodes = {};
      for (const li of card.querySelectorAll('.review-eps li')) {
        if (!li.querySelector('.keep input').checked) continue;
        episodes[li.dataset.title] = kind() === 'tv' ? parseInt(li.querySelector('.ep input').value, 10) || 0 : 0;
      }
      const edit = { kind: kind(), title: q('.title-in').value.trim(), year: parseInt(q('.year').value, 10) || 0,
        tmdb_id: parseInt(card.dataset.tmdb, 10) || 0, tvdb_id: parseInt(card.dataset.tvdb, 10) || 0,
        season: kind() === 'tv' ? parseInt(q('.season').value, 10) || 0 : 0, episodes };
      card.querySelectorAll('button').forEach(b => b.disabled = true);
      status.textContent = 'Importing… (the app moves the files; this can take a minute)';
      fetch(`/api/reviews/${encodeURIComponent(card.dataset.id)}/approve`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(edit) })
        .then(async r => { const j = await r.json(); if (!r.ok) throw new Error(j.error); return j; })
        .then(j => { status.textContent = (j.warnings || []).length ? 'Imported with warnings: ' + j.warnings.join('; ') : 'Imported'; reviewKey = null; refresh(); })
        .catch(e => { status.textContent = e.message; card.querySelectorAll('button').forEach(b => b.disabled = false); });
    };
    q('.discard').onclick = () => {
      if (!confirm('Forget this disc? Its files stay in staging; nothing is imported.')) return;
      fetch(`/api/reviews/${encodeURIComponent(card.dataset.id)}/discard`, { method: 'POST' }).then(() => { reviewKey = null; refresh(); });
    };
  };

  // One panel at a time, picked in the sidebar (a menu on phones); counts
  // in the sidebar say where there is something to look at.
  const PANELS = ['drives', 'review', 'boxsets', 'recent'];
  const setCount = (panel, n, highlight) => {
    const el = document.getElementById('count-' + panel);
    if (!el) return;
    el.textContent = n ? n : '';
    el.classList.toggle('hot', !!highlight);
  };
  const showPanel = () => {
    const want = location.hash.slice(1);
    const cur = PANELS.includes(want) ? want : 'drives';
    document.querySelectorAll('.panel').forEach(p => p.classList.toggle('active', p.dataset.panel === cur));
    document.querySelectorAll('#home-nav a').forEach(a => a.classList.toggle('sel', a.dataset.panel === cur));
    const sel = document.getElementById('panel-select');
    if (sel) sel.value = cur;
  };
  window.addEventListener('hashchange', showPanel);
  const panelSelect = document.getElementById('panel-select');
  if (panelSelect) panelSelect.onchange = e => { location.hash = e.target.value; };
  showPanel();

  // Box sets in progress: which discs of each catalogued release are done.
  const renderBoxSets = sets => {
    setCount('boxsets', sets.length);
    $('#boxsets').innerHTML = !sets.length ? '<div class="card empty">Discs matched in TheDiscDB show up here with the rest of their set.</div>' : `${sets.map(b => `<div class="card boxset">
      <div class="row"><span class="title">${esc(b.title)}${b.year ? ` (${b.year})` : ''}</span><span class="muted">${esc(b.release_name)}</span><span class="spacer"></span>
        <span class="muted">${b.total ? `${b.ripped} of ${plural(b.total, 'disc')}` : plural(b.ripped, 'disc') + ' ripped'}</span></div>
      ${b.groups.map(g => `<div class="group"><span class="gname">${esc(g.name)}</span><span class="chips">${g.discs.map(d =>
        `<span class="chip ${d.ripped ? 'ok' : 'missing'}" title="${esc(d.name)}${d.ripped ? ' · ripped ' + esc(fmtWhen(d.at)) : ' · not ripped yet'}">${esc(d.short)}${d.ripped ? ' ✓' : ''}</span>`).join('')}</span>
        <span class="muted">${g.discs.filter(d => d.ripped).length}/${g.discs.length}</span></div>`).join('')}
    </div>`).join('')}`;
  };

  const render = s => {
    $('#version').textContent = s.version || '';
    $('#notices').innerHTML = notices(s);
    models = Object.fromEntries(s.drives.filter(d => d.model).map(d => [d.path, d.model]));
    mode = s.mode || 'auto';
    // Jobs carried on after a restart whose disc was already ejected.
    const finishing = (s.finishing || []).map(j => driveCard({ path: j.drive, model: models[j.drive], job: j })
      .replace('<div class="card">', '<div class="card finishing">').replace(/<button class="danger"[^>]*>Cancel<\/button>/, ''));
    $('#drives').innerHTML = (s.drives.length ? s.drives.map(driveCard).join('') : '<div class="card empty">No optical drives</div>') +
      (finishing.length ? `<h3 class="muted small">Finishing after a restart (disc already out)</h3>${finishing.join('')}` : '');
    const busy = s.drives.filter(d => d.job && !terminal(d.job.stage)).length + finishing.length;
    setCount('drives', busy ? busy + ' ripping' : '', busy > 0);
    renderRecent();
  };

  // Box sets change only when a disc finishes; refresh them every 30 s.
  let boxSetsAt = 0;
  const boxSetsDue = () => { if (Date.now() - boxSetsAt < 30000) return false; boxSetsAt = Date.now(); return true; };

  let timer;
  const refresh = () => Promise.all([
    fetch('/api/status').then(r => r.json()),
    fetch('/api/history?limit=25').then(r => r.json()).catch(() => history),
    fetch('/api/reviews').then(r => r.json()).catch(() => null),
    boxSetsDue() ? fetch('/api/boxsets').then(r => r.json()).catch(() => null) : null,
  ]).then(([s, h, rv, bs]) => { history = Array.isArray(h) ? h : []; render(s); if (Array.isArray(rv)) renderReviews(rv); if (Array.isArray(bs)) renderBoxSets(bs); }).catch(() => {})
    .finally(() => { clearTimeout(timer); timer = setTimeout(refresh, document.hidden ? 10000 : 2000); });
  document.addEventListener('visibilitychange', refresh);
  refresh();
})();
