const test = require('node:test');
const assert = require('node:assert/strict');

const { buildChartModel, chartSampleFromPing, traceRowsHTML } = require('./app.js');

test('chartSampleFromPing preserves successful RTT samples', () => {
  assert.deepEqual(chartSampleFromPing({ success: true, rtt_ms: 12.5 }), { success: true, rtt: 12.5 });
});

test('chartSampleFromPing treats failed pings as loss samples', () => {
  assert.deepEqual(chartSampleFromPing({ success: false, error: 'timeout' }), { success: false, rtt: NaN });
});

test('buildChartModel uses one filled time slot per loss sample', () => {
  const model = buildChartModel([
    { success: true, rtt: 10 },
    { success: false, rtt: NaN },
    { success: true, rtt: 20 },
  ], 100, 20);

  assert.deepEqual(model.segments, [['95,10', '95.84,10', '96.67,10'], ['98.33,2', '99.17,2', '100,2']]);
  assert.deepEqual(model.losses, [{ x: 96.67, width: 1.66 }]);
  assert.equal(model.successCount, 2);
  assert.equal(model.lossCount, 1);
  assert.match(model.label, /3 ping samples, 2 received, 1 lost/);
});

test('buildChartModel renders consecutive losses without gaps', () => {
  const model = buildChartModel([
    { success: true, rtt: 10 },
    { success: false, rtt: NaN },
    { success: false, rtt: NaN },
    { success: true, rtt: 20 },
  ], 100, 20);

  assert.deepEqual(model.losses, [{ x: 95, width: 1.67 }, { x: 96.67, width: 1.66 }]);
  assert.deepEqual(model.segments, [['93.33,10', '94.17,10', '95,10'], ['98.33,2', '99.17,2', '100,2']]);
});

test('buildChartModel defaults to a compact chart width', () => {
  const model = buildChartModel([{ success: true, rtt: 10 }]);

  assert.equal(model.width, 130);
});

test('buildChartModel keeps the most recent sixty chart samples', () => {
  const samples = Array.from({ length: 62 }, (_, i) => ({ success: true, rtt: i + 1 }));
  const model = buildChartModel(samples, 100, 20);

  assert.equal(model.sampleCount, 60);
  assert.equal(model.maxRTT, 62);
  assert.equal(model.successCount, 60);
});

test('traceRowsHTML escapes MTR host and ASN data', () => {
  const html = traceRowsHTML({ rows: [{
    hop: 1,
    host: 'edge<1>',
    address: '192.0.2.1',
    asn: 'AS64500',
    as_name: 'EXAMPLE<NET> - Example & Transit, Inc., US',
    loss_pct: 0,
    sent: 10,
    last_ms: 1.25,
    avg_ms: 1.5,
    best_ms: 1,
    worst_ms: 2,
  }] });

  assert.match(html, /edge&lt;1&gt;/);
  assert.doesNotMatch(html, /edge<1>/);
  assert.match(html, /<span class="asn" title="Example &amp; Transit, Inc., US">AS64500 <span class="as-name">EXAMPLE&lt;NET&gt;<\/span><\/span>/);
});

test('traceRowsHTML shows short AS names without an organization whole and without a tooltip', () => {
  const html = traceRowsHTML({ rows: [{ hop: 1, address: '192.0.2.1', asn: 'AS64501', as_name: 'EXAMPLE-AS, CN' }] });

  assert.match(html, /<span class="asn">AS64501 <span class="as-name">EXAMPLE-AS, CN<\/span><\/span>/);
});

test('traceRowsHTML truncates long AS names with the full name as the tooltip', () => {
  const html = traceRowsHTML({ rows: [
    { hop: 1, address: '192.0.2.1', asn: 'AS64501', as_name: 'EXAMPLE-AS No.1 Example Street, CN' },
    { hop: 2, address: '192.0.2.2', asn: 'AS64502', as_name: 'EXAMPLE-VERY-LONG-TRANSIT-HANDLE - Example Transit, US' },
  ] });

  assert.match(html, /<span class="asn" title="EXAMPLE-AS No.1 Example Street, CN">AS64501 <span class="as-name">EXAMPLE-AS No.1 Examp...<\/span><\/span>/);
  assert.match(html, /<span class="asn" title="EXAMPLE-VERY-LONG-TRANSIT-HANDLE - Example Transit, US">AS64502 <span class="as-name">EXAMPLE-VERY-LONG-TRA...<\/span><\/span>/);
});
