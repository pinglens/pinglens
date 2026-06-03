const form = document.querySelector('#probe-form');
const target = document.querySelector('#target');
const message = document.querySelector('#message');
const regionsBox = document.querySelector('#regions');
const resultsBody = document.querySelector('#results');
const stopJob = document.querySelector('#stop-job');
const refreshRegions = document.querySelector('#refresh-regions');
let source;
let rows = new Map();
let regionNames = new Map();
let currentJobID;

function fmt(value, suffix = '') {
  if (value === undefined || value === null || Number.isNaN(Number(value))) return '—';
  return `${Number(value).toFixed(2)}${suffix}`;
}

function regionName(region) {
  if (typeof region === 'object' && region !== null) {
    if (region.name) regionNames.set(region.id, region.name);
    return region.name || region.id;
  }
  return regionNames.get(region) || region;
}

async function loadRegions() {
  const res = await fetch('/api/regions');
  const data = await res.json();
  regionsBox.innerHTML = '';
  regionNames = new Map();
  for (const region of data.regions) {
    if (region.name) regionNames.set(region.id, region.name);
    const el = document.createElement('span');
    el.className = `region-pill ${region.online ? 'online' : 'offline'}`;
    el.textContent = `${regionName(region)} ${region.online ? 'online' : 'offline'}`;
    regionsBox.appendChild(el);
  }
}

function ensureRow(region) {
  const id = typeof region === 'object' && region !== null ? region.id : region;
  if (rows.has(id)) return rows.get(id);
  const tr = document.createElement('tr');
  tr.innerHTML = `<td><span class="region-name">${escapeHTML(regionName(region))}</span></td><td class="status muted">queued</td><td class="resolved muted mono">—</td><td class="sent mono">0</td><td class="loss mono">—</td><td class="avg mono">—</td><td class="best mono">—</td><td class="worst mono">—</td><td><button class="secondary expand" disabled>pending</button></td>`;
  const details = document.createElement('tr');
  details.className = 'details-row';
  details.hidden = true;
  details.innerHTML = '<td colspan="9"><div class="details muted mono">MTR details pending.</div></td>';
  resultsBody.appendChild(tr);
  resultsBody.appendChild(details);
  const state = { id, tr, details, pings: 0, trace: null };
  tr.querySelector('.expand').addEventListener('click', () => { details.hidden = !details.hidden; });
  rows.set(id, state);
  return state;
}

function applyEvent(event) {
  const row = ensureRow(event.cluster_id);
  if (event.resolved) row.tr.querySelector('.resolved').textContent = event.resolved;
  const status = row.tr.querySelector('.status');
  if (event.type === 'offline' || event.type === 'busy' || event.type === 'error') {
    row.done = true;
    status.textContent = event.error || event.type;
    status.className = 'status bad';
  } else if (event.type === 'start') {
    status.textContent = 'running';
    status.className = 'status warn';
    row.done = false;
  } else if (event.type === 'ping' && event.ping) {
    row.pings++;
    row.tr.querySelector('.sent').textContent = row.pings;
    status.textContent = event.ping.success ? `${fmt(event.ping.rtt_ms, ' ms')}` : event.ping.error;
    status.className = event.ping.success ? 'status good mono' : 'status bad';
  } else if (event.type === 'summary' && event.summary) {
    row.tr.querySelector('.sent').textContent = event.summary.sent;
    row.tr.querySelector('.loss').textContent = fmt(event.summary.loss_pct, '%');
    row.tr.querySelector('.avg').textContent = fmt(event.summary.avg_ms, ' ms');
    row.tr.querySelector('.best').textContent = fmt(event.summary.min_ms, ' ms');
    row.tr.querySelector('.worst').textContent = fmt(event.summary.max_ms, ' ms');
  } else if (event.type === 'trace' && event.trace) {
    row.trace = event.trace;
    renderTrace(row);
  } else if (event.type === 'complete') {
    row.done = true;
    status.textContent = 'complete';
    status.className = 'status good';
  }
  updateJobCompletion();
}
function updateJobCompletion() {
  if (!currentJobID || rows.size === 0) return;
  for (const row of rows.values()) {
    if (!row.done) return;
  }
  stopJob.hidden = true;
  currentJobID = undefined;
  if (source) {
    source.close();
    source = undefined;
  }
}


function renderTrace(row) {
  const button = row.tr.querySelector('.expand');
  button.disabled = false;
  button.textContent = 'expand';
  if (row.trace.error) {
    row.details.querySelector('.details').innerHTML = `<p class="bad">${escapeHTML(row.trace.error)}</p>`;
    return;
  }
  const rowsHTML = row.trace.rows.map(hop => `<tr><td>${hop.hop}</td><td>${escapeHTML(hop.host || '—')}</td><td>${escapeHTML(hop.address || '—')}</td><td>${asnCell(hop)}</td><td>${fmt(hop.loss_pct, '%')}</td><td>${hop.sent || '—'}</td><td>${fmt(hop.last_ms, ' ms')}</td><td>${fmt(hop.avg_ms, ' ms')}</td><td>${fmt(hop.best_ms, ' ms')}</td><td>${fmt(hop.worst_ms, ' ms')}</td></tr>`).join('');
  row.details.querySelector('.details').innerHTML = `<table class="trace-table"><thead><tr><th>Hop</th><th>Host</th><th>IP</th><th>ASN</th><th>Loss</th><th>Snt</th><th>Last</th><th>Avg</th><th>Best</th><th>Worst</th></tr></thead><tbody>${rowsHTML}</tbody></table>`;
}

function asnCell(hop) {
  if (!hop.asn) return '—';
  const name = hop.as_name || '';
  return `<span class="asn" title="${escapeHTML(name)}">${escapeHTML(hop.asn)}</span>${name ? ` <span class="as-name">${escapeHTML(name)}</span>` : ''}`;
}

function escapeHTML(value) {
  return String(value).replace(/[&<>"']/g, c => ({'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;'}[c]));
}

async function cancelCurrentJob(reason) {
  if (!currentJobID) return;
  const id = currentJobID;
  currentJobID = undefined;
  stopJob.hidden = true;
  if (source) {
    source.close();
    source = undefined;
  }
  try {
    await fetch(`/api/jobs/${id}`, {method: 'DELETE'});
  } catch {
    // The job may already be gone; nothing useful for the UI to do.
  }
  if (reason) message.textContent = reason;
}

async function startTest(value, updateURL) {
  message.textContent = 'Starting test…';
  resultsBody.innerHTML = '';
  rows = new Map();
  await cancelCurrentJob();
  const res = await fetch('/api/jobs', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({target: value})});
  const data = await res.json();
  if (!res.ok) {
    message.textContent = data.error || 'Failed to start test';
    return;
  }
  if (updateURL) {
    const url = new URL(window.location.href);
    url.searchParams.set('target', data.target);
    window.history.replaceState({}, '', url);
  }
  message.textContent = `Testing ${data.target} for up to 10 minutes`;
  currentJobID = data.job_id;
  stopJob.hidden = false;
  for (const region of data.regions) ensureRow(region);
  source = new EventSource(`/api/jobs/${data.job_id}/events`);
  source.onmessage = e => applyEvent(JSON.parse(e.data));
  source.onerror = () => {
    message.textContent = 'Event stream disconnected; refresh or start a new test.';
  };
  for (const type of ['start', 'ping', 'summary', 'trace', 'complete', 'offline', 'busy', 'error']) {
    source.addEventListener(type, e => applyEvent(JSON.parse(e.data)));
  }
  source.addEventListener('canceled', () => {
    stopJob.hidden = true;
    currentJobID = undefined;
    if (source) source.close();
    message.textContent = 'Test canceled.';
  });
}

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  await startTest(target.value, true);
});

refreshRegions.addEventListener('click', loadRegions);

stopJob.addEventListener('click', async () => {
  await cancelCurrentJob('Canceling test…');
});

window.addEventListener('beforeunload', () => {
  if (!currentJobID) return;
  fetch(`/api/jobs/${currentJobID}`, {method: 'DELETE', keepalive: true});
});

loadRegions()
  .then(async () => {
    const targetParam = new URLSearchParams(window.location.search).get('target');
    if (targetParam) {
      target.value = targetParam;
      await startTest(targetParam, false);
    }
  })
  .catch(err => { message.textContent = err.message; });
