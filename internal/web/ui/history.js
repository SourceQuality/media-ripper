(() => {
  const $ = (s, el = document) => el.querySelector(s);
  const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
  const pad = n => String(n).padStart(2, '0');
  const fmtDur = ns => { const t = Math.round((ns || 0) / 1e9), h = Math.floor(t / 3600), m = Math.floor(t % 3600 / 60), s = t % 60; return h ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`; };
  const fmtBytes = n => !n ? '' : n >= 1e9 ? (n / 1e9).toFixed(2) + ' GB' : (n / 1e6).toFixed(0) + ' MB';
  const fmtWhen = t => t ? new Date(t).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }) : '';

  const heading = j => {
    const id = j.identity || {};
    if (id.kind === 'tv' && id.title) return `${id.title} · Season ${id.season || 1}${id.disc ? ', Disc ' + id.disc : ''}`;
    if (id.kind === 'movie' && id.title) return id.year ? `${id.title} (${id.year})` : id.title;
    return j.label || 'Unidentified disc';
  };
  // Titles are as trustworthy as the disc's match: checked against
  // TheDiscDB (or its content hash), or confirmed by a person.
  const verified = j => j.hash_matched || (j.catalog_matched > 0 && j.catalog_matched === j.catalog_compared) || (j.identity && j.identity.source === 'manual');

  let all = [];
  const icon = { done: '✓', failed: '✕', cancelled: '–', skipped: '↷', review: '?' };

  const renderList = () => {
    const q = $('#filter').value.trim().toLowerCase();
    const rows = all.filter(j => !q || heading(j).toLowerCase().includes(q) || (j.label || '').toLowerCase().includes(q));
    $('#jobs').innerHTML = rows.map(j => `<li><a href="#${encodeURIComponent(j.id)}" class="${location.hash === '#' + encodeURIComponent(j.id) ? 'sel' : ''}">
      <span class="badge ${esc(j.stage)}">${icon[j.stage] || '•'}</span><span><strong>${esc(heading(j))}</strong><br><span class="muted">${esc(fmtWhen(j.finished_at || j.started_at))}</span></span></a></li>`).join('') || '<li class="muted">Nothing yet</li>';
  };

  const decision = (j, t) => {
    const pick = ((j.selection && j.selection.picks) || []).find(p => p.title.id === t.id);
    if (pick) {
      const what = pick.episode ? `S${pad(pick.season)}E${pad(pick.episode)} ${pick.episode_title || ''}` : (j.identity && j.identity.kind === 'movie' ? 'Main movie' : 'Ripped');
      const badge = verified(j) ? '<span class="tag ok">verified</span>' : '<span class="tag warn">unverified</span>';
      return `<strong>${esc(what)}</strong> ${badge}<div class="muted small">${esc(pick.reason)}</div>`;
    }
    const skip = ((j.selection && j.selection.skipped) || []).find(s => s.title_id === t.id);
    return `<span class="muted">Skipped${skip ? ': ' + esc(skip.reason) : ''}</span>`;
  };

  const fileCell = (j, t) => {
    const o = (j.outputs || []).find(o => o.title_id === t.id);
    if (!o) return '';
    const state = o.import ? `<span class="${o.import === 'imported' ? 'ok' : 'warn'}">${esc(o.import)}</span>` : 'delivered';
    return `<div class="small">${o.ripped_as ? `<code>${esc(o.ripped_as)}</code> → ` : ''}<span class="path">${esc(o.path)}</span></div><div class="small">${state}</div>`;
  };

  const renderDetail = id => {
    const view = $('#detail-view');
    if (!id) { view.innerHTML = '<div class="card empty">Choose a disc</div>'; return; }
    fetch(`/api/jobs/${encodeURIComponent(id)}`).then(r => r.json()).then(j => {
      if (j.error) { view.innerHTML = `<div class="card empty">${esc(j.error)}</div>`; return; }
      const ids = [['Disc label', j.label], ['Fingerprint', j.fingerprint], ['Content hash', j.content_hash], ['Catalogue', j.catalog], ['Backup', j.backup && j.backup.path], ['Drive', j.drive], ['Disc', j.disc_type],
        ['Started', fmtWhen(j.started_at)], ['Took', j.elapsed]].filter(([, v]) => v);
      const titles = (j.titles || []).map(t => `<tr>
        <td><code>${esc(t.source || 't' + pad(t.id))}</code><div class="muted small">title ${t.id}</div></td>
        <td>${esc(fmtDur(t.duration))}<div class="muted small">${t.chapters} ch</div></td>
        <td>${esc(t.size || fmtBytes(t.size_bytes))}<div class="muted small">${esc(t.video || '')}</div></td>
        <td class="small">${t.audio} audio<br>${t.subtitles} subs</td>
        <td>${decision(j, t)}</td><td>${fileCell(j, t)}</td></tr>`).join('');
      view.innerHTML = `<div class="card">
        <div class="row"><span class="badge ${esc(j.stage)}">${icon[j.stage] || '•'}</span><span class="title">${esc(heading(j))}</span><span class="spacer"></span><span class="muted">${esc(j.stage)}</span></div>
        ${j.verification ? `<div class="sub muted">${esc(j.verification)}</div>` : ''}
        ${j.error ? `<div class="stage failed">${esc(j.error)}</div>` : ''}
        ${(j.warnings || []).map(w => `<div class="warning">${esc(w)}</div>`).join('')}
        <dl class="ids">${ids.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('')}</dl>
        <div class="row actions-row">
          <a class="btn" href="/api/jobs/${encodeURIComponent(j.id)}/manifest" download>Disc manifest (.odm.json)</a>
          <a class="btn" href="/api/jobs/${encodeURIComponent(j.id)}/contribution" target="_blank">Title mapping</a>
          <a class="btn" href="https://thediscdb.com/contribute" target="_blank" rel="noopener">Contribute on TheDiscDB</a>
        </div>
        <p class="muted small">${verified(j) ? 'This disc is already verified against TheDiscDB or by you.' : 'Not in TheDiscDB yet? Upload the disc manifest on its Disc Manifest tab and enter the title mapping once Radarr/Sonarr has imported the files.'}</p>
      </div>
      <div class="card"><h2>Every title on the disc</h2>
        <div class="table-wrap"><table class="titles"><thead><tr><th>Source</th><th>Length</th><th>Size</th><th>Tracks</th><th>What it is</th><th>File</th></tr></thead><tbody>${titles || '<tr><td colspan="6" class="empty">No scan recorded</td></tr>'}</tbody></table></div>
      </div>
      ${(j.log || []).length ? `<div class="card"><h2>Log</h2><pre class="log">${esc(j.log.map(l => `${new Date(l.time).toLocaleTimeString()}  ${l.message}`).join('\n'))}</pre></div>` : ''}`;
    });
  };

  // Totals since the first disc.
  fetch('/api/stats').then(r => r.json()).then(s => {
    const done = (s.discs.done || 0) + (s.discs.review || 0);
    const hours = s.video_seconds / 3600;
    const rate = s.busy_seconds > 0 ? s.bytes / s.busy_seconds / 1e6 : 0;
    $('#stats').innerHTML = `<div class="big">${done}</div><div class="muted small">discs ripped${s.first_at ? ' since ' + new Date(s.first_at).toLocaleDateString() : ''}</div>
      <div class="small">${s.titles} titles · ${(s.bytes / 1e9).toFixed(0)} GB · ${hours.toFixed(1)} h of video</div>
      <div class="small">${s.verified} verified · ${s.imported} imported${s.review_pending ? ` · <strong>${s.review_pending} waiting for review</strong>` : ''}</div>
      ${rate ? `<div class="muted small">${rate.toFixed(1)} MB/s per disc on average, start to finish</div>` : ''}`;
  }).catch(() => {});

  const route = () => { renderList(); renderDetail(decodeURIComponent(location.hash.slice(1))); };
  $('#filter').oninput = renderList;
  window.addEventListener('hashchange', route);
  fetch('/api/history?limit=500').then(r => r.json()).then(h => { all = Array.isArray(h) ? h : []; route(); });
})();
