(function () {
  'use strict';

  const CHART_WIDTH = 260;
  const CHART_HEIGHT = 44;
  const MAX_CHART_SAMPLES = 600;

  function fmt(value, suffix = '') {
    if (value === undefined || value === null || Number.isNaN(Number(value))) return '—';
    return `${Number(value).toFixed(2)}${suffix}`;
  }

  function chartSampleFromPing(ping) {
    if (!ping) return { success: false, rtt: NaN };
    const rtt = Number(ping.rtt_ms);
    return {
      success: ping.success === true && Number.isFinite(rtt) && rtt >= 0,
      rtt,
    };
  }

  function roundCoord(value) {
    return Math.round(value * 100) / 100;
  }

  function clamp(value, min, max) {
    return Math.min(Math.max(value, min), max);
  }

  function buildChartModel(samples, width = CHART_WIDTH, height = CHART_HEIGHT) {
    const source = Array.isArray(samples) ? samples : [];
    const visible = source.length > MAX_CHART_SAMPLES ? source.slice(source.length - MAX_CHART_SAMPLES) : source;
    let maxRTT = 1;
    for (const sample of visible) {
      if (sample && sample.success === true && Number.isFinite(sample.rtt) && sample.rtt >= 0 && sample.rtt > maxRTT) {
        maxRTT = sample.rtt;
      }
    }

    const segments = [];
    const losses = [];
    let current = [];
    let successCount = 0;
    let lossCount = 0;
    const sampleCount = visible.length;
    const step = sampleCount > 1 ? width / (sampleCount - 1) : 0;
    const barWidth = sampleCount > 0 ? Math.max(1, Math.min(6, width / sampleCount)) : 0;
    const plotTop = 2;
    const plotHeight = Math.max(1, height - 4);

    function flushSegment() {
      if (current.length > 0) {
        segments.push(current);
        current = [];
      }
    }

    for (let i = 0; i < sampleCount; i++) {
      const sample = visible[i];
      const x = sampleCount > 1 ? i * step : width / 2;
      if (sample && sample.success === true && Number.isFinite(sample.rtt) && sample.rtt >= 0) {
        successCount++;
        const ratio = clamp(sample.rtt / maxRTT, 0, 1);
        const y = plotTop + (1 - ratio) * plotHeight;
        current.push(`${roundCoord(x)},${roundCoord(y)}`);
        continue;
      }
      lossCount++;
      flushSegment();
      losses.push({
        x: roundCoord(clamp(x - barWidth / 2, 0, Math.max(0, width - barWidth))),
        width: roundCoord(barWidth),
      });
    }
    flushSegment();

    let label = 'No ping samples yet';
    if (sampleCount > 0) {
      label = `${sampleCount} ping samples, ${successCount} received, ${lossCount} lost`;
      if (successCount > 0) label += `, max ${fmt(maxRTT, ' ms')}`;
    }

    return { width, height, maxRTT, segments, losses, successCount, lossCount, sampleCount, label };
  }

  function renderChart(row) {
    const model = buildChartModel(row.samples);
    const lossBars = model.losses.map(bar => `<rect class="chart-loss-bar" x="${bar.x}" y="0" width="${bar.width}" height="${model.height}"></rect>`).join('');
    const lines = model.segments.map(segment => `<polyline class="chart-line" points="${segment.join(' ')}"></polyline>`).join('');
    row.chart.setAttribute('viewBox', `0 0 ${model.width} ${model.height}`);
    row.chart.setAttribute('aria-label', model.label);
    row.chart.innerHTML = `<line class="chart-grid" x1="0" y1="${model.height - 2}" x2="${model.width}" y2="${model.height - 2}"></line>${lossBars}${lines}`;
  }

  function traceRowsHTML(trace) {
    const rows = Array.isArray(trace && trace.rows) ? trace.rows : [];
    return rows.map(hop => `<tr><td>${hop.hop}</td><td>${escapeHTML(hop.host || '—')}</td><td>${escapeHTML(hop.address || '—')}</td><td>${asnCell(hop)}</td><td>${fmt(hop.loss_pct, '%')}</td><td>${hop.sent || '—'}</td><td>${fmt(hop.last_ms, ' ms')}</td><td>${fmt(hop.avg_ms, ' ms')}</td><td>${fmt(hop.best_ms, ' ms')}</td><td>${fmt(hop.worst_ms, ' ms')}</td></tr>`).join('');
  }

  function renderTrace(row) {
    const button = row.tr.querySelector('.expand');
    button.disabled = false;
    button.textContent = 'MTR';
    if (row.trace.error) {
      row.details.querySelector('.trace-output').innerHTML = `<p class="bad">${escapeHTML(row.trace.error)}</p>`;
      return;
    }
    const rowsHTML = traceRowsHTML(row.trace);
    row.details.querySelector('.trace-output').innerHTML = `<div class="trace-scroll"><table class="trace-table"><thead><tr><th>Hop</th><th>Host</th><th>IP</th><th>ASN</th><th>Loss</th><th>Snt</th><th>Last</th><th>Avg</th><th>Best</th><th>Worst</th></tr></thead><tbody>${rowsHTML}</tbody></table></div>`;
  }

  function asnCell(hop) {
    if (!hop.asn) return '—';
    const name = hop.as_name || '';
    return `<span class="asn" title="${escapeHTML(name)}">${escapeHTML(hop.asn)}</span>${name ? ` <span class="as-name">${escapeHTML(name)}</span>` : ''}`;
  }

  function installUI(document) {
    const form = document.querySelector('#probe-form');
    if (!form) return;

    const target = document.querySelector('#target');
    const message = document.querySelector('#message');
    const resultsBody = document.querySelector('#results');
    const stopJob = document.querySelector('#stop-job');
    let source;
    let rows = new Map();
    let regionNames = new Map();
    let currentJobID;

    function regionName(region) {
      if (typeof region === 'object' && region !== null) {
        if (region.name) regionNames.set(region.id, region.name);
        return region.name || region.id;
      }
      return regionNames.get(region) || region;
    }

    function ensureRow(region) {
      const id = typeof region === 'object' && region !== null ? region.id : region;
      if (rows.has(id)) return rows.get(id);
      const tr = document.createElement('tr');
      tr.innerHTML = `<td><span class="region-name">${escapeHTML(regionName(region))}</span></td><td class="status muted">queued</td><td class="chart-cell"><svg class="probe-chart" role="img" aria-label="No ping samples yet" viewBox="0 0 ${CHART_WIDTH} ${CHART_HEIGHT}" preserveAspectRatio="none"></svg></td><td class="sent mono">0</td><td class="recv mono">0</td><td class="loss mono">—</td><td class="last mono">—</td><td class="min mono">—</td><td class="avg mono">—</td><td class="max mono">—</td><td><button class="secondary expand" disabled>MTR</button></td>`;
      const details = document.createElement('tr');
      details.className = 'details-row';
      details.hidden = true;
      details.innerHTML = '<td colspan="11"><div class="trace-output muted mono">MTR details pending.</div></td>';
      resultsBody.appendChild(tr);
      resultsBody.appendChild(details);
      const state = { id, tr, details, chart: tr.querySelector('.probe-chart'), samples: [], sent: 0, recv: 0, trace: null };
      tr.querySelector('.expand').addEventListener('click', () => { details.hidden = !details.hidden; });
      renderChart(state);
      rows.set(id, state);
      return state;
    }

    function applyEvent(event) {
      const row = ensureRow(event.cluster_id);
      if (event.resolved) row.tr.querySelector('.region-name').title = event.resolved;
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
        row.sent++;
        const sample = chartSampleFromPing(event.ping);
        row.samples.push(sample);
        if (row.samples.length > MAX_CHART_SAMPLES) row.samples.shift();
        row.tr.querySelector('.sent').textContent = row.sent;
        if (sample.success) {
          row.recv++;
          row.tr.querySelector('.recv').textContent = row.recv;
          row.tr.querySelector('.last').textContent = fmt(event.ping.rtt_ms, ' ms');
          status.textContent = 'running';
          status.className = 'status warn';
        } else {
          row.tr.querySelector('.last').textContent = event.ping.error || 'loss';
          status.textContent = 'loss';
          status.className = 'status bad';
        }
        renderChart(row);
      } else if (event.type === 'summary' && event.summary) {
        row.sent = event.summary.sent;
        row.recv = event.summary.received;
        row.tr.querySelector('.sent').textContent = event.summary.sent;
        row.tr.querySelector('.recv').textContent = event.summary.received;
        row.tr.querySelector('.loss').textContent = fmt(event.summary.loss_pct, '%');
        row.tr.querySelector('.min').textContent = fmt(event.summary.min_ms, ' ms');
        row.tr.querySelector('.avg').textContent = fmt(event.summary.avg_ms, ' ms');
        row.tr.querySelector('.max').textContent = fmt(event.summary.max_ms, ' ms');
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
      message.textContent = 'Starting test...';
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
      message.textContent = `Testing ${data.target}...`;
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

    stopJob.addEventListener('click', async () => {
      await cancelCurrentJob('Canceling test...');
    });

    window.addEventListener('beforeunload', () => {
      if (!currentJobID) return;
      fetch(`/api/jobs/${currentJobID}`, {method: 'DELETE', keepalive: true});
    });

    const targetParam = new URLSearchParams(window.location.search).get('target');
    if (targetParam) {
      target.value = targetParam;
      startTest(targetParam, false).catch(err => { message.textContent = err.message; });
    }
  }

  function escapeHTML(value) {
    return String(value).replace(/[&<>"']/g, c => ({'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;'}[c]));
  }

  const exported = { buildChartModel, chartSampleFromPing, traceRowsHTML };
  if (typeof module !== 'undefined' && module.exports) module.exports = exported;
  if (typeof window !== 'undefined') window.PingLensChart = exported;
  if (typeof document !== 'undefined') installUI(document);
})();