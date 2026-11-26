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
      var det = document.createElement('button');
      det.type = 'button'; det.className = 'btn btn-ghost btn-sm'; det.textContent = '详情';
      det.setAttribute('data-volume', v.name);
      det.addEventListener('click', function () { openDetail(v.name); });
      act.appendChild(det);
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
      var detU = document.createElement('button');
      detU.type = 'button'; detU.className = 'btn btn-ghost btn-sm'; detU.textContent = '详情';
      detU.setAttribute('data-volume', v.name);
      detU.addEventListener('click', function () { openDetail(v.name); });
      act.appendChild(detU);
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

  /** 打开卷详情抽屉（实现见 volume-drawer.js：分区由后端 settings[] 驱动）。 */
  function openDetail(name) {
    if (window.v2volumeDrawer) { return window.v2volumeDrawer.open(name); }
    return Promise.resolve();
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

  function buildVolumeDialog(types) {
    var wrap = document.createElement('div');
    wrap.className = 'modal-backdrop';
    var opts = types.map(function (t) {
      return '<option value="' + t + '">' + t + '</option>';
    }).join('');
    wrap.innerHTML = '<div class="modal" role="dialog" aria-modal="true">' +
      '<h3>创建用户卷</h3>' +
      '<label class="field">名称<input id="v2-vc-name" autocomplete="off" placeholder="alnum/-/_"></label>' +
      '<label class="field">类型<select id="v2-vc-type">' + opts + '</select></label>' +
      '<label class="field">容量（字节，0 = 不限）<input id="v2-vc-cap" inputmode="numeric" value="0"></label>' +
      '<label class="field">额外配置（每行 key=value）<textarea id="v2-vc-extra" rows="4" ' +
      'placeholder="url=https://example.com/dav"></textarea></label>' +
      '<div class="modal-actions"><button type="button" class="btn" data-vc="cancel">取消</button>' +
      '<button type="button" class="btn btn-primary" data-vc="ok">创建</button></div></div>';
    document.body.appendChild(wrap);
    return wrap;
  }

  /** 解析 "key=value" 行（忽略空行/# 注释；全部按字符串提交，数字/布尔由服务端解析）。 */
  function parseKV(text) {
    var out = {};
    String(text || '').split(new RegExp('\r?\n')).forEach(function (line) {
      var t = line.trim();
      if (!t || t.charAt(0) === '#') { return; }
      var i = t.indexOf('=');
      if (i <= 0) { return; }
      out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
    });
    return out;
  }

  function createUserVolume() {
    var types = ['webdav', 'baidupcs', 's3', 'sftp'];
    var wrap = buildVolumeDialog(types);
    var close = function () { wrap.remove(); };
    wrap.addEventListener('click', function (e) { if (e.target === wrap) { close(); } });
    wrap.querySelector('[data-vc=cancel]').addEventListener('click', close);
    wrap.querySelector('[data-vc=ok]').addEventListener('click', function () {
      var name = (wrap.querySelector('#v2-vc-name').value || '').trim();
      var type = wrap.querySelector('#v2-vc-type').value;
      var cap = Number(wrap.querySelector('#v2-vc-cap').value) || 0;
      var extra = parseKV(wrap.querySelector('#v2-vc-extra').value);
      if (!name) { banner('卷名称必填', 'warn'); return; }
      close();
      api.json('POST', '/api/volumes/user', {
        body: new TextEncoder().encode(JSON.stringify({
          name: name, type: type, capacity: Math.max(0, cap), extra: extra
        })),
        contentType: 'application/json'
      }).then(function () {
        if (window.v2toast) { window.v2toast('ok', '已创建卷 ' + name); }
        load(); loadUser();
      }).catch(function (e) { banner('创建卷失败：' + (e.message || e), 'warn'); });
    });
    var n = wrap.querySelector('#v2-vc-name');
    if (n) { n.focus(); }
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

  if (typeof window !== 'undefined') { window.v2volumes = { init: init, load: load, loadUser: loadUser, openDetail: openDetail, normalizeVolumes: normalizeVolumes, normalizeUserVolumes: normalizeUserVolumes }; }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { normalizeVolumes: normalizeVolumes, normalizeUserVolumes: normalizeUserVolumes, normalizeSecrets: normalizeSecrets, parseKV: parseKV };
  }
})();
