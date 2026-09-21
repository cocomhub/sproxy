/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * volume-health.test.js —— volume-health.js 卷健康仪表纯函数模块单测。
 *
 * 运行：node --test web/static/volume-health.test.js（已并入 make web-test）。
 * 覆盖：/metrics 文本解析（parseVolumeMetrics / parseRebalanceMetrics）/ 健康徽标判定
 *      （renderVolumeHealth）/ 迁移进度条渲染（renderRebalanceProgress）。
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

const r = require(path.join(__dirname, 'volume-health.js'));

// ---- parseVolumeMetrics ----
test('parseVolumeMetrics 空文本返回空数组', () => {
  assert.deepEqual(r.parseVolumeMetrics(''), []);
  assert.deepEqual(r.parseVolumeMetrics(null), []);
  assert.deepEqual(r.parseVolumeMetrics('  \n\n  '), []);
});

test('parseVolumeMetrics 无卷指标文本返回空数组', () => {
  const txt = '# TYPE sproxy_requests_total counter\nsproxy_requests_total 42\n';
  assert.deepEqual(r.parseVolumeMetrics(txt), []);
});

test('parseVolumeMetrics 解析 total/failures 配对并计算失败率', () => {
  const txt = [
    '# TYPE sproxy_volume_io_total counter',
    'sproxy_volume_io_total{volume="main",op="upload"} 3',
    'sproxy_volume_io_total{volume="main",op="download"} 5',
    'sproxy_volume_io_failures_total{volume="main",op="upload"} 1',
    'sproxy_volume_io_failures_total{volume="disk2",op="download"} 2',
    '# TYPE sproxy_volume_io_latency_nanos_total counter',
    'sproxy_volume_io_latency_nanos_total{volume="main",op="upload"} 20000000',
  ].join('\n');
  const vols = r.parseVolumeMetrics(txt);
  // main/upload: total=3 failures=1 → fail_rate=33.33%
  const mu = vols.find((v) => v.volume === 'main' && v.op === 'upload');
  assert.ok(mu, '应解析出 main/upload');
  assert.equal(mu.total, 3);
  assert.equal(mu.failures, 1);
  assert.ok(Math.abs(mu.failRate - (1 / 3) * 100) < 0.01, 'main/upload 失败率=33.33%');
  // main/download: total=5 无 failures → 0
  const md = vols.find((v) => v.volume === 'main' && v.op === 'download');
  assert.ok(md);
  assert.equal(md.failures, 0);
  assert.equal(md.failRate, 0);
  // disk2/download: total 缺失只有 failures → total=0
  const dd = vols.find((v) => v.volume === 'disk2' && v.op === 'download');
  assert.ok(dd, '应解析出 disk2/download（即使只有 failures）');
  assert.equal(dd.total, 0);
  assert.equal(dd.failures, 2);
});

test('parseVolumeMetrics 标签顺序无关（op 在 volume 前）', () => {
  const txt = 'sproxy_volume_io_total{op="upload",volume="v1"} 7\n';
  const vols = r.parseVolumeMetrics(txt);
  const v = vols[0];
  assert.ok(v);
  assert.equal(v.volume, 'v1');
  assert.equal(v.op, 'upload');
  assert.equal(v.total, 7);
});

test('parseVolumeMetrics 忽略非 volume_io 行与注释', () => {
  const txt = [
    '# HELP sproxy_requests_total ...',
    'sproxy_requests_total 1',
    'sproxy_volume_io_total{volume="main",op="upload"} 3',
    '# EOF',
  ].join('\n');
  const vols = r.parseVolumeMetrics(txt);
  assert.equal(vols.length, 1);
  assert.equal(vols[0].volume, 'main');
});

// ---- 健康判定 ----
test('healthLevel 0 失败率 = healthy', () => {
  assert.equal(r.healthLevel(0), 'healthy');
  assert.equal(r.healthLevel(0.0), 'healthy');
});

test('healthLevel (0,5%) = warning', () => {
  assert.equal(r.healthLevel(1), 'warning');
  assert.equal(r.healthLevel(4.99), 'warning');
});

test('healthLevel ≥5% = degraded', () => {
  assert.equal(r.healthLevel(5), 'degraded');
  assert.equal(r.healthLevel(100), 'degraded');
});

test('healthLevel 无样本（NaN/负）保守 = degraded', () => {
  assert.equal(r.healthLevel(NaN), 'degraded');
  assert.equal(r.healthLevel(-1), 'degraded');
});

// ---- renderVolumeHealth ----
test('renderVolumeHealth 空数组渲染空态', () => {
  const html = r.renderVolumeHealth([]);
  assert.ok(html.includes('暂无卷健康数据'));
});

test('renderVolumeHealth 渲染卷行（卷名/操作/总数/失败率/徽标）', () => {
  const html = r.renderVolumeHealth([
    { volume: 'main', op: 'upload', total: 100, failures: 0, failRate: 0 },
    { volume: 'main', op: 'download', total: 50, failures: 3, failRate: 6 },
  ]);
  assert.ok(html.includes('main'));
  assert.ok(html.includes('upload'));
  assert.ok(html.includes('100'));
  assert.ok(html.includes('healthy'), '失败率 0 应 healthy 徽标');
  assert.ok(html.includes('degraded'), '失败率 6% 应 degraded 徽标');
});

test('renderVolumeHealth warning 徽标', () => {
  const html = r.renderVolumeHealth([
    { volume: 'v1', op: 'download', total: 200, failures: 2, failRate: 1 },
  ]);
  assert.ok(html.includes('warning'));
});

test('renderVolumeHealth 值做 HTML 转义', () => {
  const html = r.renderVolumeHealth([
    { volume: 'a<b', op: 'upload', total: 1, failures: 0, failRate: 0 },
  ]);
  assert.ok(html.includes('a&lt;b'));
  assert.ok(!html.includes('a<b'));
});

// ---- rebalance 迁移进度（roadmap 3.3 P1 残余：#440 后补） ----

// parseRebalanceMetrics 解析 /metrics 文本 → rebalance 进度数组（{from,to,percent}）。
test('parseRebalanceMetrics 空文本返回空数组', () => {
  assert.deepEqual(r.parseRebalanceMetrics(''), []);
  assert.deepEqual(r.parseRebalanceMetrics(null), []);
  assert.deepEqual(r.parseRebalanceMetrics('  \n\n  '), []);
});

test('parseRebalanceMetrics 无 rebalance 指标文本返回空数组', () => {
  const txt = [
    '# HELP sproxy_volume_io_total Per-volume IO requests',
    'sproxy_volume_io_total{volume="main",op="upload"} 3',
    '',
  ].join('\n');
  assert.deepEqual(r.parseRebalanceMetrics(txt), []);
});

test('parseRebalanceMetrics 解析 from/to/percent', () => {
  const txt = [
    '# HELP sproxy_rebalance_progress Volume rebalance progress percent',
    'sproxy_rebalance_progress{from_volume="main",to_volume="disk2"} 40',
    '',
  ].join('\n');
  const prog = r.parseRebalanceMetrics(txt);
  assert.equal(prog.length, 1);
  assert.equal(prog[0].from, 'main');
  assert.equal(prog[0].to, 'disk2');
  assert.equal(prog[0].percent, 40);
});

test('parseRebalanceMetrics 多任务按 from/to 分组 + 忽略非进度行', () => {
  const txt = [
    'sproxy_rebalance_progress{from_volume="main",to_volume="disk2"} 40',
    'sproxy_rebalance_progress{from_volume="a",to_volume="b"} 100',
    'sproxy_volume_io_total{volume="main",op="upload"} 3',
    'sproxy_rebalance_progress{from_volume="main",to_volume="disk2"} 55',
    '',
  ].join('\n');
  const prog = r.parseRebalanceMetrics(txt);
  // 同 (from,to) 取最新值：main→disk2 55；a→b 100。
  assert.equal(prog.length, 2);
  const byFrom = {};
  for (const p of prog) byFrom[p.from] = p;
  assert.equal(byFrom['main'].percent, 55);
  assert.equal(byFrom['a'].percent, 100);
});

test('parseRebalanceMetrics 标签顺序无关 + 值转义', () => {
  const txt = [
    'sproxy_rebalance_progress{to_volume="disk2",from_volume="main"} 10',
    '',
  ].join('\n');
  const prog = r.parseRebalanceMetrics(txt);
  assert.equal(prog.length, 1);
  assert.equal(prog[0].from, 'main');
  assert.equal(prog[0].to, 'disk2');
});

// renderRebalanceProgress 渲染进度条区（每对 from→to 一条）。
test('renderRebalanceProgress 空数组渲染空态', () => {
  const html = r.renderRebalanceProgress([]);
  assert.ok(html.includes('暂无迁移'));
});

test('renderRebalanceProgress 渲染进度条（width% + 百分比文本 + 卷对）', () => {
  const html = r.renderRebalanceProgress([{ from: 'main', to: 'disk2', percent: 40 }]);
  assert.ok(html.includes('main'));
  assert.ok(html.includes('disk2'));
  assert.ok(html.includes('width:40%'));
  assert.ok(html.includes('40%'));
});

test('renderRebalanceProgress 多任务渲染多行', () => {
  const html = r.renderRebalanceProgress([
    { from: 'main', to: 'disk2', percent: 40 },
    { from: 'a', to: 'b', percent: 100 },
  ]);
  assert.ok(html.includes('main'));
  assert.ok(html.includes('a'));
  assert.ok(html.includes('100%'));
});

test('renderRebalanceProgress 值做 HTML 转义', () => {
  const html = r.renderRebalanceProgress([{ from: 'm<ain', to: 'd&isk', percent: 50 }]);
  assert.ok(html.includes('m&lt;ain'));
  assert.ok(html.includes('d&amp;isk'));
});
