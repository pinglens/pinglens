const test = require('node:test');
const assert = require('node:assert/strict');

const { buildChartModel, chartSampleFromPing } = require('./app.js');

test('chartSampleFromPing preserves successful RTT samples', () => {
  assert.deepEqual(chartSampleFromPing({ success: true, rtt_ms: 12.5 }), { success: true, rtt: 12.5 });
});

test('chartSampleFromPing treats failed pings as loss samples', () => {
  assert.deepEqual(chartSampleFromPing({ success: false, error: 'timeout' }), { success: false, rtt: NaN });
});

test('buildChartModel renders red loss bars and separates latency lines across loss', () => {
  const model = buildChartModel([
    { success: true, rtt: 10 },
    { success: false, rtt: NaN },
    { success: true, rtt: 20 },
  ], 100, 20);

  assert.deepEqual(model.segments, [['0,10'], ['100,2']]);
  assert.deepEqual(model.losses, [{ x: 47, width: 6 }]);
  assert.equal(model.successCount, 2);
  assert.equal(model.lossCount, 1);
  assert.match(model.label, /3 ping samples, 2 received, 1 lost/);
});

test('buildChartModel keeps the most recent chart samples', () => {
  const samples = Array.from({ length: 602 }, (_, i) => ({ success: true, rtt: i + 1 }));
  const model = buildChartModel(samples, 100, 20);

  assert.equal(model.sampleCount, 600);
  assert.equal(model.maxRTT, 602);
  assert.equal(model.successCount, 600);
});
