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
  const terminal = st => ['done', 'failed', 'cancelled', 'skipped', 'review'].includes(st);

  const renderList = () => {
    const q = $('#filter').value.trim().toLowerCase();
    const rows = all.filter(j => !q || heading(j).toLowerCase().includes(q) || (j.label || '').toLowerCase().includes(q));
    $('#jobs').innerHTML = rows.map(j => `<li><a href="#${encodeURIComponent(j.id)}" class="${location.hash === '#' + encodeURIComponent(j.id) ? 'sel' : ''}">
      <span class="badge ${esc(j.stage)}">${icon[j.stage] || '▶'}</span><span><strong>${esc(heading(j))}</strong><br><span class="muted">${terminal(j.stage) ? esc(fmtWhen(j.finished_at || j.started_at)) : esc(j.stage) + ' now'}</span></span></a></li>`).join('') || '<li class="muted">Nothing yet</li>';
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

  // The disc's files as read for its content hash: fetched once per disc,
  // never read from the drive again.
  let files = { id: '', inv: null, open: false };
  const filesCard = () => {
    const inv = files.inv;
    if (!inv || !(inv.files || []).length) return '';
    const total = inv.files.reduce((n, f) => n + f.size, 0);
    return `<div class="card"><details id="files"${files.open ? ' open' : ''}><summary><h2>Files on the disc</h2> <span class="muted">${inv.files.length} files · ${esc(fmtBytes(total))}</span></summary>
      <div class="table-wrap"><table class="titles"><thead><tr><th>Path</th><th>Size</th></tr></thead><tbody>
      ${inv.files.map(f => `<tr><td><code>${esc(f.path)}</code></td><td class="num">${esc(fmtBytes(f.size) || f.size + ' B')}</td></tr>`).join('')}
      </tbody></table></div></details></div>`;
  };
  const loadFiles = id => files.id === id && files.inv ? Promise.resolve() :
    fetch(`/api/jobs/${encodeURIComponent(id)}/files`).then(r => r.ok ? r.json() : null).then(inv => { files = { id, inv: inv && inv.files ? inv : null, open: files.id === id && files.open }; }).catch(() => {});

  // The folder thediscdb.com's "Add a disc" asks for, rebuilt from what
  // was kept after the rip: on the NAS share, or as a zip.
  const fmtSize = n => n >= 1e9 ? (n / 1e9).toFixed(1) + ' GB' : Math.max(1, Math.round(n / 1e6)) + ' MB';
  const folderCard = (id, f) => {
    if (!f) return '';
    let body;
    if (f.kept) {
      body = `<div class="row actions-row">
          ${f.nas ? `<span>On the NAS: <code>${esc(f.nas)}</code></span><button data-folder="remove">Remove from NAS</button>`
            : '<button class="primary" data-folder="nas">Put the folder on the NAS</button>'}
          <a class="btn" href="/api/jobs/${encodeURIComponent(id)}/disc-folder.zip" download>Download zip</a>
        </div>
        <p class="muted small">The NAS copy takes almost no space. The zip downloads as roughly ${esc(fmtSize(f.bytes * 0.0013))} (about two minutes to download for a 75 GB disc), but its video files are placeholders that unzip to ${esc(fmtSize(f.bytes))} of zeros: delete the folder once the disc is added.</p>`;
    } else if (f.waiting) {
      body = '<p><strong>Put the disc in the drive now.</strong> It is read (a few seconds, no rip) and ejected.</p>';
    } else {
      body = `<div class="row actions-row"><button data-folder="read">Read from disc</button></div>
        <p class="muted small">The disc's small files were not kept${f.files ? '' : ' (ripped before this was added)'}. Click, then put the disc in: it is read and ejected, not ripped again.</p>`;
    }
    return `<div class="card"><h2>Add this disc to TheDiscDB</h2>
      <p class="small">On thediscdb.com, <em>Contribute → Add Disc</em> asks you to <em>Select Disc Root Folder</em>: pick the <code>${esc(f.name)}</code> folder (the one holding <code>BDMV</code>). Then, on the disc's page, upload the disc manifest under <em>Advanced</em>.</p>
      ${body}</div>`;
  };

  let refresh;
  const renderDetail = id => {
    clearTimeout(refresh);
    const view = $('#detail-view');
    if (!id) { view.innerHTML = '<div class="card empty">Choose a disc</div>'; return; }
    let folder = null;
    const loadFolder = () => fetch(`/api/jobs/${encodeURIComponent(id)}/disc-folder`).then(r => r.ok ? r.json() : null).then(f => { folder = f; }).catch(() => {});
    fetch(`/api/jobs/${encodeURIComponent(id)}`).then(r => r.json()).then(j => Promise.all([loadFiles(id), loadFolder()]).then(() => j)).then(j => {
      if (j.error) { view.innerHTML = `<div class="card empty">${esc(j.error)}</div>`; return; }
      const discdb = j.hash_matched ? 'Matched: this exact disc, by its content hash' : j.content_hash ? 'No disc with this content hash' : '';
      const ids = [['Disc label', j.label], ['Fingerprint', j.fingerprint], ['Content hash', j.content_hash], ['TheDiscDB', discdb], ['Catalogue', j.catalog], ['Backup', j.backup && j.backup.path], ['Drive', j.drive], ['Disc', j.disc_type],
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
        <dl class="ids">${ids.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${k === 'Content hash' ? `<code>${esc(v)}</code> <button class="small" data-copy="${esc(v)}">Copy</button>` : esc(v)}</dd>`).join('')}</dl>
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
      ${terminal(j.stage) ? folderCard(id, folder) : ''}
      ${filesCard()}
      ${(j.log || []).length ? `<div class="card"><h2>Log</h2><pre class="log">${esc(j.log.map(l => `${new Date(l.time).toLocaleTimeString()}  ${l.message}`).join('\n'))}</pre></div>` : ''}`;
      const det = $('#files');
      if (det) det.ontoggle = () => { files.open = det.open; };
      view.querySelectorAll('button[data-copy]').forEach(b => b.onclick = () => {
        navigator.clipboard.writeText(b.dataset.copy).then(() => { b.textContent = 'Copied'; }, () => { b.textContent = 'Select it to copy'; });
      });
      view.querySelectorAll('button[data-folder]').forEach(b => b.onclick = () => {
        const what = b.dataset.folder;
        const url = `/api/jobs/${encodeURIComponent(id)}/disc-folder/${what === 'read' ? 'read' : 'nas'}`;
        b.disabled = true;
        if (what === 'nas') b.textContent = 'Writing…';
        fetch(url, { method: what === 'remove' ? 'DELETE' : 'POST' }).then(r => r.json()).then(res => {
          if (res.error) alert(res.error);
        }).finally(() => renderDetail(id));
      });
      // A disc still being ripped, or awaited for its folder: keep it current.
      if (!terminal(j.stage) || (folder && folder.waiting)) refresh = setTimeout(() => { if (decodeURIComponent(location.hash.slice(1)) === id) renderDetail(id); }, 5000);
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
  // Discs being ripped right now head the list.
  Promise.all([
    fetch('/api/history?limit=500').then(r => r.json()).catch(() => []),
    fetch('/api/status').then(r => r.json()).catch(() => ({})),
  ]).then(([h, s]) => {
    const live = ((s && s.drives) || []).map(d => d.job).filter(j => j && !terminal(j.stage));
    const done = Array.isArray(h) ? h : [];
    all = [...live, ...done.filter(j => !live.some(l => l.id === j.id))];
    route();
  });
})();
