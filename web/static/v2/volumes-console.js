// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 控制台卷分区：系统卷（GET /api/volumes）+ 用户卷（GET /api/volumes/user）。
// 只读展示 + 单卷/全部刷新（POST /api/volumes/refresh）；建/删卷仍走 API/CLI（见边界说明）。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;
  var fmt = typeof window === 'undefined' ? null : window.sharedFormat;

  function el(id) { return document.getElementById(id); }

  function banner(text, kind) {
    var box = el('v2-vl-state');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  function pct(used, total) {
    if (!(total > 0)) { return null; }
    return Math.min(100, Math.round((used / total) * 100));
  }

  function normalizeVolumes(body) {
    var list = body && Array.isArray(body.volumes) ? body.volumes : [];
    return list.filter(Boolean).map(function (v) {
      return {
        name: v.name || '', type: v.type || 'local', mode: v.mode || '',
        capacity: Number(v.capacity) || 0, usage: Number(v.usage) || 0,
        state: v.state || 'healthy'
      };
    });
  }

  function normalizeUserVolumes(body) {
    var list = body && Array.isArray(body.volumes) ? body.volumes : [];
    return list.filter(function (v) { return v && v.name; }).map(function (v) {
      return {
        name: v.name, type: v.type || '', capacity: Number(v.capacity) || 0,
        usage: Number(v.usage) || 0
      };
    });
  }

  function statePill(state) {
    var span = document.createElement('span');
    span.className = 'pill ' + (state === 'healthy' ? 'pill-ok' : 'pill-warn');
    span.textContent = state || '—';
    return span;
  }

  function render(vols) {
    var tbody = el('v2-vl-body');
    if (!tbody) { return; }
    tbody.textContent = '';
    vols.forEach(function (v) {
      var tr = document.createElement('tr');
      var name = document.createElement('td'); name.className = 'mono'; name.textContent = v.name;
      tr.appendChild(name);
      var type = document.createElement('td'); type.textContent = v.type;
      tr.appendChild(type);
      var mode = document.createElement('td'); mode.className = 'muted'; mode.textContent = v.mode || '—';
      tr.appendChild(mode);
      var cap = document.createElement('td'); cap.className = 'r num';
      cap.textContent = v.capacity > 0 ? fmt.formatSize(v.capacity) : '不限';
      tr.appendChild(cap);

      var usage = document.createElement('td');
      var p = pct(v.usage, v.capacity);
      var bar = document.createElement('div');
      bar.className = 'bar' + (p != null && p >= 90 ? ' bad' : '');
      var fill = document.createElement('i');
      fill.style.width = (p == null ? 0 : p) + '%';
      bar.appendChild(fill);
      var txt = document.createElement('div');
      txt.className = 'muted'; txt.style.fontSize = '11px';
      txt.textContent = fmt.formatSize(v.usage) + (p == null ? '' : ' · ' + p + '%');
      usage.appendChild(bar); usage.appendChild(txt);
      tr.appendChild(usage);

      var st = document.createElement('td'); st.appendChild(statePill(v.state));
      tr.appendChild(st);
      var act = document.createElement('td'); act.className = 'r actions';
      var btn = document.createElement('button');
      btn.type = 'button'; btn.className = 'btn btn-ghost btn-sm'; btn.textContent = '刷新';
      btn.addEventListener('click', function () { refresh(v.name); });
      act.appendChild(btn);
      tr.appendChild(act);
      tbody.appendChild(tr);
    });
    var empty = el('v2-vl-empty');
    if (empty) { empty.hidden = vols.length > 0; }
  }

  function renderUserVolumes(vols) {
    var tbody = el('v2-vl-user-body');
    if (!tbody) { return; }
    tbody.textContent = '';
    vols.forEach(function (v) {
      var tr = document.createElement('tr');
      [v.name, v.type].forEach(function (text, i) {
        var td = document.createElement('td');
        td.textContent = text || '—';
        if (i === 0) { td.className = 'mono'; }
        tr.appendChild(td);
      });
      var cap = document.createElement('td'); cap.className = 'r num';
      cap.textContent = v.capacity > 0 ? fmt.formatSize(v.capacity) : '不限';
      tr.appendChild(cap);
      var usage = document.createElement('td'); usage.className = 'r num';
      usage.textContent = fmt.formatSize(v.usage);
      tr.appendChild(usage);
      var act = document.createElement('td'); act.className = 'r actions';
      var del = document.createElement('button');
      del.type = 'button'; del.className = 'btn btn-ghost btn-sm'; del.textContent = '删除';
      del.addEventListener('click', function () { deleteUserVolume(v.name); });
      act.appendChild(del);
      tr.appendChild(act);
      tbody.appendChild(tr);
    });
    var hint = el('v2-vl-user-empty');
    if (hint) { hint.hidden = vols.length > 0; }
  }

  function load() {
    if (!api) { return Promise.resolve(); }
    banner('');
    return api.json('GET', '/api/volumes')
      .then(function (body) { render(normalizeVolumes(body)); })
      .catch(function (e) { render([]); banner('卷列表读取失败：' + (e.message || e), 'warn'); });
  }

  function loadUser() {
    if (!api) { return Promise.resolve(); }
    return api.json('GET', '/api/volumes/user')
      .then(function (body) { renderUserVolumes(normalizeUserVolumes(body)); })
      .catch(function () { renderUserVolumes([]); });
  }

  function refresh(name) {
    var path = '/api/volumes/refresh' + (name ? '?volume=' + encodeURIComponent(name) : '');
    api.json('POST', path).then(function () {
      if (window.v2toast) { window.v2toast('ok', name ? ('已刷新 ' + name) : '已刷新全部卷'); }
      load();
    }).catch(function (e) { banner('刷新失败：' + (e.message || e), 'warn'); });
  }

  function normalizeSecrets(body) {
    var list = body && Array.isArray(body.secrets) ? body.secrets : [];
    return list.filter(function (n) { return typeof n === 'string' && n; });
  }

  function renderSecrets(names) {
    var tbody = el('v2-sec-body');
    if (!tbody) { return; }
    tbody.textContent = '';
    names.forEach(function (name) {
      var tr = document.createElement('tr');
      var n = document.createElement('td'); n.className = 'mono'; n.textContent = name;
      tr.appendChild(n);
      var act = document.createElement('td'); act.className = 'r actions';
      var del = document.createElement('button');
      del.type = 'button'; del.className = 'btn btn-ghost btn-sm'; del.textContent = '删除';
      del.addEventListener('click', function () { deleteSecret(name); });
      act.appendChild(del);
      tr.appendChild(act);
      tbody.appendChild(tr);
    });
    var empty = el('v2-sec-empty');
    if (empty) { empty.hidden = names.length > 0; }
  }

  function secBanner(text, kind) {
    var box = el('v2-sec-state');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  function loadSecrets() {
    if (!api) { return Promise.resolve(); }
    secBanner('');
    return api.json('GET', '/api/secrets')
      .then(function (body) { renderSecrets(normalizeSecrets(body)); })
      .catch(function (e) {
        renderSecrets([]);
        secBanner('Secret 卷不可用：' + (e.message || e), 'warn');
      });
  }

  function createSecret() {
    var name = window.prompt('Secret 名称（字母/数字/-/_）');
    if (!name) { return; }
    api.json('POST', '/api/secrets', {
      body: new TextEncoder().encode(JSON.stringify({ name: name.trim(), mode: 'random', origin: 'webui' })),
      contentType: 'application/json'
    }).then(function (res) {
      if (res && res.value) {
        window.prompt('密钥只显示一次，请立即备份（Ctrl+C 复制）', res.value);
      }
      if (window.v2toast) { window.v2toast('ok', '已创建 secret ' + (res.name || name)); }
      loadSecrets();
    }).catch(function (e) { secBanner('创建失败：' + (e.message || e), 'warn'); });
  }

  function deleteSecret(name) {
    if (!window.confirm('删除 secret ' + name + '？删除后不可恢复。')) { return; }
    api.json('DELETE', '/api/secrets/' + encodeURIComponent(name))
      .then(function () {
        if (window.v2toast) { window.v2toast('ok', '已删除 ' + name); }
        loadSecrets();
      })
      .catch(function (e) { secBanner('删除失败：' + (e.message || e), 'warn'); });
  }

  function deleteUserVolume(name) {
    if (!window.confirm('删除用户卷 ' + name + '？（卷内文件将一并移除）')) { return; }
    api.json('DELETE', '/api/volumes/user?name=' + encodeURIComponent(name))
      .then(function () {
        if (window.v2toast) { window.v2toast('ok', '已删除卷 ' + name); }
        load(); loadUser();
      })
      .catch(function (e) { banner('删除卷失败：' + (e.message || e), 'warn'); });
  }

  function createUserVolume() {
    var name = window.prompt('卷名称（字母/数字/-/_）');
    if (!name) { return; }
    var type = window.prompt('类型（如 webdav / baidupcs）', 'webdav');
    if (!type) { return; }
    var cap = window.prompt('容量字节（0 = 不限）', '0');
    if (cap === null) { return; }
    var extra = window.prompt('extra JSON（可选，例如 {"url":"https://example.com/dav"}）', '{}');
    if (extra === null) { return; }
    var body = { name: name.trim(), type: type.trim(), capacity: Math.max(0, Number(cap) || 0) };
    try { body.extra = extra.trim() ? JSON.parse(extra) : {}; } catch (e) { banner('extra 不是合法 JSON', 'warn'); return; }
    api.json('POST', '/api/volumes/user', {
      body: new TextEncoder().encode(JSON.stringify(body)),
      contentType: 'application/json'
    }).then(function () {
      if (window.v2toast) { window.v2toast('ok', '已创建卷 ' + name); }
      load(); loadUser();
    }).catch(function (e) { banner('创建卷失败：' + (e.message || e), 'warn'); });
  }

  var inited = false;

  function init() {
    if (inited) { load(); loadUser(); loadSecrets(); return; }
    inited = true;
    var r = el('v2-vl-refresh'); if (r) { r.addEventListener('click', function () { refresh(''); }); }
    var ur = el('v2-vl-user-refresh'); if (ur) { ur.addEventListener('click', loadUser); }
    var sr = el('v2-sec-refresh'); if (sr) { sr.addEventListener('click', loadSecrets); }
    var sc = el('v2-sec-create'); if (sc) { sc.addEventListener('click', createSecret); }
    var vc = el('v2-vl-create'); if (vc) { vc.addEventListener('click', createUserVolume); }
    if (el('v2-vl-body')) { load(); loadUser(); loadSecrets(); }
  }

  if (typeof window !== 'undefined') { window.v2volumes = { init: init, load: load, loadUser: loadUser, normalizeVolumes: normalizeVolumes, normalizeUserVolumes: normalizeUserVolumes }; }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { normalizeVolumes: normalizeVolumes, normalizeUserVolumes: normalizeUserVolumes, normalizeSecrets: normalizeSecrets };
  }
})();
