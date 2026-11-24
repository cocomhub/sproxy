// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 控制台接入分区：凭据清单（GET /api/credentials，admin）+ 接入指引。
// 非管理员/无凭据场景服务端回 403 ⇒ 只展示指引与降级提示，不伪造数据。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;

  function el(id) { return document.getElementById(id); }

  function banner(text, kind) {
    var box = el('v2-ac-state');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  function normalizeAKs(body) {
    var list = body && Array.isArray(body.ak) ? body.ak : [];
    return list.filter(Boolean).map(function (k) {
      return {
        ak: k.ak || '', owner: k.owner || '',
        skCount: Number(k.sk_count) || 0, aliveSK: Number(k.alive_sk) || 0
      };
    });
  }

  function render(aks) {
    var tbody = el('v2-ac-body');
    if (!tbody) { return; }
    tbody.textContent = '';
    aks.forEach(function (k) {
      var tr = document.createElement('tr');
      var cells = [k.ak, k.owner, String(k.skCount), String(k.aliveSK)];
      cells.forEach(function (v, i) {
        var td = document.createElement('td');
        td.textContent = v || '—';
        if (i === 0) { td.className = 'mono'; }
        if (i >= 2) { td.className = 'num r'; }
        tr.appendChild(td);
      });
      tbody.appendChild(tr);
    });
    var empty = el('v2-ac-empty');
    if (empty) { empty.hidden = aks.length > 0; }
  }

  function renderGuide() {
    var box = el('v2-ac-guide');
    if (!box) { return; }
    var origin = window.location.origin;
    var lines = [
      ['sclient', 'sclient --server ' + origin + ' list'],
      ['上传', 'sclient --server ' + origin + ' upload ./file.mp4'],
      ['隧道', 'sclient --server ' + origin + ' tunnel https://127.0.0.1:8080/'],
      ['WebDAV', origin + '/dav/（服务端 webdav.enabled=true 时可用）'],
      ['S3', origin + '/s3/（服务端 s3 启用时可用，SigV4 凭据在服务端配置）']
    ];
    box.textContent = '';
    lines.forEach(function (kv) {
      var row = document.createElement('div');
      row.className = 'kv';
      var k = document.createElement('span'); k.className = 'k muted'; k.textContent = kv[0];
      var v = document.createElement('code'); v.textContent = kv[1];
      row.appendChild(k); row.appendChild(v);
      box.appendChild(row);
    });
  }

  function load() {
    banner('');
    api.json('GET', '/api/credentials')
      .then(function (body) {
        var aks = normalizeAKs(body);
        render(aks);
        if (!aks.length) { banner('当前没有已登记凭据。'); }
      })
      .catch(function (e) {
        render([]);
        banner('凭据清单需要管理员权限：' + (e.message || e), 'warn');
      });
  }

  var inited = false;

  function init() {
    if (inited) { return; }
    inited = true;
    var r = el('v2-ac-refresh'); if (r) { r.addEventListener('click', load); }
    renderGuide();
    if (el('v2-ac-body')) { load(); }
  }

  if (typeof window !== 'undefined') { window.v2access = { init: init, load: load }; }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { normalizeAKs: normalizeAKs };
  }
})();
