/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * volume-health.js —— 卷健康仪表纯渲染模块（与 DOM/网络完全隔离，可 node:test 直测）。
 *
 * 隔离原则（同 user-volumes.js / app-render.js）：
 *   1. 纯函数生成 HTML 字符串 / 解析，不碰 DOM、不读模块状态、不碰全局；
 *   2. app.js 只做 DOM 写入与网络/流程，凡此处有的函数一律走本模块（volumeHealth.*）；
 *   3. 本文件 UMD：浏览器挂全局 volumeHealth，Node module.exports 可 require。
 *      顶层无任何副作用（不引用 document/window/localStorage）。
 *
 * 数据源：#432 卷健康指标入 /metrics（pkg/server/metrics.go）——
 *   sproxy_volume_io_total{volume,op} / sproxy_volume_io_failures_total{volume,op} /
 *   sproxy_volume_io_latency_nanos_total{volume,op}。本模块解析 total+failures 配对，
 *   失败率 = failures/total*100，按阈值分级（healthy/warning/degraded）。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.volumeHealth = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  // 健康阈值（可测）：失败率 0 = healthy；>0 且 <5% = warning；≥5% = degraded。
  var WARNING_THRESHOLD = 0;   // 失败率 > 0 即 warning 起点
  var DEGRADED_THRESHOLD = 5;  // 失败率 ≥ 5% 即 degraded

  // escHtml 转义（与 user-volumes.js 同实现，模块内聚）。
  function escHtml(s) {
    return String(s == null ? '' : s).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
  }

  // ---- /metrics 文本解析（纯）----

  // 行级解析：匹配 `sproxy_volume_io_<total|failures>_total{volume="v",op="o"} <n>`。
  // 返回 {total: {v: {op: n}}, failures: {...}} 两族映射，再由配对阶段聚合。
  function lineParts(line) {
    // 形如: sproxy_volume_io_total{volume="main",op="upload"} 3
    // 或     sproxy_volume_io_failures_total{volume="main",op="upload"} 1
    var m = /^sproxy_volume_io(_failures)?_total\{([^}]*)\}\s+(\d+)/.exec(line);
    if (!m) return null;
    var kind = m[1]; // "_failures" 或 undefined（total）
    var raw = m[2];
    var val = parseInt(m[3], 10);
    var labels = {};
    // 标签 k="v" 解析（顺序无关；值内引号转义场景忽略——指标值无引号）。
    var re = /([a-zA-Z_]+)="([^"]*)"/g;
    var mm;
    while ((mm = re.exec(raw)) !== null) {
      labels[mm[1]] = mm[2];
    }
    if (labels.volume === undefined || labels.op === undefined) return null;
    var family = kind === '_failures' ? 'failures' : 'total';
    return { family: family, volume: labels.volume, op: labels.op, val: val };
  }

  // parseVolumeMetrics 解析 /metrics 文本 → 卷健康数组。
  // 输出项：{volume, op, total, failures, failRate}——failRate 百分数（0-100）。
  // 有 failures 但无 total 的配对：total=0（失败率视作 100% 由 healthLevel 保守兜底）。
  // 仅 total 无 failures：failures=0、failRate=0。
  function parseVolumeMetrics(text) {
    if (!text) return [];
    var totals = {};   // key: volume+"\u0000"+op → count
    var failures = {}; // 同上
    var lines = String(text).split(/\r?\n/);
    for (var i = 0; i < lines.length; i++) {
      var p = lineParts(lines[i]);
      if (!p) continue;
      var key = p.volume + '\u0000' + p.op;
      if (p.family === 'failures') {
        failures[key] = p.val;
      } else {
        totals[key] = p.val;
      }
    }
    var seen = {};
    var out = [];
    var keys = Object.keys(totals).concat(Object.keys(failures));
    for (var j = 0; j < keys.length; j++) {
      var k = keys[j];
      if (seen[k]) continue;
      seen[k] = true;
      var t = totals[k] || 0;
      var f = failures[k] || 0;
      var rate = t > 0 ? (f / t) * 100 : (f > 0 ? 100 : 0);
      var kv = k.split('\u0000');
      out.push({ volume: kv[0], op: kv[1], total: t, failures: f, failRate: rate });
    }
    return out;
  }

  // ---- 健康分级（纯） ----
  // healthLevel(failRatePercent) → 'healthy' | 'warning' | 'degraded'。
  // 无样本（NaN）或负值保守判 degraded（面板不应显示健康绿）。
  function healthLevel(failRatePercent) {
    if (typeof failRatePercent !== 'number' || Number.isNaN(failRatePercent) || failRatePercent < 0) {
      return 'degraded';
    }
    if (failRatePercent === 0) return 'healthy';
    if (failRatePercent < DEGRADED_THRESHOLD) return 'warning';
    return 'degraded';
  }

  // ---- 面板 HTML 渲染（纯） ----
  // renderVolumeHealth(vols) → HTML 字符串。空数组渲染空态提示。
  // 每卷一行：卷名 / 操作 / 总次数 / 失败数 / 失败率 / 健康徽标（degraded 高亮）。
  function renderVolumeHealth(vols) {
    if (!vols || vols.length === 0) {
      return '<div class="empty-msg" style="color:var(--text-muted);font-size:13px;">暂无卷健康数据（/metrics 无 volume_io 样本）</div>';
    }
    var html = '<div style="margin-top:16px;border-top:1px solid var(--border-color);padding-top:12px;">';
    html += '<div style="font-weight:600;margin-bottom:8px;">卷健康</div>';
    html += '<table style="width:100%;border-collapse:collapse;font-size:13px;">';
    html += '<thead><tr style="background:var(--bg-hover);">';
    html += '<th style="padding:5px 8px;text-align:left;border-bottom:1px solid var(--border-color);">卷</th>';
    html += '<th style="padding:5px 8px;text-align:left;border-bottom:1px solid var(--border-color);">操作</th>';
    html += '<th style="padding:5px 8px;text-align:right;border-bottom:1px solid var(--border-color);">总次数</th>';
    html += '<th style="padding:5px 8px;text-align:right;border-bottom:1px solid var(--border-color);">失败</th>';
    html += '<th style="padding:5px 8px;text-align:right;border-bottom:1px solid var(--border-color);">失败率</th>';
    html += '<th style="padding:5px 8px;text-align:center;border-bottom:1px solid var(--border-color);">状态</th>';
    html += '</tr></thead><tbody>';
    for (var i = 0; i < vols.length; i++) {
      var v = vols[i];
      var lvl = healthLevel(v.failRate);
      var badge = '';
      if (lvl === 'healthy') {
        badge = '<span style="color:#2e7d32;font-weight:600;">healthy</span>';
      } else if (lvl === 'warning') {
        badge = '<span style="color:#b26a00;font-weight:600;">warning</span>';
      } else {
        badge = '<span style="color:#c62828;font-weight:700;">degraded</span>';
      }
      var rateTxt = v.total > 0 ? v.failRate.toFixed(2) + '%' : (v.failures > 0 ? '100%' : '0%');
      html += '<tr' + (lvl === 'degraded' ? ' style="background:rgba(198,40,40,0.08);"' : '') + '>';
      html += '<td style="padding:5px 8px;border-bottom:1px solid var(--border-color);font-weight:600;">' + escHtml(v.volume) + '</td>';
      html += '<td style="padding:5px 8px;border-bottom:1px solid var(--border-color);font-size:12px;color:var(--text-secondary);">' + escHtml(v.op) + '</td>';
      html += '<td style="padding:5px 8px;border-bottom:1px solid var(--border-color);text-align:right;">' + v.total + '</td>';
      html += '<td style="padding:5px 8px;border-bottom:1px solid var(--border-color);text-align:right;">' + v.failures + '</td>';
      html += '<td style="padding:5px 8px;border-bottom:1px solid var(--border-color);text-align:right;">' + rateTxt + '</td>';
      html += '<td style="padding:5px 8px;border-bottom:1px solid var(--border-color);text-align:center;">' + badge + '</td>';
      html += '</tr>';
    }
    html += '</tbody></table>';
    html += '</div>';
    return html;
  }

  // ---- rebalance 迁移进度（roadmap 3.3 P1 残余：#440 后补） ----

  // lineRebalance 匹配 `sproxy_rebalance_progress{from_volume="f",to_volume="t"} <p>`。
  // 返回 {from, to, percent} 或 null。percent 是 0-100 整数（Prometheus 文本 int64）。
  function lineRebalance(line) {
    var m = /^sproxy_rebalance_progress\{([^}]*)\}\s+(\d+)/.exec(line);
    if (!m) return null;
    var labels = {};
    var re = /([a-zA-Z_]+)="([^"]*)"/g;
    var mm;
    while ((mm = re.exec(m[1])) !== null) {
      labels[mm[1]] = mm[2];
    }
    if (labels.from_volume === undefined || labels.to_volume === undefined) return null;
    return { from: labels.from_volume, to: labels.to_volume, percent: parseInt(m[2], 10) };
  }

  // parseRebalanceMetrics 解析 /metrics 文本 → rebalance 进度数组。
  // 输出项：{from, to, percent}（percent 0-100）。同 (from,to) 多行取最新值（覆盖旧值）。
  // 无进度指标返回空数组（面板不显示迁移区）。
  function parseRebalanceMetrics(text) {
    if (!text) return [];
    var latest = {}; // key: from+to → percent
    var lines = String(text).split(/\r?\n/);
    for (var i = 0; i < lines.length; i++) {
      var p = lineRebalance(lines[i]);
      if (!p) continue;
      latest[p.from + '\u0000' + p.to] = p.percent;
    }
    var out = [];
    var keys = Object.keys(latest);
    for (var j = 0; j < keys.length; j++) {
      var kv = keys[j].split('\u0000');
      out.push({ from: kv[0], to: kv[1], percent: latest[keys[j]] });
    }
    return out;
  }

  // renderRebalanceProgress 渲染迁移进度区 HTML（每对 from→to 一条进度条）。
  // width=percent% + 百分比文本；空数组渲染空态提示（无迁移进行时区隐藏由调用方决定，
  // 本函数仅返回「无迁移」提示，调用方可按空数组决定不显示整个区）。
  function renderRebalanceProgress(progs) {
    if (!progs || progs.length === 0) {
      return '<div class="empty-msg" style="color:var(--text-muted);font-size:13px;">暂无迁移</div>';
    }
    var html = '<div style="margin-top:12px;">';
    for (var i = 0; i < progs.length; i++) {
      var p = progs[i];
      var pct = Math.max(0, Math.min(100, p.percent));
      html += '<div style="margin-bottom:8px;font-size:13px;">';
      html += '<div style="margin-bottom:2px;">' + escHtml(p.from) + ' → ' + escHtml(p.to)
        + ' <span style="color:var(--text-muted);font-size:12px;">' + pct + '%</span></div>';
      html += '<div style="height:8px;background:var(--bg-hover);border-radius:4px;overflow:hidden;">';
      html += '<div style="height:100%;width:' + pct + '%;background:#1565c0;transition:width .3s;"></div>';
      html += '</div></div>';
    }
    html += '</div>';
    return html;
  }

  return {
    parseVolumeMetrics: parseVolumeMetrics,
    healthLevel: healthLevel,
    renderVolumeHealth: renderVolumeHealth,
    parseRebalanceMetrics: parseRebalanceMetrics,
    renderRebalanceProgress: renderRebalanceProgress,
    WARNING_THRESHOLD: WARNING_THRESHOLD,
    DEGRADED_THRESHOLD: DEGRADED_THRESHOLD,
  };
});
