(function () {
  'use strict';

  const CHART_WIDTH = 130;
  const CHART_HEIGHT = 44;
  const MAX_CHART_SAMPLES = 60;
  const MAX_AS_NAME_LENGTH = 24;

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
    let currentY;
    let successCount = 0;
    let lossCount = 0;
    const sampleCount = visible.length;
    const slotWidth = width / MAX_CHART_SAMPLES;
    const chartStart = width - slotWidth * sampleCount;
    const plotTop = 2;
    const plotHeight = Math.max(1, height - 4);

    function flushSegment(endX) {
      if (current.length > 0) {
        current.push(`${endX},${currentY}`);
        segments.push(current);
        current = [];
        currentY = undefined;
      }
    }

    for (let i = 0; i < sampleCount; i++) {
      const sample = visible[i];
      const slotStart = roundCoord(chartStart + i * slotWidth);
      const slotEnd = roundCoord(chartStart + (i + 1) * slotWidth);
      if (sample && sample.success === true && Number.isFinite(sample.rtt) && sample.rtt >= 0) {
        successCount++;
        const ratio = clamp(sample.rtt / maxRTT, 0, 1);
        const y = roundCoord(plotTop + (1 - ratio) * plotHeight);
        if (current.length === 0) current.push(`${slotStart},${y}`);
        current.push(`${roundCoord(slotStart + (slotEnd - slotStart) / 2)},${y}`);
        currentY = y;
        continue;
      }
      lossCount++;
      flushSegment(slotStart);
      losses.push({
        x: slotStart,
        width: roundCoord(slotEnd - slotStart),
      });
    }
    flushSegment(width);

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

  // Team Cymru AS names read "HANDLE - Organization, CC". The table shows the
  // short handle with the organization as a tooltip; names without that
  // separator are shown whole. Either is cut to MAX_AS_NAME_LENGTH to keep the
  // column narrow, and a cut name's tooltip carries the full name instead.
  function splitASName(name) {
    const sep = name.indexOf(' - ');
    if (sep < 0) return { handle: name.trim(), org: '' };
    return { handle: name.slice(0, sep).trim(), org: name.slice(sep + 3).trim() };
  }

  function asnCell(hop) {
    if (!hop.asn) return '—';
    const name = (hop.as_name || '').trim();
    const { handle, org } = splitASName(name);
    const truncated = handle.length > MAX_AS_NAME_LENGTH;
    const shown = truncated ? `${handle.slice(0, MAX_AS_NAME_LENGTH - 3)}...` : handle;
    const title = truncated ? name : org;
    return `<span class="asn"${title ? ` title="${escapeHTML(title)}"` : ''}>${escapeHTML(hop.asn)}${shown ? ` <span class="as-name">${escapeHTML(shown)}</span>` : ''}</span>`;
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
      tr.innerHTML = `<td><span class="region-name">${escapeHTML(regionName(region))}</span></td><td class="resolved-address muted mono">—</td><td class="status mono muted">queued</td><td class="chart-cell"><svg class="probe-chart" role="img" aria-label="No ping samples yet" viewBox="0 0 ${CHART_WIDTH} ${CHART_HEIGHT}" preserveAspectRatio="none"></svg></td><td class="loss mono">—</td><td class="min mono">—</td><td class="avg mono">—</td><td class="max mono">—</td><td><button class="secondary expand" disabled>MTR</button></td>`;
      const details = document.createElement('tr');
      details.className = 'details-row';
      details.hidden = true;
      details.innerHTML = '<td colspan="9"><div class="trace-output muted mono">MTR details pending.</div></td>';
      resultsBody.appendChild(tr);
      resultsBody.appendChild(details);
      const state = { id, tr, details, chart: tr.querySelector('.probe-chart'), samples: [], trace: null };
      tr.querySelector('.expand').addEventListener('click', () => { details.hidden = !details.hidden; });
      renderChart(state);
      rows.set(id, state);
      return state;
    }

    function applyEvent(event) {
      const row = ensureRow(event.cluster_id);
      if (event.resolved) {
        row.tr.querySelector('.resolved-address').textContent = event.resolved;
        row.tr.querySelector('.region-name').title = event.resolved;
      }
      const status = row.tr.querySelector('.status');
      if (event.type === 'offline' || event.type === 'busy' || event.type === 'error') {
        row.done = true;
        status.textContent = event.error || event.type;
        status.className = 'status mono bad';
      } else if (event.type === 'start') {
        status.textContent = 'running';
        status.className = 'status mono warn';
        row.done = false;
      } else if (event.type === 'ping' && event.ping) {
        const sample = chartSampleFromPing(event.ping);
        row.samples.push(sample);
        if (row.samples.length > MAX_CHART_SAMPLES) row.samples.shift();
        if (sample.success) {
          status.textContent = fmt(event.ping.rtt_ms, ' ms');
          status.className = 'status mono good';
        } else {
          status.textContent = event.ping.error || 'loss';
          status.className = 'status mono bad';
        }
        renderChart(row);
      } else if (event.type === 'summary' && event.summary) {
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
        status.className = 'status mono good';
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