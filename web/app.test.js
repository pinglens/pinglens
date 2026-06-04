const test = require('node:test');
const assert = require('node:assert/strict');

const { buildChartModel, chartSampleFromPing, traceRowsHTML } = require('./app.js');

test('chartSampleFromPing preserves successful RTT samples', () => {
  assert.deepEqual(chartSampleFromPing({ success: true, rtt_ms: 12.5 }), { success: true, rtt: 12.5 });
});

test('chartSampleFromPing treats failed pings as loss samples', () => {
  assert.deepEqual(chartSampleFromPing({ success: false, error: 'timeout' }), { success: false, rtt: NaN });
});

test('buildChartModel stops latency line at loss bar edges', () => {
  const model = buildChartModel([
    { success: true, rtt: 10 },
    { success: false, rtt: NaN },
    { success: true, rtt: 20 },
  ], 100, 20);

  assert.deepEqual(model.segments, [['0,10', '47,10'], ['53,2', '100,2']]);
  assert.deepEqual(model.losses, [{ x: 47, width: 6 }]);
  assert.equal(model.successCount, 2);
  assert.equal(model.lossCount, 1);
  assert.match(model.label, /3 ping samples, 2 received, 1 lost/);
});

test('buildChartModel defaults to a compact chart width', () => {
  const model = buildChartModel([{ success: true, rtt: 10 }]);

  assert.equal(model.width, 130);
});

test('buildChartModel keeps the most recent chart samples', () => {
  const samples = Array.from({ length: 602 }, (_, i) => ({ success: true, rtt: i + 1 }));
  const model = buildChartModel(samples, 100, 20);

  assert.equal(model.sampleCount, 600);
  assert.equal(model.maxRTT, 602);
  assert.equal(model.successCount, 600);
});

test('traceRowsHTML escapes MTR host and ASN data', () => {
  const html = traceRowsHTML({ rows: [{
    hop: 1,
    host: 'edge<1>',
    address: '192.0.2.1',
    asn: 'AS64500',
    as_name: 'Example & Transit',
    loss_pct: 0,
    sent: 10,
    last_ms: 1.25,
    avg_ms: 1.5,
    best_ms: 1,
    worst_ms: 2,
  }] });

  assert.match(html, /edge&lt;1&gt;/);
  assert.match(html, /Example &amp; Transit/);
  assert.doesNotMatch(html, /edge<1>/);
});
