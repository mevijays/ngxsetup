'use strict';

// ---- low-level API helper ----------------------------------------------------

async function api(method, path, body) {
  const opts = { method, credentials: 'same-origin', headers: {} };
  if (method !== 'GET') opts.headers['X-Requested-With'] = 'ngxsetup-web';
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch { data = { error: text }; } }
  if (!res.ok) {
    const err = new Error((data && data.error) || ('request failed: ' + res.status));
    err.data = data;
    throw err;
  }
  return data;
}

async function apiForm(path, formData) {
  const res = await fetch(path, { method: 'POST', credentials: 'same-origin', headers: { 'X-Requested-With': 'ngxsetup-web' }, body: formData });
  const text = await res.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch { data = { error: text }; } }
  if (!res.ok) {
    const err = new Error((data && data.error) || ('request failed: ' + res.status));
    err.data = data;
    throw err;
  }
  return data;
}

// ---- small helpers --------------------------------------------------------------

function escapeHTML(s) {
  const d = document.createElement('div');
  d.textContent = s == null ? '' : String(s);
  return d.innerHTML;
}
function fmt(n, digits) {
  if (n === null || n === undefined) return '-';
  if (typeof n !== 'number') return n;
  return n.toFixed(digits === undefined ? 1 : digits);
}
function icon(name, extra) { return `<i class="fa-solid ${name} ${extra || ''}"></i>`; }
function humanBytes(n) {
  if (n === null || n === undefined || isNaN(n)) return '-';
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(1)} ${units[i]}`;
}
function badgeClass(status) {
  if (status === 'ok' || status === true) return 'badge-ok';
  if (status === 'warn') return 'badge-warn';
  if (status === 'fail' || status === false) return 'badge-fail';
  return 'badge-off';
}

// A small set of muted, colorblind-friendlyish colors reused across every
// Chart.js instance so the dashboard reads as one coherent palette rather
// than each chart picking its own.
const CHART_COLORS = {
  indigo: '#6366f1', emerald: '#10b981', amber: '#f59e0b', rose: '#f43f5e',
  sky: '#0ea5e9', slate: '#94a3b8', violet: '#8b5cf6', teal: '#14b8a6',
};
Chart.defaults.font.family = "-apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif";
Chart.defaults.font.size = 12;
Chart.defaults.color = '#64748b';
Chart.defaults.plugins.legend.labels.boxWidth = 12;
Chart.defaults.plugins.legend.labels.usePointStyle = true;

// ---- toast notifications -----------------------------------------------------

let toastWrap = null;
function toast(message, kind) {
  if (!toastWrap) {
    toastWrap = document.createElement('div');
    toastWrap.className = 'fixed bottom-6 right-6 flex flex-col gap-2 z-50';
    document.body.appendChild(toastWrap);
  }
  const el = document.createElement('div');
  el.className = 'toast' + (kind ? ' ' + kind : '');
  el.textContent = message;
  toastWrap.appendChild(el);
  setTimeout(() => el.remove(), 6000);
}

// ---- copy-to-clipboard buttons -------------------------------------------------
//
// Used wherever a value (an SSH public key, a suggested command) is shown
// for the operator to paste elsewhere rather than to read — select-and-copy
// works too, but a button is one click instead of a fiddly triple-click on
// text that wraps across lines. `data-copy-target` names the id of the
// element holding the literal text to copy, so callers never have to fight
// HTML-escaping a value into a data attribute.
function bindCopyButtons(root) {
  root.querySelectorAll('.copy-btn[data-copy-target]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const el = document.getElementById(btn.dataset.copyTarget);
      if (!el) return;
      const text = el.textContent.trim();
      try {
        await navigator.clipboard.writeText(text);
        toast('Copied to clipboard', 'ok');
      } catch (err) {
        toast('Could not copy automatically — select and copy it manually', 'err');
      }
    });
  });
}

// ---- confirmation modal -------------------------------------------------------

// Resolves to `false` on cancel, or `{ ok: true, values }` on confirm —
// `values` maps every [id] element inside `body` to its value at the moment
// OK was clicked, read before the modal (and those elements) leave the DOM.
function confirmModal({ title, body, confirmLabel, danger, requireText }) {
  return new Promise((resolve) => {
    const backdrop = document.createElement('div');
    backdrop.className = 'modal-backdrop';
    backdrop.innerHTML = `
      <div class="modal-box">
        <h3 class="text-base font-semibold text-slate-900 mb-2">${escapeHTML(title)}</h3>
        <div class="text-sm text-slate-600">${body}</div>
        ${requireText ? `<label class="field-label mt-3">Type <code class="chip">${escapeHTML(requireText)}</code> to confirm
          <input type="text" id="modal-confirm-text" autocomplete="off" class="field-input mt-1"></label>` : ''}
        <div class="flex justify-end gap-2 mt-5">
          <button id="modal-cancel" class="btn">Cancel</button>
          <button id="modal-ok" class="btn ${danger ? 'btn-danger-solid' : 'btn-primary'}">${escapeHTML(confirmLabel || 'Confirm')}</button>
        </div>
      </div>`;
    document.body.appendChild(backdrop);
    const okBtn = backdrop.querySelector('#modal-ok');
    const input = backdrop.querySelector('#modal-confirm-text');
    if (requireText) {
      okBtn.disabled = true;
      input.addEventListener('input', () => { okBtn.disabled = input.value !== requireText; });
    }
    backdrop.querySelector('#modal-cancel').onclick = () => { backdrop.remove(); resolve(false); };
    okBtn.onclick = () => {
      const values = {};
      backdrop.querySelectorAll('[id]').forEach((el) => {
        if (el.tagName === 'INPUT' && el.type === 'checkbox') values[el.id] = el.checked;
        else if ('value' in el) values[el.id] = el.value;
      });
      backdrop.remove();
      resolve({ ok: true, values });
    };
  });
}

// ---- navigation -----------------------------------------------------------------

const views = {}; // name -> render(container, param) function
let currentCleanup = null;

function switchView(name, param) {
  if (currentCleanup) { currentCleanup(); currentCleanup = null; }
  document.querySelectorAll('.nav-item[data-view]').forEach((btn) => {
    btn.classList.toggle('active', btn.dataset.view === name);
  });
  const container = document.getElementById('content');
  container.innerHTML = '<p class="text-slate-400 text-sm">Loading…</p>';
  const fn = views[name];
  if (!fn) { container.innerHTML = '<p>Unknown view.</p>'; return; }
  Promise.resolve(fn(container, param)).then((cleanup) => {
    if (typeof cleanup === 'function') currentCleanup = cleanup;
  }).catch((err) => {
    container.innerHTML = `<div class="card"><p class="text-rose-600 text-sm">${escapeHTML(err.message)}</p></div>`;
  });
}

document.querySelectorAll('.nav-item[data-view]').forEach((btn) => {
  btn.addEventListener('click', () => switchView(btn.dataset.view));
});

// ---- version / maintainer footer -----------------------------------------------

// Populated from the server rather than hard-coded twice: internal/build is
// the single source of truth for these three values (see that package's doc
// comment), and this keeps the footer honest about which binary is actually
// running instead of whatever text happened to be in index.html at the time
// it was written.
(async () => {
  try {
    const v = await api('GET', '/api/version');
    const badge = document.getElementById('app-version-badge');
    if (badge && v.version) badge.textContent = v.version;
    const maintainer = document.getElementById('app-maintainer');
    if (maintainer && v.maintainer) maintainer.innerHTML = `<i class="fa-solid fa-user mr-1"></i>Maintained by ${escapeHTML(v.maintainer)}`;
    const repoLink = document.getElementById('app-repo-link');
    if (repoLink && v.repo_url) {
      repoLink.href = v.repo_url;
      repoLink.innerHTML = `<i class="fa-solid fa-code-branch mr-1"></i>${escapeHTML(v.repo_url.replace(/^https?:\/\//, ''))}`;
    }
  } catch {
    // Footer already has static fallback text; a failed fetch here is not
    // worth surfacing to the operator.
  }
})();

// A small helper every "run an action, show a transcript" panel shares.
function actionPanel(container, { title, iconName, run, confirmOpts, buttonLabel, buttonClass }) {
  const card = document.createElement('div');
  card.className = 'card';
  card.innerHTML = `<div class="flex items-center justify-between mb-3">
      <h3 class="card-title mb-0">${icon(iconName || 'fa-play', '')}${escapeHTML(title)}</h3>
      <button class="btn ${buttonClass || 'btn-primary'} run-btn">${escapeHTML(buttonLabel || 'Run')}</button>
    </div>
    <div class="output-wrap hidden"><div class="output-panel"></div></div>`;
  container.appendChild(card);
  const btn = card.querySelector('.run-btn');
  const outWrap = card.querySelector('.output-wrap');
  const out = card.querySelector('.output-panel');
  btn.addEventListener('click', async () => {
    if (confirmOpts) {
      const res = await confirmModal(confirmOpts);
      if (!res) return;
    }
    btn.disabled = true;
    const originalLabel = btn.innerHTML;
    btn.innerHTML = '<span class="spinner"></span> Working…';
    outWrap.classList.remove('hidden');
    out.classList.remove('err');
    out.textContent = '';
    try {
      const result = await run();
      out.textContent = (result && result.output) || '(no output)';
      toast(title + ' finished', 'ok');
    } catch (err) {
      out.classList.add('err');
      out.textContent = ((err.data && err.data.output) ? err.data.output + '\n\n' : '') + 'Error: ' + err.message;
      toast(title + ' failed: ' + err.message, 'err');
    } finally {
      btn.disabled = false;
      btn.innerHTML = originalLabel;
    }
  });
  return card;
}

// ---- Dashboard ------------------------------------------------------------------

function parseStatusRows(rows) {
  const map = {};
  (rows || []).forEach(([k, v]) => { map[k] = v; });
  const out = {};
  const load = /^([\d.]+)\s+([\d.]+)\s+([\d.]+)\s+across\s+(\d+)\s+core.*?—\s*(\w+)/.exec(map.load || '');
  if (load) out.load = { one: +load[1], five: +load[2], fifteen: +load[3], cores: +load[4], verdict: load[5] };
  const mem = /(\d+)\s*MB used of (\d+)\s*MB\s*\((\d+)%\)/.exec(map.memory || '');
  if (mem) out.memory = { usedMB: +mem[1], totalMB: +mem[2], pct: +mem[3] };
  const disk = /(\d+)\s*MB free of (\d+)\s*MB/.exec(map.disk || '');
  if (disk) out.disk = { freeMB: +disk[1], totalMB: +disk[2], usedMB: +disk[2] - +disk[1] };
  return out;
}

function verdictColor(v) {
  if (v === 'saturated') return CHART_COLORS.rose;
  if (v === 'busy') return CHART_COLORS.amber;
  return CHART_COLORS.emerald;
}

views.dashboard = async (container) => {
  const [status, doctor] = await Promise.all([api('GET', '/api/status'), api('GET', '/api/doctor')]);
  const parsed = parseStatusRows(status.rows);

  container.innerHTML = `
    <h2 class="page-title">${icon('fa-gauge-high', 'text-indigo-500')}Dashboard</h2>
    <div class="grid grid-cols-1 lg:grid-cols-3 gap-5 mb-1">
      <div class="card">
        <h3 class="card-title">${icon('fa-weight-hanging')}Load average</h3>
        <div class="h-40"><canvas id="chart-load"></canvas></div>
      </div>
      <div class="card">
        <h3 class="card-title">${icon('fa-memory')}Memory</h3>
        <div class="h-40 flex items-center justify-center"><canvas id="chart-mem"></canvas></div>
      </div>
      <div class="card">
        <h3 class="card-title">${icon('fa-hard-drive')}Disk</h3>
        <div class="h-40 flex items-center justify-center"><canvas id="chart-disk"></canvas></div>
      </div>
    </div>
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-5">
      <div class="card">
        <h3 class="card-title">${icon('fa-circle-info')}Status</h3>
        <dl class="kv-grid">${(status.rows || []).map(([k, v]) => `<dt>${escapeHTML(k)}</dt><dd>${escapeHTML(v)}</dd>`).join('')}</dl>
      </div>
      <div class="card">
        <div class="flex items-center justify-between mb-3">
          <h3 class="card-title mb-0">${icon('fa-stethoscope')}Doctor</h3>
          <span>${doctor.failures ? `<span class="badge badge-fail">${doctor.failures} failing</span>` : ''}
          ${doctor.warnings ? `<span class="badge badge-warn">${doctor.warnings} warning${doctor.warnings === 1 ? '' : 's'}</span>` : ''}
          ${!doctor.failures && !doctor.warnings ? '<span class="badge badge-ok">all clear</span>' : ''}</span>
        </div>
        <table class="data-table"><tbody>
          ${(doctor.checks || []).map((c) => `<tr>
            <td>${escapeHTML(c.Name)}</td>
            <td><span class="badge ${badgeClass(c.Status)}">${c.Status}</span></td>
            <td class="text-slate-400">${escapeHTML(c.Detail)}</td>
          </tr>`).join('')}
        </tbody></table>
      </div>
    </div>`;

  if (parsed.load) {
    new Chart(container.querySelector('#chart-load'), {
      type: 'bar',
      data: {
        labels: ['1 min', '5 min', '15 min'],
        datasets: [{
          data: [parsed.load.one, parsed.load.five, parsed.load.fifteen],
          backgroundColor: verdictColor(parsed.load.verdict),
          borderRadius: 4,
        }],
      },
      options: {
        responsive: true, maintainAspectRatio: false,
        plugins: { legend: { display: false }, tooltip: { callbacks: { label: (c) => `${c.parsed.y} (${parsed.load.cores} cores)` } } },
        scales: { y: { beginAtZero: true, grid: { color: '#f1f5f9' } }, x: { grid: { display: false } } },
      },
    });
  }
  if (parsed.memory) {
    new Chart(container.querySelector('#chart-mem'), {
      type: 'doughnut',
      data: {
        labels: ['Used', 'Available'],
        datasets: [{ data: [parsed.memory.usedMB, parsed.memory.totalMB - parsed.memory.usedMB], backgroundColor: [CHART_COLORS.indigo, '#e2e8f0'], borderWidth: 0 }],
      },
      options: {
        responsive: true, maintainAspectRatio: false, cutout: '70%',
        plugins: { legend: { position: 'bottom' }, tooltip: { callbacks: { label: (c) => `${c.label}: ${c.parsed} MB` } } },
      },
    });
  }
  if (parsed.disk) {
    new Chart(container.querySelector('#chart-disk'), {
      type: 'doughnut',
      data: {
        labels: ['Used', 'Free'],
        datasets: [{ data: [parsed.disk.usedMB, parsed.disk.freeMB], backgroundColor: [CHART_COLORS.violet, '#e2e8f0'], borderWidth: 0 }],
      },
      options: {
        responsive: true, maintainAspectRatio: false, cutout: '70%',
        plugins: { legend: { position: 'bottom' }, tooltip: { callbacks: { label: (c) => `${c.label}: ${(c.parsed / 1024).toFixed(1)} GB` } } },
      },
    });
  }
};

// ---- Sites ------------------------------------------------------------------

views.sites = async (container) => {
  const { sites } = await api('GET', '/api/sites');
  container.innerHTML = `
    <h2 class="page-title">${icon('fa-globe', 'text-indigo-500')}Sites</h2>
    <div class="card">
      <h3 class="card-title">${icon('fa-circle-plus')}Add a site</h3>
      <form id="add-site-form">
        <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
          <div><label class="field-label">Domain</label><input type="text" name="domain" placeholder="example.com" required class="field-input"></div>
          <div><label class="field-label">Aliases (comma separated)</label><input type="text" name="aliases" placeholder="www.example.com" class="field-input"></div>
        </div>
        <label class="field-checkbox-row"><input type="checkbox" name="wordpress" checked> Install WordPress</label>
        <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
          <div><label class="field-label">TLS</label>
            <select name="tlsmode" class="field-input">
              <option value="none">None (plain HTTP)</option>
              <option value="letsencrypt">Let's Encrypt (DNS must point here)</option>
              <option value="self">Self-signed</option>
            </select>
          </div>
          <label class="field-checkbox-row self-end"><input type="checkbox" name="install"> Complete WordPress install now</label>
        </div>
        <div class="grid grid-cols-1 md:grid-cols-2 gap-4 install-fields hidden">
          <div><label class="field-label">Admin user</label><input type="text" name="admin_user" placeholder="admin" class="field-input"></div>
          <div><label class="field-label">Admin email</label><input type="email" name="admin_email" placeholder="you@example.com" class="field-input"></div>
        </div>
        <div class="install-fields hidden mt-3"><label class="field-label">Site title</label><input type="text" name="title" placeholder="My Site" class="field-input"></div>
        <div class="flex justify-end mt-4">
          <button type="submit" class="btn btn-primary">${icon('fa-circle-plus')}Create site</button>
        </div>
      </form>
      <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
    </div>
    <div class="card">
      <h3 class="card-title">${icon('fa-list')}Configured sites</h3>
      <table class="data-table">
        <thead><tr><th>Domain</th><th>State</th><th>TLS</th><th>Database</th><th></th></tr></thead>
        <tbody id="sites-tbody"></tbody>
      </table>
    </div>`;

  const installToggle = container.querySelector('input[name="install"]');
  installToggle.addEventListener('change', () => {
    container.querySelectorAll('.install-fields').forEach((el) => el.classList.toggle('hidden', !installToggle.checked));
  });

  renderSitesTable(container.querySelector('#sites-tbody'), sites);

  container.querySelector('#add-site-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const tlsmode = fd.get('tlsmode');
    const body = {
      domain: fd.get('domain').trim(), aliases: fd.get('aliases') || '',
      wordpress: fd.get('wordpress') === 'on', tls: tlsmode === 'letsencrypt', self_signed: tlsmode === 'self',
      install: fd.get('install') === 'on', admin_user: fd.get('admin_user') || '',
      admin_email: fd.get('admin_email') || '', title: fd.get('title') || '',
    };
    const outWrap = container.querySelector('.output-wrap');
    const out = outWrap.querySelector('.output-panel');
    outWrap.classList.remove('hidden'); out.classList.remove('err'); out.textContent = 'Working…';
    const btn = e.target.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      const result = await api('POST', '/api/sites', body);
      out.textContent = result.output || '(no output)';
      toast('Site created', 'ok');
      e.target.reset();
      switchView('sites');
    } catch (err) {
      out.classList.add('err');
      out.textContent = ((err.data && err.data.output) ? err.data.output + '\n\n' : '') + 'Error: ' + err.message;
      toast('Failed to create site: ' + err.message, 'err');
    } finally { btn.disabled = false; }
  });
};

function renderSitesTable(tbody, sites) {
  if (!sites || sites.length === 0) {
    tbody.innerHTML = '<tr><td colspan="5" class="text-slate-400">No sites configured yet.</td></tr>';
    return;
  }
  tbody.innerHTML = sites.map((s) => `
    <tr data-domain="${escapeHTML(s.domain)}">
      <td class="font-medium text-slate-900">
        <button class="hover:text-indigo-600 hover:underline" data-act="detail">${escapeHTML(s.domain)}</button>
      </td>
      <td><span class="badge ${s.enabled ? 'badge-ok' : 'badge-off'}">${s.enabled ? 'enabled' : 'disabled'}</span></td>
      <td>${escapeHTML(s.cert_source || '-')}</td>
      <td class="font-mono text-xs">${escapeHTML(s.db_name || '-')}</td>
      <td class="text-right whitespace-nowrap">
        <button class="btn btn-sm" data-act="detail">${icon('fa-chart-simple')}Activity</button>
        <button class="btn btn-sm" data-act="toggle">${s.enabled ? 'Disable' : 'Enable'}</button>
        <button class="btn btn-sm" data-act="fixperms">Fix perms</button>
        <button class="btn btn-sm btn-danger" data-act="remove">Remove</button>
      </td>
    </tr>`).join('');

  tbody.querySelectorAll('button[data-act]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const domain = btn.closest('tr').dataset.domain;
      const act = btn.dataset.act;
      if (act === 'detail') {
        switchView('siteDetail', domain);
      } else if (act === 'toggle') {
        const enabling = btn.textContent.trim() === 'Enable';
        btn.disabled = true;
        try {
          await api('POST', `/api/sites/${encodeURIComponent(domain)}/${enabling ? 'enable' : 'disable'}`);
          toast(`${domain} ${enabling ? 'enabled' : 'disabled'}`, 'ok');
          switchView('sites');
        } catch (err) { toast(err.message, 'err'); btn.disabled = false; }
      } else if (act === 'fixperms') {
        btn.disabled = true;
        try {
          await api('POST', `/api/sites/${encodeURIComponent(domain)}/fix-perms`);
          toast(`Permissions fixed for ${domain}`, 'ok');
        } catch (err) { toast(err.message, 'err'); } finally { btn.disabled = false; }
      } else if (act === 'remove') {
        const res = await confirmModal({
          title: `Remove ${domain}`,
          body: `<p>Choose what to delete. Leaving both unchecked disconnects the site from nginx/PHP but keeps its files and database.</p>
            <label class="field-checkbox-row"><input type="checkbox" id="purge-files"> Delete files and system user</label>
            <label class="field-checkbox-row"><input type="checkbox" id="purge-db"> Delete database</label>`,
          confirmLabel: 'Remove', danger: true, requireText: domain,
        });
        if (!res) return;
        btn.disabled = true;
        try {
          await api('DELETE', `/api/sites/${encodeURIComponent(domain)}`, {
            purge_files: !!res.values['purge-files'], purge_db: !!res.values['purge-db'], confirm_domain: domain,
          });
          toast(`${domain} removed`, 'ok');
          switchView('sites');
        } catch (err) { toast(err.message, 'err'); btn.disabled = false; }
      }
    });
  });
}

// ---- Site migration ---------------------------------------------------------

views.migrate = (container) => {
  container.innerHTML = `
    <h2 class="page-title">${icon('fa-right-left', 'text-indigo-500')}Site Migration</h2>
    <div class="card">
      <h3 class="card-title">${icon('fa-server')}Connect to a remote server</h3>
      <p class="text-xs text-slate-500 mb-3">Reads /etc/nginx/sites-enabled and each vhost's wp-config.php over SSH. Nothing on the remote server is changed by this step — nothing here is migrated until you select sites below and start the migration.</p>
      <form id="migrate-connect-form">
        <div class="grid grid-cols-1 md:grid-cols-3 gap-4">
          <div class="md:col-span-2"><label class="field-label">Host</label>
            <input type="text" name="host" placeholder="203.0.113.10 or old-server.example.com" required class="field-input"></div>
          <div><label class="field-label">Port</label><input type="number" name="port" value="22" min="1" max="65535" class="field-input"></div>
        </div>
        <div class="mt-3"><label class="field-label">SSH user</label>
          <input type="text" name="user" value="root" required class="field-input max-w-sm">
          <p class="text-xs text-slate-400 mt-1">A non-root user needs passwordless (NOPASSWD) sudo on the remote host.</p>
        </div>
        <div class="mt-3"><label class="field-label">Private key</label>
          <textarea name="private_key" rows="6" required class="field-input font-mono text-xs" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----&#10;...&#10;-----END OPENSSH PRIVATE KEY-----"></textarea>
          <p class="text-xs text-slate-400 mt-1">Used only for this session and never written anywhere but a temporary, mode-0600 file removed as soon as this migration finishes.</p>
        </div>
        <button type="submit" class="btn btn-primary mt-3">${icon('fa-plug')}Connect &amp; discover sites</button>
      </form>
      <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
    </div>
    <div id="migrate-sites-card"></div>
    <div id="migrate-progress-card"></div>`;

  const connectForm = container.querySelector('#migrate-connect-form');
  connectForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(connectForm);
    const body = {
      host: fd.get('host').trim(), port: parseInt(fd.get('port'), 10) || 22,
      user: fd.get('user').trim(), private_key: fd.get('private_key'),
    };
    const outWrap = connectForm.closest('.card').querySelector('.output-wrap');
    const out = outWrap.querySelector('.output-panel');
    outWrap.classList.remove('hidden'); out.classList.remove('err');
    out.textContent = 'Connecting — this can take a few seconds, longer if the connection needs to retry…';
    const btn = connectForm.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      const result = await api('POST', '/api/migrate/discover', body);
      out.textContent = `Found ${result.sites.length} vhost(s).`;
      renderMigrateSites(container.querySelector('#migrate-sites-card'), result.sites);
    } catch (err) {
      out.classList.add('err');
      out.textContent = 'Error: ' + err.message;
      toast('Discovery failed: ' + err.message, 'err');
    } finally { btn.disabled = false; }
  });

  let stopped = false;
  pollMigrateStatus(container.querySelector('#migrate-progress-card'), () => stopped);
  return () => { stopped = true; };
};

function renderMigrateSites(container, sites) {
  if (!sites || !sites.length) {
    container.innerHTML = `<div class="card"><p class="text-slate-400 text-sm">No vhosts found on that server's /etc/nginx/sites-enabled.</p></div>`;
    return;
  }
  container.innerHTML = `
    <div class="card">
      <h3 class="card-title">${icon('fa-list-check')}Discovered sites</h3>
      <div class="overflow-x-auto">
      <table class="data-table">
        <thead><tr><th></th><th>Domain</th><th>Document root</th><th>Database</th><th>Status</th></tr></thead>
        <tbody>
          ${sites.map((s) => `<tr data-domain="${escapeHTML(s.domain)}">
            <td><input type="checkbox" class="migrate-site-check" ${s.migratable ? '' : 'disabled'}></td>
            <td class="font-medium text-slate-900">${escapeHTML(s.domain)}
              ${s.aliases && s.aliases.length ? `<div class="text-xs text-slate-400 font-normal">${s.aliases.map(escapeHTML).join(', ')}</div>` : ''}</td>
            <td class="font-mono text-xs">${escapeHTML(s.root || '-')}</td>
            <td class="font-mono text-xs">${s.db_name ? escapeHTML(s.db_name) : '-'}${s.db_user ? ` <span class="text-slate-400">(${escapeHTML(s.db_user)})</span>` : ''}</td>
            <td>${s.migratable
              ? `<span class="badge badge-ok">ready</span>`
              : `<span class="badge badge-off" title="${escapeHTML(s.reason)}">${escapeHTML(s.reason)}</span>`}</td>
          </tr>`).join('')}
        </tbody>
      </table>
      </div>
      <div class="grid grid-cols-1 md:grid-cols-2 gap-4 mt-4 max-w-lg">
        <div><label class="field-label">Certificate</label>
          <select id="migrate-tls" class="field-input">
            <option value="none">None (plain HTTP)</option>
            <option value="self">Self-signed</option>
            <option value="letsencrypt">Let's Encrypt (DNS must point here first)</option>
          </select>
        </div>
      </div>
      <div class="flex items-center justify-between mt-4">
        <button id="migrate-select-all" class="btn btn-sm">Select all ready sites</button>
        <button id="migrate-start" class="btn btn-primary">${icon('fa-right-left')}Migrate selected</button>
      </div>
    </div>`;

  container.querySelector('#migrate-select-all').addEventListener('click', () => {
    container.querySelectorAll('.migrate-site-check:not(:disabled)').forEach((cb) => { cb.checked = true; });
  });

  container.querySelector('#migrate-start').addEventListener('click', async () => {
    const domains = Array.from(container.querySelectorAll('.migrate-site-check:checked')).map((cb) => cb.closest('tr').dataset.domain);
    if (!domains.length) { toast('Select at least one site to migrate', 'err'); return; }
    const tlsMode = container.querySelector('#migrate-tls').value;
    const res = await confirmModal({
      title: `Migrate ${domains.length} site${domains.length === 1 ? '' : 's'}`,
      body: `<p>This creates a new site here for each domain below, restores its database and copies its files over. A site that fails partway is fully rolled back — nothing half-migrated is left registered.</p>
        <ul class="list-disc list-inside text-sm mt-2">${domains.map((d) => `<li>${escapeHTML(d)}</li>`).join('')}</ul>`,
      confirmLabel: 'Start migration',
    });
    if (!res) return;
    const btn = container.querySelector('#migrate-start');
    btn.disabled = true;
    try {
      await api('POST', '/api/migrate/start', { domains, tls: tlsMode === 'letsencrypt', self_signed: tlsMode === 'self' });
      toast('Migration started', 'ok');
      pollMigrateStatus(document.getElementById('migrate-progress-card'), () => false);
    } catch (err) {
      toast('Could not start migration: ' + err.message, 'err');
      btn.disabled = false;
    }
  });
}

function migrateStateBadge(state) {
  switch (state) {
    case 'success': return 'badge-ok';
    case 'failed': return 'badge-fail';
    case 'running': return 'badge-warn';
    default: return 'badge-off';
  }
}
function migrateBarColor(state) {
  switch (state) {
    case 'success': return 'bg-emerald-500';
    case 'failed': return 'bg-rose-500';
    default: return 'bg-indigo-500';
  }
}

function renderMigrateProgress(container, status) {
  if (!container) return;
  if (!status || (!status.running && (!status.sites || !status.sites.length))) {
    container.innerHTML = '';
    return;
  }
  container.innerHTML = `
    <div class="card">
      <div class="flex items-center justify-between mb-3">
        <h3 class="card-title mb-0">${icon('fa-spinner', status.running ? 'fa-spin' : '')}Migration progress</h3>
        ${status.running ? `<button id="migrate-cancel" class="btn btn-sm btn-danger">${icon('fa-stop')}Cancel</button>` : ''}
      </div>
      <div class="space-y-3">
        ${(status.sites || []).map((s) => `
          <div>
            <div class="flex items-center justify-between text-sm mb-1">
              <span class="font-medium text-slate-900">${escapeHTML(s.domain)}</span>
              <span class="badge ${migrateStateBadge(s.state)}">${escapeHTML(s.state)}</span>
            </div>
            <div class="text-xs text-slate-500 mb-1">${escapeHTML(s.step || '')}${s.error ? ` — ${escapeHTML(s.error)}` : ''}</div>
            <div class="w-full bg-slate-200 rounded-full h-2 overflow-hidden">
              <div class="h-2 rounded-full ${migrateBarColor(s.state)}" style="width:${s.percent || 0}%"></div>
            </div>
          </div>`).join('')}
      </div>
      <h4 class="text-xs font-semibold text-slate-500 uppercase tracking-wide mt-4 mb-2">Log</h4>
      <div class="log-panel max-h-64" id="migrate-log">${(status.log || []).map(escapeHTML).join('\n')}</div>
    </div>`;
  const log = container.querySelector('#migrate-log');
  if (log) log.scrollTop = log.scrollHeight;
  const cancelBtn = container.querySelector('#migrate-cancel');
  if (cancelBtn) cancelBtn.addEventListener('click', async () => {
    cancelBtn.disabled = true;
    try { await api('POST', '/api/migrate/cancel', {}); toast('Cancelling…', 'ok'); } catch (err) { toast(err.message, 'err'); }
  });
}

// pollMigrateStatus polls /api/migrate/status every couple of seconds for
// as long as a job is running (or, called once from view setup, just to
// show whatever the last job left behind — e.g. the operator navigated
// away mid-migration and came back). isStopped lets the view's own cleanup
// end the loop when the operator leaves this page while a poll is still
// scheduled.
function pollMigrateStatus(container, isStopped) {
  if (!container) return;
  async function tick() {
    if (isStopped()) return;
    try {
      const status = await api('GET', '/api/migrate/status');
      if (isStopped()) return;
      renderMigrateProgress(container, status);
      if (status.running) setTimeout(tick, 2000);
    } catch {
      // A transient poll failure is not worth surfacing as a toast on top
      // of whatever the log panel already shows; just stop this loop.
    }
  }
  tick();
}

// ---- Site activity detail ----------------------------------------------------

views.siteDetail = (container, domain) => {
  if (!domain) { switchView('sites'); return; }
  container.innerHTML = `
    <button id="back-btn" class="text-sm text-indigo-600 hover:underline mb-3">${icon('fa-arrow-left')} Back to Sites</button>
    <h2 class="page-title">${icon('fa-chart-simple', 'text-indigo-500')}${escapeHTML(domain)}</h2>
    <div id="activity-body"><p class="text-slate-400 text-sm">Loading…</p></div>`;
  container.querySelector('#back-btn').addEventListener('click', () => switchView('sites'));

  let stopped = false;
  let ipChart = null, geoChart = null;
  const body = container.querySelector('#activity-body');

  async function tick(first) {
    if (stopped) return;
    try {
      const a = await api('GET', `/api/sites/${encodeURIComponent(domain)}/activity`);
      if (first) {
        body.innerHTML = `
          <div class="grid grid-cols-2 md:grid-cols-4 gap-4 mb-5">
            <div class="card !mb-0"><div class="text-xs text-slate-500">Active PHP workers</div><div class="text-2xl font-semibold text-slate-900">${a.workers ?? '-'}<span class="text-sm text-slate-400 font-normal">/${a.max_workers ?? '-'}</span></div></div>
            <div class="card !mb-0"><div class="text-xs text-slate-500">Requests/sec</div><div class="text-2xl font-semibold text-slate-900">${fmt(a.req_per_sec, 2) ?? '-'}</div></div>
            <div class="card !mb-0"><div class="text-xs text-slate-500">Cache hit rate</div><div class="text-2xl font-semibold text-slate-900">${a.cache_hit_pct >= 0 ? fmt(a.cache_hit_pct) + '%' : '-'}</div></div>
            <div class="card !mb-0"><div class="text-xs text-slate-500">Distinct IPs seen</div><div class="text-2xl font-semibold text-slate-900">${a.distinct_ips}</div></div>
          </div>
          <p class="text-xs text-slate-400 mb-4">From the last ${a.sample_lines} access-log line(s) — a bounded sample, not the whole file, refreshed every few seconds.</p>
          <div class="grid grid-cols-1 lg:grid-cols-2 gap-5">
            <div class="card">
              <h3 class="card-title">${icon('fa-network-wired')}Top requesting IPs</h3>
              <div class="h-64"><canvas id="chart-ips"></canvas></div>
            </div>
            <div class="card">
              <h3 class="card-title">${icon('fa-earth-americas')}Visitor geography</h3>
              <div id="geo-body" class="h-64 flex items-center justify-center"></div>
            </div>
          </div>`;

        const ipCtx = body.querySelector('#chart-ips');
        ipChart = new Chart(ipCtx, {
          type: 'bar',
          data: { labels: [], datasets: [{ data: [], backgroundColor: CHART_COLORS.sky, borderRadius: 4 }] },
          options: {
            indexAxis: 'y', responsive: true, maintainAspectRatio: false,
            plugins: { legend: { display: false } },
            scales: { x: { beginAtZero: true, grid: { color: '#f1f5f9' } }, y: { grid: { display: false } } },
          },
        });
      }

      if (ipChart) {
        const top = (a.top_ips || []).slice(0, 10);
        ipChart.data.labels = top.map((r) => r.ip);
        ipChart.data.datasets[0].data = top.map((r) => r.count);
        ipChart.update('none');
      }

      const geoBody = body.querySelector('#geo-body');
      if (geoBody) {
        if (!a.geo || !a.geo.enabled) {
          geoBody.innerHTML = `<div class="text-center text-sm text-slate-400 px-4">
            ${icon('fa-earth-americas', 'text-3xl mb-2 text-slate-300')}<br>
            Geo lookup not configured.<br>
            <span class="text-xs">Set <code class="chip">geoip_database_path</code> in Config to a MaxMind GeoLite2-Country .mmdb file to enable this.</span>
          </div>`;
        } else if (a.geo.countries && a.geo.countries.length) {
          if (!geoBody.querySelector('canvas')) {
            geoBody.innerHTML = '<canvas></canvas>';
            geoChart = new Chart(geoBody.querySelector('canvas'), {
              type: 'pie',
              data: { labels: [], datasets: [{ data: [], backgroundColor: Object.values(CHART_COLORS) }] },
              options: { responsive: true, maintainAspectRatio: false, plugins: { legend: { position: 'right' } } },
            });
          }
          const top = a.geo.countries.slice(0, 8);
          geoChart.data.labels = top.map((c) => c.country);
          geoChart.data.datasets[0].data = top.map((c) => c.count);
          geoChart.update('none');
        } else {
          geoBody.innerHTML = `<div class="text-center text-sm text-slate-400">No geolocatable traffic in the current sample.</div>`;
        }
      }
    } catch (err) {
      if (first) body.innerHTML = `<p class="text-rose-600 text-sm">${escapeHTML(err.message)}</p>`;
    }
    if (!stopped) setTimeout(() => tick(false), 5000);
  }
  tick(true);

  return () => { stopped = true; };
};

// ---- Live Stats -----------------------------------------------------------------

// pushRolling appends one tick to a single- or multi-dataset line chart and
// keeps a fixed rolling window — the same "last 30 points" shape every
// rolling chart on this page shares (chart-cpu, chart-nginx, chart-db, and
// the per-site chart-reqs below, which keeps its own version of this because
// it also has to grow new datasets as sites appear).
function pushRolling(chart, label, values) {
  chart.data.labels.push(String(label));
  if (chart.data.labels.length > 30) chart.data.labels.shift();
  chart.data.datasets.forEach((ds, i) => {
    ds.data.push(values[i]);
    if (ds.data.length > 30) ds.data.shift();
  });
  chart.update('none');
}

// verdictFromLoad mirrors provision.Status()'s own load-average verdict
// (load1 > cores*2 => saturated, load1 > cores => busy) so this page's
// coloring agrees with the Dashboard's.
function verdictFromLoad(load1, cores) {
  if (!cores) return 'normal';
  if (load1 > cores * 2) return 'saturated';
  if (load1 > cores) return 'busy';
  return 'normal';
}

function rollingLineChart(canvas, label, color) {
  return new Chart(canvas, {
    type: 'line',
    data: { labels: [], datasets: [{ label, data: [], borderColor: color, backgroundColor: color, tension: 0.3, pointRadius: 0, borderWidth: 2, fill: false }] },
    options: {
      responsive: true, maintainAspectRatio: false, animation: false,
      plugins: { legend: { display: false } },
      scales: { y: { beginAtZero: true, grid: { color: '#f1f5f9' } }, x: { grid: { display: false } } },
    },
  });
}

views.stats = (container) => {
  container.innerHTML = `
    <h2 class="page-title">${icon('fa-chart-line', 'text-indigo-500')}Live Stats</h2>

    <div class="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-5">
      <div class="card !mb-0"><div class="text-xs text-slate-500">CPU</div><div class="text-2xl font-semibold text-slate-900" id="stat-cpu">-</div></div>
      <div class="card !mb-0"><div class="text-xs text-slate-500">Memory</div><div class="text-2xl font-semibold text-slate-900" id="stat-mem">-</div></div>
      <div class="card !mb-0"><div class="text-xs text-slate-500">Disk (${icon('fa-hard-drive', 'text-xs')} /var)</div><div class="text-2xl font-semibold text-slate-900" id="stat-disk">-</div></div>
      <div class="card !mb-0"><div class="text-xs text-slate-500">Load average</div><div class="text-2xl font-semibold text-slate-900" id="stat-load">-</div></div>
    </div>

    <div class="grid grid-cols-1 lg:grid-cols-3 gap-5 mb-5">
      <div class="card lg:col-span-2">
        <h3 class="card-title">${icon('fa-microchip')}CPU usage</h3>
        <div class="h-48"><canvas id="chart-cpu"></canvas></div>
      </div>
      <div class="card">
        <h3 class="card-title">${icon('fa-weight-hanging')}Load average</h3>
        <div class="h-48"><canvas id="chart-load"></canvas></div>
      </div>
    </div>

    <div class="grid grid-cols-1 lg:grid-cols-2 gap-5 mb-5">
      <div class="card">
        <h3 class="card-title">${icon('fa-memory')}Memory</h3>
        <div class="h-40 flex items-center justify-center"><canvas id="chart-mem"></canvas></div>
      </div>
      <div class="card">
        <h3 class="card-title">${icon('fa-hard-drive')}Disk</h3>
        <div class="h-40 flex items-center justify-center"><canvas id="chart-disk"></canvas></div>
      </div>
    </div>

    <div class="grid grid-cols-1 lg:grid-cols-2 gap-5 mb-5">
      <div class="card">
        <div class="flex items-center justify-between mb-3">
          <h3 class="card-title mb-0">${icon('fa-server')}nginx</h3>
          <span id="nginx-status-badge"></span>
        </div>
        <div class="h-36 mb-3"><canvas id="chart-nginx"></canvas></div>
        <dl class="kv-grid" id="nginx-kv"></dl>
      </div>
      <div class="card">
        <div class="flex items-center justify-between mb-3">
          <h3 class="card-title mb-0">${icon('fa-database')}Database</h3>
          <span id="db-status-badge"></span>
        </div>
        <div class="h-36 mb-3"><canvas id="chart-db"></canvas></div>
        <dl class="kv-grid" id="db-kv"></dl>
      </div>
    </div>

    <h2 class="page-title !mb-3">${icon('fa-globe', 'text-indigo-500')}Per-site</h2>
    <div class="card">
      <div class="h-56 mb-5"><canvas id="chart-reqs"></canvas></div>
      <div class="overflow-x-auto">
      <table class="data-table">
        <thead><tr>
          <th>Domain</th><th>CPU%</th><th>Mem MB</th><th>Workers</th><th>Req/s</th><th>Cache hit%</th><th>DB MB</th>
          <th>FPM queue</th><th>Max children hit</th><th>Slow reqs</th>
        </tr></thead>
        <tbody id="stats-tbody"><tr><td colspan="10" class="text-slate-400">Loading…</td></tr></tbody>
      </table>
      </div>
    </div>`;

  const tbody = container.querySelector('#stats-tbody');
  const statCpu = container.querySelector('#stat-cpu');
  const statMem = container.querySelector('#stat-mem');
  const statDisk = container.querySelector('#stat-disk');
  const statLoad = container.querySelector('#stat-load');
  const nginxBadge = container.querySelector('#nginx-status-badge');
  const nginxKv = container.querySelector('#nginx-kv');
  const dbBadge = container.querySelector('#db-status-badge');
  const dbKv = container.querySelector('#db-kv');
  let stopped = false;

  const reqChart = new Chart(container.querySelector('#chart-reqs'), {
    type: 'line',
    data: { labels: [], datasets: [] },
    options: {
      responsive: true, maintainAspectRatio: false, animation: false,
      plugins: { legend: { position: 'bottom' } },
      scales: { y: { beginAtZero: true, grid: { color: '#f1f5f9' } }, x: { grid: { display: false } } },
    },
  });
  const palette = Object.values(CHART_COLORS);
  const series = {}; // domain -> dataset index

  const cpuChart = rollingLineChart(container.querySelector('#chart-cpu'), 'CPU %', CHART_COLORS.rose);
  cpuChart.options.scales.y.max = 100;
  const nginxChart = rollingLineChart(container.querySelector('#chart-nginx'), 'Active connections', CHART_COLORS.sky);
  const dbChart = rollingLineChart(container.querySelector('#chart-db'), 'Queries/sec', CHART_COLORS.emerald);

  const loadChart = new Chart(container.querySelector('#chart-load'), {
    type: 'bar',
    data: { labels: ['1 min', '5 min', '15 min'], datasets: [{ data: [0, 0, 0], backgroundColor: CHART_COLORS.slate, borderRadius: 4 }] },
    options: {
      responsive: true, maintainAspectRatio: false,
      plugins: { legend: { display: false } },
      scales: { y: { beginAtZero: true, grid: { color: '#f1f5f9' } }, x: { grid: { display: false } } },
    },
  });
  const memChart = new Chart(container.querySelector('#chart-mem'), {
    type: 'doughnut',
    data: { labels: ['Used', 'Available'], datasets: [{ data: [0, 1], backgroundColor: [CHART_COLORS.indigo, '#e2e8f0'], borderWidth: 0 }] },
    options: {
      responsive: true, maintainAspectRatio: false, cutout: '70%',
      plugins: { legend: { position: 'bottom' }, tooltip: { callbacks: { label: (c) => `${c.label}: ${c.parsed} MB` } } },
    },
  });
  const diskChart = new Chart(container.querySelector('#chart-disk'), {
    type: 'doughnut',
    data: { labels: ['Used', 'Free'], datasets: [{ data: [0, 1], backgroundColor: [CHART_COLORS.violet, '#e2e8f0'], borderWidth: 0 }] },
    options: {
      responsive: true, maintainAspectRatio: false, cutout: '70%',
      plugins: { legend: { position: 'bottom' }, tooltip: { callbacks: { label: (c) => `${c.label}: ${(c.parsed / 1024).toFixed(1)} GB` } } },
    },
  });

  let tickN = 0;

  async function tick() {
    if (stopped) return;
    try {
      const [{ sites }, sys] = await Promise.all([api('GET', '/api/stats'), api('GET', '/api/system-stats')]);

      // ---- per-site table + request-rate chart (unchanged shape, three new columns) ----
      if (!sites.length) {
        tbody.innerHTML = '<tr><td colspan="10" class="text-slate-400">No enabled sites.</td></tr>';
      } else {
        tbody.innerHTML = sites.map((s) => `
          <tr>
            <td class="font-medium text-slate-900">${escapeHTML(s.domain)}</td>
            <td>${fmt(s.cpu_percent)}</td>
            <td>${s.memory_mb ?? '-'}</td>
            <td>${s.workers ?? 0}/${s.max_workers ?? 0}</td>
            <td>${fmt(s.req_per_sec, 2)}</td>
            <td>${s.cache_hit_pct !== undefined && s.cache_hit_pct >= 0 ? fmt(s.cache_hit_pct) + '%' : '-'}</td>
            <td>${s.db_size_mb ?? '-'}</td>
            <td>${s.fpm_listen_queue >= 0 ? s.fpm_listen_queue : '-'}</td>
            <td>${s.fpm_max_children_reached >= 0 ? s.fpm_max_children_reached : '-'}</td>
            <td>${s.fpm_slow_requests >= 0 ? s.fpm_slow_requests : '-'}</td>
          </tr>`).join('');
      }

      tickN++;
      reqChart.data.labels.push(String(tickN));
      if (reqChart.data.labels.length > 30) reqChart.data.labels.shift();
      sites.forEach((s) => {
        if (!(s.domain in series)) {
          const idx = reqChart.data.datasets.length;
          series[s.domain] = idx;
          reqChart.data.datasets.push({
            label: s.domain, data: [], borderColor: palette[idx % palette.length],
            backgroundColor: palette[idx % palette.length], tension: 0.3, pointRadius: 0, borderWidth: 2,
          });
        }
        const ds = reqChart.data.datasets[series[s.domain]];
        ds.data.push(s.req_per_sec);
        if (ds.data.length > 30) ds.data.shift();
      });
      reqChart.update('none');

      // ---- host-wide system stats ----
      statCpu.textContent = fmt(sys.cpu_percent) + '%';
      statMem.textContent = sys.mem_total_mb ? fmt(sys.mem_used_percent) + '%' : '-';
      statDisk.textContent = sys.disk_total_mb ? fmt(sys.disk_used_percent) + '%' : '-';
      statLoad.innerHTML = `${fmt(sys.load1, 2)} <span class="text-sm text-slate-400 font-normal">/ ${sys.cores} core${sys.cores === 1 ? '' : 's'}</span>`;

      pushRolling(cpuChart, tickN, [sys.cpu_percent]);

      loadChart.data.datasets[0].data = [sys.load1, sys.load5, sys.load15];
      loadChart.data.datasets[0].backgroundColor = verdictColor(verdictFromLoad(sys.load1, sys.cores));
      loadChart.update('none');

      if (sys.mem_total_mb) {
        memChart.data.datasets[0].data = [sys.mem_used_mb, Math.max(sys.mem_avail_mb, 0)];
        memChart.update('none');
      }
      if (sys.disk_total_mb) {
        diskChart.data.datasets[0].data = [sys.disk_used_mb, Math.max(sys.disk_total_mb - sys.disk_used_mb, 0)];
        diskChart.update('none');
      }

      if (sys.nginx_error) {
        nginxBadge.innerHTML = `<span class="badge badge-fail">unavailable</span>`;
        nginxKv.innerHTML = `<dt>Status</dt><dd class="text-rose-600">${escapeHTML(sys.nginx_error)}</dd>`;
      } else {
        nginxBadge.innerHTML = `<span class="badge badge-ok">reachable</span>`;
        nginxKv.innerHTML = `
          <dt>Active connections</dt><dd>${sys.nginx.active}</dd>
          <dt>Accepted / Handled</dt><dd>${sys.nginx.accepts} / ${sys.nginx.handled}</dd>
          <dt>Total requests</dt><dd>${sys.nginx.requests}</dd>
          <dt>Reading / Writing / Waiting</dt><dd>${sys.nginx.reading} / ${sys.nginx.writing} / ${sys.nginx.waiting}</dd>`;
        pushRolling(nginxChart, tickN, [sys.nginx.active]);
      }

      if (sys.db_error) {
        dbBadge.innerHTML = `<span class="badge badge-fail">unavailable</span>`;
        dbKv.innerHTML = `<dt>Status</dt><dd class="text-rose-600">${escapeHTML(sys.db_error)}</dd>`;
      } else {
        dbBadge.innerHTML = `<span class="badge badge-ok">reachable</span>`;
        dbKv.innerHTML = `
          <dt>Queries/sec</dt><dd>${fmt(sys.db.queries_per_sec, 1)}</dd>
          <dt>Connections</dt><dd>${sys.db.threads_connected} connected, ${sys.db.threads_running} running</dd>
          <dt>Slow queries</dt><dd>${sys.db.slow_queries}</dd>
          <dt>Max used connections</dt><dd>${sys.db.max_used_connections}</dd>
          <dt>Buffer pool hit rate</dt><dd>${sys.db.buffer_pool_hit_percent >= 0 ? fmt(sys.db.buffer_pool_hit_percent) + '%' : '-'}</dd>`;
        pushRolling(dbChart, tickN, [sys.db.queries_per_sec]);
      }
    } catch (err) {
      if (!stopped) tbody.innerHTML = `<tr><td colspan="10" class="text-rose-600">${escapeHTML(err.message)}</td></tr>`;
    }
    if (!stopped) setTimeout(tick, 3000);
  }
  tick();
  return () => { stopped = true; };
};

// ---- Log Viewer -----------------------------------------------------------------

views.logs = async (container) => {
  const { sources } = await api('GET', '/api/logs/sources');
  const byCategory = {};
  (sources || []).forEach((s) => { (byCategory[s.category] = byCategory[s.category] || []).push(s); });
  const categories = Object.keys(byCategory).sort((a, b) => (a === 'system' ? -1 : b === 'system' ? 1 : a.localeCompare(b)));

  container.innerHTML = `
    <h2 class="page-title">${icon('fa-file-lines', 'text-indigo-500')}Log Viewer</h2>
    <div class="card">
      <div class="grid grid-cols-1 md:grid-cols-4 gap-4 items-end">
        <div class="md:col-span-2">
          <label class="field-label">Log source</label>
          <select id="log-source" class="field-input">
            ${categories.map((cat) => `<optgroup label="${escapeHTML(cat === 'system' ? 'System' : cat)}">
              ${byCategory[cat].map((s) => `<option value="${escapeHTML(s.key)}" ${!s.exists ? 'disabled' : ''}>${escapeHTML(s.label)}${!s.exists ? ' (not available yet)' : ''}</option>`).join('')}
            </optgroup>`).join('')}
          </select>
        </div>
        <div>
          <label class="field-label">Mode</label>
          <select id="log-mode" class="field-input">
            <option value="snapshot">Snapshot (last N lines)</option>
            <option value="live">Live tail</option>
          </select>
        </div>
        <div id="log-lines-field">
          <label class="field-label">Lines</label>
          <input type="number" id="log-lines" value="200" min="10" max="2000" class="field-input">
        </div>
      </div>
      <div class="flex gap-2 mt-4">
        <button id="log-start" class="btn btn-primary">${icon('fa-play')}Load</button>
        <button id="log-stop" class="btn hidden">${icon('fa-stop')}Stop live tail</button>
        <label class="field-checkbox-row ml-auto"><input type="checkbox" id="log-autoscroll" checked> Auto-scroll</label>
      </div>
      <div id="log-output" class="log-panel mt-4">Select a source and click Load.</div>
    </div>`;

  const sourceSel = container.querySelector('#log-source');
  const modeSel = container.querySelector('#log-mode');
  const linesField = container.querySelector('#log-lines-field');
  const startBtn = container.querySelector('#log-start');
  const stopBtn = container.querySelector('#log-stop');
  const autoscroll = container.querySelector('#log-autoscroll');
  const out = container.querySelector('#log-output');

  modeSel.addEventListener('change', () => linesField.classList.toggle('hidden', modeSel.value === 'live'));

  let stopped = true;
  let offset = 0;

  function append(lines) {
    if (!lines || !lines.length) return;
    out.textContent += (out.textContent === 'Select a source and click Load.' ? '' : '\n') + lines.join('\n');
    if (autoscroll.checked) out.scrollTop = out.scrollHeight;
  }
  function replace(lines) {
    out.textContent = (lines || []).join('\n') || '(empty)';
    if (autoscroll.checked) out.scrollTop = out.scrollHeight;
  }

  async function liveTick() {
    if (stopped) return;
    try {
      const data = await api('GET', `/api/logs/tail?source=${encodeURIComponent(sourceSel.value)}&mode=live&offset=${offset}`);
      offset = data.offset;
      if (data.replace) replace(data.lines); else append(data.lines);
    } catch (err) {
      append(['[error: ' + err.message + ']']);
    }
    if (!stopped) setTimeout(liveTick, 2000);
  }

  startBtn.addEventListener('click', async () => {
    stopped = true; // stop any previous live loop
    out.textContent = 'Loading…';
    const lines = container.querySelector('#log-lines').value || 200;
    try {
      const data = await api('GET', `/api/logs/tail?source=${encodeURIComponent(sourceSel.value)}&mode=snapshot&lines=${lines}`);
      replace(data.lines);
      offset = data.offset;
      if (modeSel.value === 'live') {
        stopped = false;
        stopBtn.classList.remove('hidden');
        liveTick();
      }
    } catch (err) {
      out.textContent = 'Error: ' + err.message;
    }
  });
  stopBtn.addEventListener('click', () => { stopped = true; stopBtn.classList.add('hidden'); });

  return () => { stopped = true; };
};

// ---- Security -----------------------------------------------------------------

views.security = async (container) => {
  const { sites } = await api('GET', '/api/sites');
  const wpSites = sites.filter((s) => s.wordpress);
  container.innerHTML = `
    <h2 class="page-title">${icon('fa-shield-halved', 'text-indigo-500')}Security</h2>
    <div class="card">
      <label class="field-label">Target</label>
      <select id="sec-target" class="field-input max-w-sm">
        <option value="">All WordPress sites</option>
        ${wpSites.map((s) => `<option value="${escapeHTML(s.domain)}">${escapeHTML(s.domain)}</option>`).join('')}
      </select>
    </div>
    <div id="sec-panels"></div>`;
  const target = () => container.querySelector('#sec-target').value;
  const panels = container.querySelector('#sec-panels');

  actionPanel(panels, {
    title: 'Scan for malware and integrity problems', iconName: 'fa-magnifying-glass', buttonLabel: 'Run scan',
    run: () => api('POST', '/api/security/scan', { domain: target() }),
  });
  actionPanel(panels, {
    title: 'Install ClamAV (antivirus engine used by the scan above)', iconName: 'fa-download', buttonLabel: 'Install ClamAV',
    run: () => api('POST', '/api/security/install-clamav', {}),
  });

  renderPatchPanel(panels, target);
};

// renderPatchPanel replaces a single blind "Patch now" button with a
// review step: load exactly what is outdated for one site — current and
// latest version for core, every plugin, every theme — let the operator
// check only what they actually want updated, then patch just that
// selection. Only ever operates on one specific site at a time (plugin and
// theme lists are meaningless averaged across many sites), so it is
// disabled while "All WordPress sites" is selected above.
function renderPatchPanel(container, target) {
  const card = document.createElement('div');
  card.className = 'card';
  card.innerHTML = `
    <div class="flex items-center justify-between mb-3">
      <h3 class="card-title mb-0">${icon('fa-arrows-rotate')}Outdated core, plugins &amp; themes</h3>
      <button id="sec-load-plan" class="btn btn-primary">${icon('fa-magnifying-glass')}Check for updates</button>
    </div>
    <div id="sec-plan-body" class="text-sm text-slate-400">Select a specific site above, then click "Check for updates."</div>
    <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>`;
  container.appendChild(card);

  const body = card.querySelector('#sec-plan-body');
  const outWrap = card.querySelector('.output-wrap');
  const out = outWrap.querySelector('.output-panel');

  card.querySelector('#sec-load-plan').addEventListener('click', async () => {
    const domain = target();
    if (!domain) { toast('Choose a specific site above — plugin and theme updates are per-site', 'err'); return; }
    body.innerHTML = '<p class="text-slate-400 text-sm">Checking wp-cli for outdated core, plugins and themes…</p>';
    outWrap.classList.add('hidden');
    try {
      const plan = await api('GET', `/api/security/patch-plan?domain=${encodeURIComponent(domain)}`);
      renderPatchPlan(body, domain, plan, outWrap, out);
    } catch (err) {
      body.innerHTML = `<p class="text-rose-600 text-sm">${escapeHTML(err.message)}</p>`;
    }
  });
}

function renderPatchPlan(body, domain, plan, outWrap, out) {
  const hasCore = !!plan.core_latest;
  const plugins = plan.plugins || [];
  const themes = plan.themes || [];
  if (!hasCore && !plugins.length && !themes.length) {
    body.innerHTML = `<p class="text-sm text-emerald-700"><i class="fa-solid fa-circle-check mr-1"></i>${escapeHTML(domain)} is already up to date.</p>`;
    return;
  }

  const row = (type, name, title, current, latest, cls) => `
    <tr>
      <td><input type="checkbox" class="${cls}" data-name="${escapeHTML(name)}" checked></td>
      <td>${escapeHTML(type)}</td>
      <td class="font-medium text-slate-900">${escapeHTML(title || name)}</td>
      <td class="font-mono text-xs">${escapeHTML(current || '-')}</td>
      <td class="font-mono text-xs text-indigo-600">${escapeHTML(latest || '-')}</td>
    </tr>`;

  body.innerHTML = `
    <div class="overflow-x-auto">
    <table class="data-table">
      <thead><tr><th></th><th>Type</th><th>Name</th><th>Current</th><th>Latest</th></tr></thead>
      <tbody>
        ${hasCore ? row('Core', 'core', 'WordPress core', plan.core_current, plan.core_latest, 'sec-item-core') : ''}
        ${plugins.map((p) => row('Plugin', p.name, p.title, p.version, p.update_version, 'sec-item-plugin')).join('')}
        ${themes.map((t) => row('Theme', t.name, t.title, t.version, t.update_version, 'sec-item-theme')).join('')}
      </tbody>
    </table>
    </div>
    <div class="flex items-center justify-between mt-4">
      <button id="sec-select-all" class="btn btn-sm">Select all</button>
      <button id="sec-patch-selected" class="btn btn-danger-solid">${icon('fa-arrows-rotate')}Patch selected</button>
    </div>`;

  const allChecks = () => body.querySelectorAll('.sec-item-core, .sec-item-plugin, .sec-item-theme');
  body.querySelector('#sec-select-all').addEventListener('click', () => {
    allChecks().forEach((cb) => { cb.checked = true; });
  });

  body.querySelector('#sec-patch-selected').addEventListener('click', async () => {
    const core = !!body.querySelector('.sec-item-core:checked');
    const selPlugins = Array.from(body.querySelectorAll('.sec-item-plugin:checked')).map((cb) => cb.dataset.name);
    const selThemes = Array.from(body.querySelectorAll('.sec-item-theme:checked')).map((cb) => cb.dataset.name);
    if (!core && !selPlugins.length && !selThemes.length) { toast('Select at least one item to patch', 'err'); return; }

    const items = [];
    if (core) items.push(`WordPress core -> ${plan.core_latest}`);
    selPlugins.forEach((n) => items.push(`plugin ${n}`));
    selThemes.forEach((n) => items.push(`theme ${n}`));
    const res = await confirmModal({
      title: `Patch ${domain}`,
      body: `<p>This updates the selected items only. Consider taking a database backup first.</p>
        <ul class="list-disc list-inside text-sm mt-2">${items.map((i) => `<li>${escapeHTML(i)}</li>`).join('')}</ul>`,
      confirmLabel: 'Patch selected', danger: true,
    });
    if (!res) return;

    const btn = body.querySelector('#sec-patch-selected');
    btn.disabled = true;
    outWrap.classList.remove('hidden'); out.classList.remove('err'); out.textContent = 'Working…';
    try {
      const result = await api('POST', '/api/security/patch', { domain, core, plugins: selPlugins, themes: selThemes });
      out.textContent = result.output || '(no output)';
      toast('Patch complete', 'ok');
    } catch (err) {
      out.classList.add('err');
      out.textContent = ((err.data && err.data.output) ? err.data.output + '\n\n' : '') + 'Error: ' + err.message;
      toast('Patch failed: ' + err.message, 'err');
    } finally { btn.disabled = false; }
  });
}

// ---- Backups --------------------------------------------------------------------

views.backups = async (container) => {
  const [sitesResp, backupsResp] = await Promise.all([api('GET', '/api/sites'), api('GET', '/api/backups')]);
  const sites = sitesResp.sites.filter((s) => s.db_name);
  container.innerHTML = `
    <h2 class="page-title">${icon('fa-database', 'text-indigo-500')}Backups</h2>
    <div class="card">
      <h3 class="card-title">${icon('fa-download')}Create a backup</h3>
      <label class="field-label">Site</label>
      <select id="backup-target" class="field-input max-w-sm">
        <option value="">All sites</option>
        ${sites.map((s) => `<option value="${escapeHTML(s.domain)}">${escapeHTML(s.domain)}</option>`).join('')}
      </select>
      <div class="mt-3"><button id="backup-run" class="btn btn-primary">${icon('fa-download')}Back up</button></div>
      <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
    </div>
    <div class="card">
      <h3 class="card-title">${icon('fa-clock-rotate-left')}Existing backups</h3>
      <table class="data-table">
        <thead><tr><th>File</th><th>Size</th><th>Modified (UTC)</th><th></th></tr></thead>
        <tbody id="backups-tbody"></tbody>
      </table>
    </div>
    <div class="card">
      <h3 class="card-title">${icon('fa-upload')}Restore</h3>
      <p class="text-xs text-slate-500 mb-3">Restoring overwrites the target database's current contents. A safety backup is taken first unless disabled.</p>
      <form id="restore-form">
        <label class="field-label">Site</label>
        <select name="domain" required class="field-input max-w-sm">
          <option value="" disabled selected>Choose a site</option>
          ${sites.map((s) => `<option value="${escapeHTML(s.domain)}">${escapeHTML(s.domain)}</option>`).join('')}
        </select>
        <div class="mt-3"><label class="field-label">Source</label>
          <select id="restore-source" class="field-input max-w-sm">
            <option value="upload">Upload a .sql file</option>
            <option value="existing">Use an existing backup</option>
          </select>
        </div>
        <div id="restore-upload-field" class="mt-3"><label class="field-label">.sql file</label><input type="file" name="file" accept=".sql,.sql.gz,text/plain" class="field-input"></div>
        <div id="restore-existing-field" class="hidden mt-3"><label class="field-label">Backup file</label>
          <select name="existing_path" class="field-input">
            ${(backupsResp.backups || []).map((b) => `<option value="${escapeHTML(b.path)}">${escapeHTML(b.name)} (${fmt(b.size_mb)} MB)</option>`).join('')}
          </select>
        </div>
        <label class="field-checkbox-row"><input type="checkbox" name="no_safety_backup"> Skip the safety backup</label>
        <button type="submit" class="btn btn-danger-solid mt-2">${icon('fa-triangle-exclamation')}Restore</button>
      </form>
      <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
    </div>
    <h2 class="page-title mt-8">${icon('fa-cloud-arrow-up', 'text-indigo-500')}Remote Backup (Borg)</h2>
    <div id="borg-section"></div>`;

  renderBackupsTable(container.querySelector('#backups-tbody'), backupsResp.backups);
  await renderBorgSection(container.querySelector('#borg-section'), sites);

  container.querySelector('#backup-run').addEventListener('click', async () => {
    const btn = container.querySelector('#backup-run');
    const outWrap = container.querySelectorAll('.output-wrap')[0];
    const out = outWrap.querySelector('.output-panel');
    btn.disabled = true; outWrap.classList.remove('hidden'); out.classList.remove('err'); out.textContent = 'Working…';
    try {
      const result = await api('POST', '/api/backups', { domain: container.querySelector('#backup-target').value });
      out.textContent = result.output || '(no output)';
      toast('Backup complete', 'ok');
      switchView('backups');
    } catch (err) {
      out.classList.add('err');
      out.textContent = ((err.data && err.data.output) ? err.data.output + '\n\n' : '') + 'Error: ' + err.message;
      toast('Backup failed: ' + err.message, 'err');
    } finally { btn.disabled = false; }
  });

  const sourceSelect = container.querySelector('#restore-source');
  sourceSelect.addEventListener('change', () => {
    const upload = sourceSelect.value === 'upload';
    container.querySelector('#restore-upload-field').classList.toggle('hidden', !upload);
    container.querySelector('#restore-existing-field').classList.toggle('hidden', upload);
  });

  container.querySelector('#restore-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const domain = e.target.domain.value;
    if (!domain) { toast('Choose a site', 'err'); return; }
    const res = await confirmModal({
      title: `Restore ${domain}`,
      body: `<p>This replaces every table in <strong>${escapeHTML(domain)}</strong>'s database. This cannot be undone by ngxsetup beyond the safety backup.</p>`,
      confirmLabel: 'Restore', danger: true, requireText: domain,
    });
    if (!res) return;

    const fd = new FormData(e.target);
    fd.set('confirm_domain', domain);
    if (sourceSelect.value === 'upload') fd.delete('existing_path'); else fd.delete('file');
    fd.set('no_safety_backup', e.target.no_safety_backup.checked ? 'true' : 'false');

    const outWrap = container.querySelectorAll('.output-wrap')[1];
    const out = outWrap.querySelector('.output-panel');
    outWrap.classList.remove('hidden'); out.classList.remove('err');
    out.textContent = 'Working… this can take a while for a large database.';
    const btn = e.target.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      const result = await apiForm('/api/restore', fd);
      out.textContent = result.output || '(no output)';
      toast('Restore complete', 'ok');
    } catch (err) {
      out.classList.add('err');
      out.textContent = ((err.data && err.data.output) ? err.data.output + '\n\n' : '') + 'Error: ' + err.message;
      toast('Restore failed: ' + err.message, 'err');
    } finally { btn.disabled = false; }
  });
};

// updateRepoUsageCard refreshes the "Repository usage" card's numbers from
// a fresh repo_stats payload (returned by GET /api/borg/archives alongside
// the archive list) — a no-op if the card isn't in the DOM, which happens
// when the repository is configured but not currently reachable, since the
// card is only rendered when status.stats came back on page load.
function updateRepoUsageCard(container, repoStats) {
  if (!repoStats) return;
  const occupied = container.querySelector('#borg-repo-occupied');
  const total = container.querySelector('#borg-repo-total');
  const ratio = container.querySelector('#borg-repo-ratio');
  const chunks = container.querySelector('#borg-repo-chunks');
  if (occupied) occupied.textContent = humanBytes(repoStats.unique_compressed_size);
  if (total) total.textContent = humanBytes(repoStats.total_size);
  if (ratio) ratio.textContent = `${fmt(repoStats.dedup_ratio * 100, 0)}%`;
  if (chunks) chunks.textContent = `${repoStats.unique_chunks} unique of ${repoStats.total_chunks} total`;
}

async function renderBorgSection(container, sites) {
  const status = await api('GET', '/api/borg/status');

  container.innerHTML = `
    <div class="card">
      <h3 class="card-title">${icon('fa-circle-info')}Status</h3>
      <dl class="kv-grid">
        <dt>borg installed</dt><dd><span class="badge ${status.installed ? 'badge-ok' : 'badge-off'}">${status.installed ? 'yes' : 'no'}</span></dd>
        <dt>repository configured</dt><dd><span class="badge ${status.configured ? 'badge-ok' : 'badge-off'}">${status.configured ? 'yes' : 'no'}</span></dd>
        ${status.configured ? `
        <dt>repository</dt><dd class="font-mono text-xs">${escapeHTML(status.repo)}</dd>
        <dt>reachable</dt><dd><span class="badge ${status.reachable ? 'badge-ok' : 'badge-fail'}">${status.reachable ? 'yes' : 'no'}</span></dd>
        <dt>schedule</dt><dd>${status.schedule ? escapeHTML(status.schedule) : '<span class="text-slate-400">not scheduled</span>'}</dd>
        ` : ''}
        ${status.ssh_public_key ? `
        <dt>ssh public key</dt><dd class="flex items-start gap-2">
          <span id="borg-ssh-pubkey" class="font-mono text-xs break-all">${escapeHTML(status.ssh_public_key)}</span>
          <button type="button" class="btn btn-sm copy-btn shrink-0" data-copy-target="borg-ssh-pubkey">${icon('fa-copy')}Copy</button>
        </dd>
        ${status.ssh_key_suggested_command ? `
        <dt>register on ngxborg</dt><dd class="flex items-start gap-2">
          <span id="borg-ssh-suggested-cmd" class="font-mono text-xs break-all">${escapeHTML(status.ssh_key_suggested_command)}</span>
          <button type="button" class="btn btn-sm copy-btn shrink-0" data-copy-target="borg-ssh-suggested-cmd">${icon('fa-copy')}Copy</button>
        </dd>
        ` : ''}
        ` : ''}
      </dl>
    </div>
    ${status.configured && status.stats ? `
    <div class="card">
      <h3 class="card-title">${icon('fa-database')}Repository usage</h3>
      <dl class="kv-grid">
        <dt>occupied on disk</dt><dd id="borg-repo-occupied" class="font-semibold text-slate-900">${humanBytes(status.stats.unique_compressed_size)}</dd>
        <dt>before deduplication</dt><dd id="borg-repo-total">${humanBytes(status.stats.total_size)}</dd>
        <dt>deduplication savings</dt><dd id="borg-repo-ratio">${fmt(status.stats.dedup_ratio * 100, 0)}%</dd>
        <dt>chunks</dt><dd id="borg-repo-chunks">${status.stats.unique_chunks} unique of ${status.stats.total_chunks} total</dd>
        ${status.stats.repo_id ? `<dt>repository ID</dt><dd class="font-mono text-xs break-all">${escapeHTML(status.stats.repo_id)}</dd>` : ''}
        ${status.stats.encryption ? `<dt>encryption</dt><dd>${escapeHTML(status.stats.encryption)}</dd>` : ''}
      </dl>
      <p class="text-xs text-slate-500 mt-2">"Occupied on disk" is what the repository actually costs in storage — every archive shares chunks with every other, so this is almost always far less than the archives' combined original size.</p>
    </div>
    ` : ''}
    <div class="card">
      <h3 class="card-title">${icon('fa-plug')}${status.configured ? 'Update repository' : 'Set up a repository'}</h3>
      <p class="text-xs text-slate-500 mb-3">The passphrase is never shown again after setup — leave it blank to generate a strong one, shown once.</p>
      <form id="borg-setup-form">
        <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
          <div class="md:col-span-2"><label class="field-label">Repository</label>
            <input type="text" name="repo" placeholder="/mnt/backup/ngxsetup or ssh://user@host:2222/./ngxsetup" required class="field-input" value="${escapeHTML(status.repo || '')}"></div>
          <div><label class="field-label">Encryption</label>
            <select name="encryption" class="field-input">
              <option value="repokey-blake2">repokey-blake2 (recommended)</option>
              <option value="repokey">repokey</option>
              <option value="keyfile-blake2">keyfile-blake2</option>
              <option value="keyfile">keyfile</option>
            </select></div>
          <div><label class="field-label">Compression</label>
            <select name="compression" class="field-input">
              <option value="zstd">zstd (recommended)</option>
              <option value="lz4">lz4 (fastest)</option>
              <option value="zlib">zlib</option>
              <option value="none">none</option>
            </select></div>
          <div class="md:col-span-2"><label class="field-label">Passphrase (optional)</label>
            <input type="password" name="passphrase" autocomplete="new-password" class="field-input" placeholder="Leave blank to generate one"></div>
          <div class="md:col-span-2"><label class="field-label">SSH private key (optional, ssh:// repositories only)</label>
            <textarea name="ssh_private_key" rows="4" spellcheck="false" autocomplete="off" class="field-input font-mono text-xs" placeholder="Paste an existing private key to import it, or leave blank to generate a new dedicated one automatically."></textarea>
            <p class="text-xs text-slate-500 mt-1">${icon('fa-circle-info', 'text-slate-400')}A remote (ssh://) repository needs its own SSH key — this tool never reuses whichever key an interactive shell happens to have. Leave this blank and a dedicated key is generated for you; its public half is shown below once setup succeeds so you can register it on the repository server (e.g. with ngxborg's <span class="font-mono">user key add</span>).</p></div>
        </div>
        <button type="submit" class="btn btn-primary mt-3">${icon('fa-plug')}${status.configured ? 'Update' : 'Initialise repository'}</button>
      </form>
      <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
    </div>
    ${status.configured ? `
    <div class="card">
      <div class="flex items-center justify-between mb-3">
        <h3 class="card-title mb-0">${icon('fa-cloud-arrow-up')}Run a backup</h3>
      </div>
      <div class="grid grid-cols-1 md:grid-cols-2 gap-4 items-end">
        <div><label class="field-label">Site</label>
          <select id="borg-backup-target" class="field-input">
            <option value="">All sites</option>
            ${sites.map((s) => `<option value="${escapeHTML(s.domain)}">${escapeHTML(s.domain)}</option>`).join('')}
          </select>
        </div>
        <label class="field-checkbox-row"><input type="checkbox" id="borg-backup-prune"> Apply retention policy afterward</label>
      </div>
      <button id="borg-backup-run" class="btn btn-primary mt-3">${icon('fa-cloud-arrow-up')}Back up to borg</button>
      <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
    </div>
    <div class="card">
      <h3 class="card-title">${icon('fa-clock-rotate-left')}Archives</h3>
      <div class="overflow-x-auto">
      <table class="data-table">
        <thead><tr>
          <th>Archive</th><th>Time</th><th>Original</th><th>Compressed</th><th>New data</th><th>Files</th><th></th>
        </tr></thead>
        <tbody id="borg-archives-tbody"><tr><td colspan="7" class="text-slate-400">Loading…</td></tr></tbody>
      </table>
      </div>
      <div id="borg-restore-output-wrap" class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
    </div>
    <div class="card">
      <h3 class="card-title">${icon('fa-clock')}Scheduled backups</h3>
      <div class="grid grid-cols-1 md:grid-cols-2 gap-4 items-end">
        <div><label class="field-label">Frequency</label>
          <select id="borg-schedule-preset" class="field-input">
            <option value="hourly">Hourly</option>
            <option value="daily" selected>Daily at 03:00</option>
            <option value="weekly">Weekly (Sunday 03:00)</option>
            <option value="custom">Custom (systemd OnCalendar)</option>
          </select>
        </div>
        <div id="borg-schedule-custom-field" class="hidden"><label class="field-label">OnCalendar expression</label>
          <input type="text" id="borg-schedule-custom" class="field-input" placeholder="*-*-* 03:00:00"></div>
        <label class="field-checkbox-row"><input type="checkbox" id="borg-schedule-prune" checked> Apply retention policy on each run</label>
      </div>
      <div class="flex gap-2 mt-3">
        <button id="borg-schedule-enable" class="btn btn-primary">${icon('fa-check')}Enable scheduled backups</button>
        <button id="borg-schedule-disable" class="btn btn-danger" ${status.schedule ? '' : 'disabled'}>${icon('fa-xmark')}Disable</button>
      </div>
      <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
    </div>
    <div class="card">
      <h3 class="card-title">${icon('fa-clock-rotate-left')}Retention policy</h3>
      <p class="text-xs text-slate-500 mb-3">0 means "keep every archive of that granularity."</p>
      <div class="grid grid-cols-3 gap-4 max-w-md">
        <div><label class="field-label">Keep daily</label><input type="number" id="borg-keep-daily" min="0" class="field-input"></div>
        <div><label class="field-label">Keep weekly</label><input type="number" id="borg-keep-weekly" min="0" class="field-input"></div>
        <div><label class="field-label">Keep monthly</label><input type="number" id="borg-keep-monthly" min="0" class="field-input"></div>
      </div>
      <button id="borg-retention-save" class="btn mt-3">${icon('fa-floppy-disk')}Save retention policy</button>
    </div>
    ` : ''}`;

  bindCopyButtons(container);

  container.querySelector('#borg-setup-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const body = {
      repo: fd.get('repo').trim(), encryption: fd.get('encryption'),
      compression: fd.get('compression'), passphrase: fd.get('passphrase') || '',
      ssh_private_key: fd.get('ssh_private_key') || '',
    };
    const outWrap = container.querySelector('#borg-setup-form').closest('.card').querySelector('.output-wrap');
    const out = outWrap.querySelector('.output-panel');
    outWrap.classList.remove('hidden'); out.classList.remove('err'); out.textContent = 'Working… this may take a moment to reach the repository.';
    const btn = e.target.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      const result = await api('POST', '/api/borg/setup', body);
      let text = result.output || '(no output)';
      if (result.data && result.data.generated_passphrase) {
        text += `\n\nGenerated passphrase (shown once): ${result.data.generated_passphrase}`;
      }
      if (result.data && result.data.public_key) {
        text += `\n\nSSH public key: ${result.data.public_key}`;
        if (result.data.generated_ssh_key) {
          text += `\n\nThis is a newly generated key — the remote server does not know it yet. Register it there before running a backup.`;
          if (result.data.ssh_key_suggested_command) {
            text += `\nIf the remote end runs ngxborg: ${result.data.ssh_key_suggested_command}`;
          }
        }
      }
      toast('Borg repository ready', 'ok');
      // Re-render in place rather than switching views outright — a full
      // switchView('backups') would tear down #content (and this output
      // panel with it) before the operator ever got to read a one-time
      // passphrase or copy the SSH public key. Re-rendering refreshes the
      // status card (so the new key/repo state shows up) while the code
      // below restores this success message into the freshly built panel.
      await renderBorgSection(container, sites);
      const newOutWrap = container.querySelector('#borg-setup-form').closest('.card').querySelector('.output-wrap');
      const newOut = newOutWrap.querySelector('.output-panel');
      newOutWrap.classList.remove('hidden');
      newOut.textContent = text;
    } catch (err) {
      out.classList.add('err');
      let errText = ((err.data && err.data.output) ? err.data.output + '\n\n' : '') + 'Error: ' + err.message;
      // A failed setup still often means a brand new SSH key was
      // generated — this is the single most common failure on a first
      // attempt against a remote that has never seen the key yet. Show
      // it here too, not just on success, so the operator has what they
      // need to register it and retry instead of a bare error.
      const errKeyData = err.data && err.data.data;
      if (errKeyData && errKeyData.public_key) {
        errText += `\n\nSSH public key: ${errKeyData.public_key}`;
        if (errKeyData.generated_ssh_key) {
          errText += `\n\nThis is a newly generated key — the remote server does not know it yet. Register it there, then try again.`;
          if (errKeyData.ssh_key_suggested_command) {
            errText += `\nIf the remote end runs ngxborg: ${errKeyData.ssh_key_suggested_command}`;
          }
        }
      }
      out.textContent = errText;
      toast('Borg setup failed: ' + err.message, 'err');
    } finally { btn.disabled = false; }
  });

  if (!status.configured) return;

  // Pre-fill retention fields from current config.
  try {
    const cfg = await api('GET', '/api/config');
    const byKey = Object.fromEntries(cfg.rows);
    container.querySelector('#borg-keep-daily').value = byKey['borg.keep_daily'] || 0;
    container.querySelector('#borg-keep-weekly').value = byKey['borg.keep_weekly'] || 0;
    container.querySelector('#borg-keep-monthly').value = byKey['borg.keep_monthly'] || 0;
  } catch {}

  container.querySelector('#borg-retention-save').addEventListener('click', async () => {
    const btn = container.querySelector('#borg-retention-save');
    btn.disabled = true;
    try {
      await api('POST', '/api/config', { key: 'borg.keep_daily', value: container.querySelector('#borg-keep-daily').value || '0' });
      await api('POST', '/api/config', { key: 'borg.keep_weekly', value: container.querySelector('#borg-keep-weekly').value || '0' });
      await api('POST', '/api/config', { key: 'borg.keep_monthly', value: container.querySelector('#borg-keep-monthly').value || '0' });
      toast('Retention policy saved', 'ok');
    } catch (err) { toast(err.message, 'err'); } finally { btn.disabled = false; }
  });

  async function loadArchives() {
    const tbody = container.querySelector('#borg-archives-tbody');
    try {
      const { archives, repo_stats } = await api('GET', '/api/borg/archives');
      updateRepoUsageCard(container, repo_stats);
      if (!archives || !archives.length) {
        tbody.innerHTML = '<tr><td colspan="7" class="text-slate-400">No archives yet.</td></tr>';
        return;
      }
      tbody.innerHTML = archives.slice().reverse().map((a) => `<tr data-archive="${escapeHTML(a.name)}">
        <td class="font-mono text-xs">${escapeHTML(a.name)}</td>
        <td>${escapeHTML(a.time)}</td>
        <td>${humanBytes(a.original_size)}</td>
        <td>${humanBytes(a.compressed_size)}</td>
        <td title="This archive's own unique contribution — chunks not already shared with an earlier archive.">${humanBytes(a.deduplicated_size)}</td>
        <td>${a.nfiles ?? '-'}</td>
        <td class="text-right whitespace-nowrap">
          <button class="btn btn-sm borg-restore-btn">${icon('fa-clock-rotate-left')}Restore</button>
          <button class="btn btn-sm btn-danger borg-delete-btn">${icon('fa-trash')}Delete</button>
        </td>
      </tr>`).join('');
      tbody.querySelectorAll('.borg-restore-btn').forEach((btn) => {
        btn.addEventListener('click', () => openBorgRestoreModal(btn.closest('tr').dataset.archive, sites));
      });
      tbody.querySelectorAll('.borg-delete-btn').forEach((btn) => {
        btn.addEventListener('click', async () => {
          const row = btn.closest('tr');
          const archive = row.dataset.archive;
          const res = await confirmModal({
            title: `Delete ${archive}`,
            body: `<p>This permanently removes this archive from the borg repository. Every other archive is left untouched. This cannot be undone.</p>`,
            confirmLabel: 'Delete', danger: true,
          });
          if (!res) return;
          btn.disabled = true;
          try {
            await api('DELETE', '/api/borg/archives', { archive });
            toast(`${archive} deleted`, 'ok');
            // Re-fetch rather than just removing the row: deleting an
            // archive can free chunks no other archive references, so the
            // repository usage card needs a fresh number too, not just the
            // table.
            loadArchives();
          } catch (err) {
            toast('Delete failed: ' + err.message, 'err');
            btn.disabled = false;
          }
        });
      });
    } catch (err) {
      tbody.innerHTML = `<tr><td colspan="7" class="text-rose-600">${escapeHTML(err.message)}</td></tr>`;
    }
  }
  loadArchives();

  container.querySelector('#borg-backup-run').addEventListener('click', async () => {
    const btn = container.querySelector('#borg-backup-run');
    const outWrap = btn.closest('.card').querySelector('.output-wrap');
    const out = outWrap.querySelector('.output-panel');
    btn.disabled = true; outWrap.classList.remove('hidden'); out.classList.remove('err'); out.textContent = 'Working…';
    try {
      const result = await api('POST', '/api/borg/backup', {
        domain: container.querySelector('#borg-backup-target').value,
        prune: container.querySelector('#borg-backup-prune').checked,
      });
      out.textContent = result.output || '(no output)';
      toast('Borg backup complete', 'ok');
      loadArchives();
    } catch (err) {
      out.classList.add('err');
      out.textContent = ((err.data && err.data.output) ? err.data.output + '\n\n' : '') + 'Error: ' + err.message;
      toast('Borg backup failed: ' + err.message, 'err');
    } finally { btn.disabled = false; }
  });

  const presetSelect = container.querySelector('#borg-schedule-preset');
  presetSelect.addEventListener('change', () => {
    container.querySelector('#borg-schedule-custom-field').classList.toggle('hidden', presetSelect.value !== 'custom');
  });

  container.querySelector('#borg-schedule-enable').addEventListener('click', async () => {
    const preset = presetSelect.value;
    const onCalendar = preset === 'custom' ? container.querySelector('#borg-schedule-custom').value.trim()
      : preset === 'hourly' ? 'hourly' : preset === 'weekly' ? 'Sun 03:00' : '03:00';
    if (!onCalendar) { toast('Enter an OnCalendar expression', 'err'); return; }
    const btn = container.querySelector('#borg-schedule-enable');
    const outWrap = btn.closest('.card').querySelector('.output-wrap');
    const out = outWrap.querySelector('.output-panel');
    btn.disabled = true; outWrap.classList.remove('hidden'); out.classList.remove('err'); out.textContent = 'Working…';
    try {
      const result = await api('POST', '/api/borg/schedule', { on_calendar: onCalendar, prune: container.querySelector('#borg-schedule-prune').checked });
      out.textContent = result.output || '(no output)';
      toast('Scheduled backups enabled', 'ok');
      switchView('backups');
    } catch (err) {
      out.classList.add('err'); out.textContent = 'Error: ' + err.message;
      toast('Failed to enable schedule: ' + err.message, 'err');
    } finally { btn.disabled = false; }
  });

  container.querySelector('#borg-schedule-disable').addEventListener('click', async () => {
    const btn = container.querySelector('#borg-schedule-disable');
    btn.disabled = true;
    try {
      await api('POST', '/api/borg/schedule', { disable: true });
      toast('Scheduled backups disabled', 'ok');
      switchView('backups');
    } catch (err) { toast(err.message, 'err'); btn.disabled = false; }
  });
}

async function openBorgRestoreModal(archive, sites) {
  const backdrop = document.createElement('div');
  backdrop.className = 'modal-backdrop';
  backdrop.innerHTML = `
    <div class="modal-box">
      <h3 class="text-base font-semibold text-slate-900 mb-2">Restore ${escapeHTML(archive)}</h3>
      <label class="field-label">Restore into
        <select id="borg-restore-domain" class="field-input mt-1">
          <option value="" disabled selected>Choose a site</option>
          ${sites.map((s) => `<option value="${escapeHTML(s.domain)}">${escapeHTML(s.domain)}</option>`).join('')}
        </select>
      </label>
      <label class="field-checkbox-row"><input type="checkbox" id="borg-restore-db" checked> Restore database</label>
      <label class="field-checkbox-row"><input type="checkbox" id="borg-restore-files"> Restore files (overwrites in place)</label>
      <label class="field-checkbox-row"><input type="checkbox" id="borg-restore-nosafety"> Skip the database safety backup</label>
      <label class="field-label mt-2">Type the domain to confirm
        <input type="text" id="borg-restore-confirm" autocomplete="off" class="field-input mt-1"></label>
      <div class="flex justify-end gap-2 mt-5">
        <button id="borg-restore-cancel" class="btn">Cancel</button>
        <button id="borg-restore-ok" class="btn btn-danger-solid" disabled>${icon('fa-triangle-exclamation')}Restore</button>
      </div>
    </div>`;
  document.body.appendChild(backdrop);

  const domainSel = backdrop.querySelector('#borg-restore-domain');
  const confirmInput = backdrop.querySelector('#borg-restore-confirm');
  const okBtn = backdrop.querySelector('#borg-restore-ok');
  function checkEnabled() { okBtn.disabled = !domainSel.value || confirmInput.value !== domainSel.value; }
  domainSel.addEventListener('change', checkEnabled);
  confirmInput.addEventListener('input', checkEnabled);
  backdrop.querySelector('#borg-restore-cancel').addEventListener('click', () => backdrop.remove());

  okBtn.addEventListener('click', async () => {
    const domain = domainSel.value;
    const database = backdrop.querySelector('#borg-restore-db').checked;
    const files = backdrop.querySelector('#borg-restore-files').checked;
    if (!database && !files) { toast('Choose database, files, or both', 'err'); return; }
    backdrop.remove();

    const outWrap = document.getElementById('borg-restore-output-wrap');
    const out = outWrap ? outWrap.querySelector('.output-panel') : null;
    if (outWrap && out) { outWrap.classList.remove('hidden'); out.classList.remove('err'); out.textContent = `Restoring ${domain} from ${archive}…`; }
    try {
      const result = await api('POST', '/api/borg/restore', {
        domain, archive, database, files, confirm_domain: domain,
        no_safety_backup: backdrop.querySelector('#borg-restore-nosafety').checked,
      });
      if (out) out.textContent = result.output || '(no output)';
      toast('Borg restore complete', 'ok');
    } catch (err) {
      if (out) { out.classList.add('err'); out.textContent = ((err.data && err.data.output) ? err.data.output + '\n\n' : '') + 'Error: ' + err.message; }
      toast('Borg restore failed: ' + err.message, 'err');
    }
  });
}

function renderBackupsTable(tbody, backups) {
  if (!backups || !backups.length) {
    tbody.innerHTML = '<tr><td colspan="4" class="text-slate-400">No backups yet.</td></tr>';
    return;
  }
  tbody.innerHTML = backups.map((b) => `<tr data-path="${escapeHTML(b.path)}" data-name="${escapeHTML(b.name)}">
    <td class="font-mono text-xs">${escapeHTML(b.name)}</td>
    <td>${fmt(b.size_mb)} MB</td>
    <td>${escapeHTML(b.mod_time)}</td>
    <td class="text-right whitespace-nowrap">
      <a class="btn btn-sm" href="/api/backups/download?path=${encodeURIComponent(b.path)}" download="${escapeHTML(b.name)}">${icon('fa-download')}Download</a>
      <button class="btn btn-sm btn-danger backup-delete-btn">${icon('fa-trash')}Delete</button>
    </td>
  </tr>`).join('');

  tbody.querySelectorAll('.backup-delete-btn').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const row = btn.closest('tr');
      const path = row.dataset.path;
      const name = row.dataset.name;
      const res = await confirmModal({
        title: `Delete ${name}`,
        body: `<p>This permanently deletes this backup file. It cannot be undone.</p>`,
        confirmLabel: 'Delete', danger: true,
      });
      if (!res) return;
      btn.disabled = true;
      try {
        await api('DELETE', '/api/backups', { path });
        toast(`${name} deleted`, 'ok');
        row.remove();
      } catch (err) {
        toast('Delete failed: ' + err.message, 'err');
        btn.disabled = false;
      }
    });
  });
}

// ---- Tuning ---------------------------------------------------------------------

views.tuning = async (container) => {
  container.innerHTML = `
    <h2 class="page-title">${icon('fa-sliders', 'text-indigo-500')}Tuning</h2>
    <div class="card">
      <label class="field-label">Profile</label>
      <select id="tune-profile" class="field-input max-w-md">
        <option value="balanced">balanced — a handful of ordinary sites</option>
        <option value="cache">cache — maximum traffic per gigabyte</option>
        <option value="density">density — many low-traffic sites</option>
        <option value="database">database — query-heavy workloads</option>
      </select>
      <div class="mt-3"><button id="tune-preview" class="btn">${icon('fa-eye')}Preview</button></div>
    </div>
    <div id="tune-result"></div>`;

  async function preview() {
    const resultEl = container.querySelector('#tune-result');
    resultEl.innerHTML = '<p class="text-slate-400 text-sm">Loading…</p>';
    const profile = container.querySelector('#tune-profile').value;
    const data = await api('GET', '/api/tune?profile=' + encodeURIComponent(profile));
    resultEl.innerHTML = `
      <div class="grid grid-cols-1 lg:grid-cols-2 gap-5">
        <div class="card"><h3 class="card-title">${icon('fa-microchip')}Machine</h3>
          <dl class="kv-grid">${(data.machine || []).map(([k, v]) => `<dt>${escapeHTML(k)}</dt><dd>${escapeHTML(v)}</dd>`).join('')}</dl>
        </div>
        <div class="card"><h3 class="card-title">${icon('fa-list-check')}Plan</h3>
          <dl class="kv-grid">${(data.plan || []).map(([k, v]) => `<dt>${escapeHTML(k)}</dt><dd>${escapeHTML(v)}</dd>`).join('')}</dl>
        </div>
      </div>
      <div class="card"><h3 class="card-title">${icon('fa-lightbulb')}Reasoning</h3>
        <ul class="list-disc list-inside text-sm text-slate-600 space-y-1">${(data.explain || []).map((l) => `<li>${escapeHTML(l)}</li>`).join('')}</ul>
        ${(data.warnings || []).map((w) => `<p class="text-rose-600 text-sm mt-2">${escapeHTML(w)}</p>`).join('')}
      </div>
      <div class="card">
        <div class="flex items-center justify-between mb-3">
          <h3 class="card-title mb-0">${icon('fa-check')}Apply this plan</h3>
          <label class="field-checkbox-row"><input type="checkbox" id="tune-save" checked> Save as default profile</label>
        </div>
        <button id="tune-apply" class="btn btn-primary">${icon('fa-bolt')}Apply and reload services</button>
        <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
      </div>`;

    resultEl.querySelector('#tune-apply').addEventListener('click', async () => {
      const res = await confirmModal({ title: 'Apply tuning', body: '<p>This rewrites nginx, PHP-FPM and database configuration and reloads the affected services.</p>', confirmLabel: 'Apply' });
      if (!res) return;
      const btn = resultEl.querySelector('#tune-apply');
      const outWrap = resultEl.querySelector('.output-wrap');
      const out = outWrap.querySelector('.output-panel');
      btn.disabled = true; outWrap.classList.remove('hidden'); out.classList.remove('err'); out.textContent = 'Working…';
      try {
        const result = await api('POST', '/api/tune/apply', { profile, save: resultEl.querySelector('#tune-save').checked });
        out.textContent = result.output || '(no output)';
        toast('Tuning applied', 'ok');
      } catch (err) {
        out.classList.add('err');
        out.textContent = ((err.data && err.data.output) ? err.data.output + '\n\n' : '') + 'Error: ' + err.message;
        toast('Apply failed: ' + err.message, 'err');
      } finally { btn.disabled = false; }
    });
  }

  container.querySelector('#tune-preview').addEventListener('click', preview);
  await preview();
};

// ---- Config ---------------------------------------------------------------------

views.config = async (container) => {
  const { rows } = await api('GET', '/api/config');
  container.innerHTML = `
    <h2 class="page-title">${icon('fa-gear', 'text-indigo-500')}Configuration</h2>
    <div class="card">
      <table class="data-table">
        <thead><tr><th>Key</th><th>Value</th><th></th></tr></thead>
        <tbody>${(rows || []).map(([k, v]) => `
          <tr data-key="${escapeHTML(k)}">
            <td class="font-mono text-xs">${escapeHTML(k)}</td>
            <td><input type="text" class="cfg-value field-input" value="${escapeHTML(v)}"></td>
            <td><button class="btn btn-sm cfg-save">Save</button></td>
          </tr>`).join('')}
        </tbody>
      </table>
    </div>`;

  container.querySelectorAll('.cfg-save').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const row = btn.closest('tr');
      const key = row.dataset.key;
      const value = row.querySelector('.cfg-value').value;
      btn.disabled = true;
      try {
        await api('POST', '/api/config', { key, value });
        toast(`${key} updated`, 'ok');
      } catch (err) { toast(err.message, 'err'); } finally { btn.disabled = false; }
    });
  });
};

// ---- Setup & Hardening ------------------------------------------------------------

views.bootstrap = async (container) => {
  container.innerHTML = `
    <h2 class="page-title">${icon('fa-toolbox', 'text-indigo-500')}Setup &amp; Hardening</h2>
    <p class="text-sm text-slate-500 mb-4">Run once on a fresh machine, or again to re-apply configuration after a manual change.</p>
    <div class="card">
      <h3 class="card-title">${icon('fa-download')}Install and configure the stack</h3>
      <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
        <div><label class="field-label">Database</label>
          <select id="setup-db" class="field-input"><option value="mariadb">MariaDB</option><option value="mysql">MySQL</option></select>
        </div>
      </div>
      <label class="field-checkbox-row"><input type="checkbox" id="setup-redis"> Install Redis for the WordPress object cache</label>
      <label class="field-checkbox-row"><input type="checkbox" id="setup-skip"> Configuration only (packages already installed)</label>
      <div class="mt-3"><button id="setup-run" class="btn btn-primary">${icon('fa-download')}Run setup</button></div>
      <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
    </div>
    <div id="secure-panel"></div>
    <div class="card">
      <h3 class="card-title">${icon('fa-lock')}Certificates</h3>
      <button id="ssl-renew" class="btn">${icon('fa-rotate')}Renew all Let's Encrypt certificates</button>
      <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
    </div>`;

  container.querySelector('#setup-run').addEventListener('click', async () => {
    const btn = container.querySelector('#setup-run');
    const outWrap = container.querySelectorAll('.output-wrap')[0];
    const out = outWrap.querySelector('.output-panel');
    btn.disabled = true; outWrap.classList.remove('hidden'); out.classList.remove('err');
    out.textContent = 'Working… this installs packages and can take a few minutes.';
    try {
      const result = await api('POST', '/api/setup', {
        database: container.querySelector('#setup-db').value,
        redis: container.querySelector('#setup-redis').checked,
        skip_packages: container.querySelector('#setup-skip').checked,
      });
      out.textContent = result.output || '(no output)';
      toast('Setup complete', 'ok');
    } catch (err) {
      out.classList.add('err');
      out.textContent = ((err.data && err.data.output) ? err.data.output + '\n\n' : '') + 'Error: ' + err.message;
      toast('Setup failed: ' + err.message, 'err');
    } finally { btn.disabled = false; }
  });

  actionPanel(container.querySelector('#secure-panel'), {
    title: 'Apply firewall, fail2ban and update hardening', iconName: 'fa-shield-halved', buttonLabel: 'Apply hardening',
    run: () => api('POST', '/api/secure', {}),
  });

  container.querySelector('#ssl-renew').addEventListener('click', async () => {
    const btn = container.querySelector('#ssl-renew');
    const outWrap = container.querySelectorAll('.output-wrap')[1];
    const out = outWrap.querySelector('.output-panel');
    btn.disabled = true; outWrap.classList.remove('hidden'); out.textContent = 'Working…';
    try {
      const result = await api('POST', '/api/ssl/renew', {});
      out.textContent = result.output || '(no output)';
      toast('Renewal check complete', 'ok');
    } catch (err) {
      out.classList.add('err'); out.textContent = 'Error: ' + err.message;
      toast('Renewal failed: ' + err.message, 'err');
    } finally { btn.disabled = false; }
  });
};

// ---- Danger Zone -------------------------------------------------------------------

views.danger = async (container) => {
  container.innerHTML = `
    <h2 class="page-title">${icon('fa-triangle-exclamation', 'text-rose-500')}Danger Zone</h2>
    <div class="card border-rose-200">
      <h3 class="card-title text-rose-700">${icon('fa-trash')}Uninstall ngxsetup</h3>
      <p class="text-sm text-slate-500 mb-3">Removes every file ngxsetup manages and restores the packaged defaults it overwrote.</p>
      <label class="field-checkbox-row"><input type="checkbox" id="u-sites"> Also delete every site's files, database and system user</label>
      <label class="field-checkbox-row"><input type="checkbox" id="u-packages"> Also remove nginx, PHP and the database server themselves</label>
      <div class="flex gap-2 mt-4">
        <button id="u-preview" class="btn">${icon('fa-eye')}Preview what this will do</button>
        <button id="u-run" class="btn btn-danger-solid">${icon('fa-trash')}Uninstall</button>
      </div>
      <div id="u-plan" class="mt-3 text-sm"></div>
      <div class="output-wrap hidden mt-3"><div class="output-panel"></div></div>
    </div>`;

  async function loadPlan() {
    const q = `purge_sites=${container.querySelector('#u-sites').checked}&purge_packages=${container.querySelector('#u-packages').checked}`;
    const { lines } = await api('GET', '/api/uninstall/plan?' + q);
    container.querySelector('#u-plan').innerHTML = `<ul class="list-disc list-inside text-slate-600 space-y-1">${(lines || []).map((l) => `<li>${escapeHTML(l)}</li>`).join('')}</ul>`;
  }
  container.querySelector('#u-preview').addEventListener('click', loadPlan);

  container.querySelector('#u-run').addEventListener('click', async () => {
    const res = await confirmModal({
      title: 'Uninstall ngxsetup', body: '<p>This cannot be undone. Review the preview above before continuing.</p>',
      confirmLabel: 'Uninstall', danger: true, requireText: 'UNINSTALL',
    });
    if (!res) return;
    const btn = container.querySelector('#u-run');
    const outWrap = container.querySelector('.output-wrap');
    const out = outWrap.querySelector('.output-panel');
    btn.disabled = true; outWrap.classList.remove('hidden'); out.textContent = 'Working…';
    try {
      const result = await api('POST', '/api/uninstall', {
        purge_sites: container.querySelector('#u-sites').checked,
        purge_packages: container.querySelector('#u-packages').checked,
        confirm: 'UNINSTALL',
      });
      out.textContent = result.output || '(no output)';
      toast('Uninstall complete', 'ok');
    } catch (err) {
      out.classList.add('err');
      out.textContent = ((err.data && err.data.output) ? err.data.output + '\n\n' : '') + 'Error: ' + err.message;
      toast('Uninstall failed: ' + err.message, 'err');
    } finally { btn.disabled = false; }
  });
};

// ---- boot ------------------------------------------------------------------------

switchView('dashboard');
