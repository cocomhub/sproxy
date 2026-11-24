// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 控制台巡检分区：POST /api/verify 触发全仓 checksum 巡检并展示报告。
// 服务端在 busy 时返回 concurrent=true（不排队、不堆叠）；坏文件只隔离不删除。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;

  function el(id) { return document.getElementById(id); }

  function banner(text, kind) {
    var box = el('v2-vf-state');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  function normalizeReport(body) {
    var r = body || {};
    var arr = function (v) { return Array.isArray(v) ? v : []; };
    return {
      total: Number(r.total) || 0,
      ok: Number(r.ok) || 0,
      mismatched: arr(r.mismatched),
      missing: arr(r.missing),
      errors: arr(r.errors),
      skipped: Number(r.skipped) || 0,
      concurrent: !!r.concurrent
    };
  }

  function stat(label, value, cls) {
    var box = document.createElement('div');
    box.className = 'kpi' + (cls ? ' ' + cls : '');
    var k = document.createElement('div'); k.className = 'k'; k.textContent = label;
    var v = document.createElement('div'); v.className = 'v num'; v.textContent = String(value);
    box.appendChild(k); box.appendChild(v);
    return box;
  }

  function render(rep) {
    var kpis = el('v2-vf-kpis');
    if (!kpis) { return; }
    kpis.textContent = '';
    kpis.appendChild(stat('核对项', rep.total));
    kpis.appendChild(stat('一致', rep.ok));
    kpis.appendChild(stat('不符（已隔离）', rep.mismatched.length, rep.mismatched.length ? 'bad' : ''));
    kpis.appendChild(stat('缺失', rep.missing.length, rep.missing.length ? 'bad' : ''));
    kpis.appendChild(stat('读/隔离失败', rep.errors.length, rep.errors.length ? 'bad' : ''));
    kpis.appendChild(stat('跳过租户', rep.skipped));

    var detail = el('v2-vf-detail');
    if (detail) {
      detail.textContent = '';
      var rows = [];
      rep.mismatched.forEach(function (m) {
        rows.push(['不符', (m && m.path) || JSON.stringify(m)]);
      });
      rep.missing.forEach(function (p) { rows.push(['缺失', p]); });
      rep.errors.forEach(function (p) { rows.push(['错误', p]); });
      rows.forEach(function (kv) {
        var row = document.createElement('div');
        row.className = 'ver-row';
        var tag = document.createElement('span');
        tag.className = 'pill ' + (kv[0] === '错误' ? 'pill-warn' : 'pill-bad');
        tag.textContent = kv[0];
        var path = document.createElement('span');
        path.className = 'mono'; path.textContent = kv[1];
        row.appendChild(tag); row.appendChild(path);
        detail.appendChild(row);
      });
      detail.hidden = rows.length === 0;
    }
    if (rep.concurrent) {
      banner('已有巡检在进行中，本次请求被跳过——请稍后重试。', 'warn');
    }
  }

  function run() {
    banner('');
    var vol = ((el('v2-vf-volume') || {}).value || '').trim();
    var body = new TextEncoder().encode(JSON.stringify({ volume: vol, force: true }));
    var btn = el('v2-vf-run');
    if (btn) { btn.disabled = true; btn.textContent = '巡检中…'; }
    api.json('POST', '/api/verify', { body: body, contentType: 'application/json' })
      .then(function (resp) { render(normalizeReport(resp)); })
      .catch(function (e) { banner('巡检失败：' + (e.message || e), 'warn'); })
      .then(function () { if (btn) { btn.disabled = false; btn.textContent = '开始巡检'; } });
  }

  var inited = false;

  function init() {
    if (inited) { return; }
    inited = true;
    var btn = el('v2-vf-run'); if (btn) { btn.addEventListener('click', run); }
    var kpis = el('v2-vf-kpis');
    if (kpis && !kpis.children.length) { kpis.appendChild(stat('核对项', '—')); }
  }

  if (typeof window !== 'undefined') { window.v2verify = { init: init, run: run }; }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { normalizeReport: normalizeReport };
  }
})();
