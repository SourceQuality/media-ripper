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
      $('#detail-log').textContent = (j.log || []).map(l => `${new Date(l.time).toLocaleTimeString()}  ${l.message}`).join('\n') || '—';
      $('#detail').showModal();
    }),
  };
  $('#detail-close').onclick = () => $('#detail').close();

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

  const driveCard = d => {
    const j = d.job && !terminal(d.job.stage) ? d.job : null;
    let body;
    if (j) {
      // Ripping: progress across all titles. Afterwards: titles delivered.
      const pct = j.stage === 'ripping' && j.progress >= 0 ? j.overall
        : ['postprocessing', 'delivering'].includes(j.stage) && j.total ? (j.outputs || []).length / j.total * 100 : -1;
      body = `<div class="row"><span class="title">${esc(jobTitle(j))}</span><span class="stage">${esc(stageText(j))}</span><span class="spacer"></span>
        <button class="danger" onclick="act.cancel('${esc(j.id)}')">Cancel</button></div>
        <div class="bar ${pct < 0 ? 'indeterminate' : ''}"><div style="width:${pct < 0 ? 0 : pct}%"></div></div>
        ${(j.activities || []).map(activity).join('')}`;
    } else {
      const status = d.status === 'disc-ok' ? (d.ignored ? 'Disc already ripped' : (d.label || 'Disc inserted')) : d.status === 'tray-open' ? 'Tray open' : d.status === 'no-disc' ? 'Empty' : d.status;
      const last = d.job ? `<span class="stage ${esc(d.job.stage)}">${esc(jobTitle(d.job))} · ${esc(d.job.stage)}</span>` : '';
      body = `<div class="row"><span class="title">${esc(status)}</span>${last}<span class="spacer"></span>
        ${d.status === 'disc-ok' ? `<button onclick="act.rescan('${esc(d.path)}')">Rip</button><button onclick="act.eject('${esc(d.path)}')">Eject</button>` : ''}</div>
        ${d.last_error ? `<div class="stage failed">${esc(d.last_error)}</div>` : ''}`;
    }
    return `<div class="card"><div class="drive">${esc(d.path)}</div>${body}</div>`;
  };

  const fmtWhen = t => { const d = new Date(t); const today = new Date(); return d.toDateString() === today.toDateString() ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); };

  const render = s => {
    $('#version').textContent = s.version || '';
    $('#drives').innerHTML = s.drives.length ? s.drives.map(driveCard).join('') : '<div class="card empty">No optical drives</div>';
    const rows = (s.recent || []).filter(j => terminal(j.stage)).map(j => `<tr>
      <td>${esc(fmtWhen(j.finished_at || j.started_at))}</td><td class="drive">${esc(j.drive)}</td>
      <td>${esc(jobTitle(j))}${j.outputs && j.outputs.length ? `<ul class="outputs">${j.outputs.map(o => `<li>${esc(o.path)}</li>`).join('')}</ul>` : ''}${j.error ? `<div class="stage failed">${esc(j.error)}</div>` : ''}</td>
      <td class="stage ${esc(j.stage)}">${esc(j.stage)}</td><td>${esc(j.elapsed || '')}</td>
      <td class="actions"><button onclick="act.detail('${esc(j.id)}')">Log</button></td></tr>`);
    $('#recent tbody').innerHTML = rows.length ? rows.join('') : '<tr><td colspan="6" class="empty">Nothing yet</td></tr>';
  };

  let timer;
  const refresh = () => fetch('/api/status').then(r => r.json()).then(render).catch(() => {}).finally(() => { clearTimeout(timer); timer = setTimeout(refresh, document.hidden ? 10000 : 2000); });
  document.addEventListener('visibilitychange', refresh);
  refresh();
})();
