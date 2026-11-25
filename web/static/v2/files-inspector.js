// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 文件检查器（P1c）：概览 / Meta / 版本 / 分享。只读优先，分享可创建。
'use strict';

(function () {
  if (typeof document === 'undefined') { return; }
  var api = window.v2api, f = window.v2files;
  if (!api || !f) { return; }

  var state = { name: '', file: null, tab: 'info', loaded: {} };

  function el(id) { return document.getElementById(id); }
  function enc(s) { return encodeURIComponent(s); }
  function view() { return window.v2filesView; }
  function relPath() {
    var sub = view() ? view().subdir() : '';
    return f.joinSubdir(sub, state.name);
  }

  function showTab(tab) {
    state.tab = tab;
    Array.prototype.forEach.call(document.querySelectorAll('[data-drtab]'), function (b) {
      b.setAttribute('aria-selected', String(b.getAttribute('data-drtab') === tab));
    });
    Array.prototype.forEach.call(document.querySelectorAll('[data-drpane]'), function (p) {
      p.hidden = p.getAttribute('data-drpane') !== tab;
    });
    if (tab === 'meta' && !state.loaded.meta) { loadMeta(); }
    if (tab === 'ver' && !state.loaded.ver) { loadVersions(); }
    if (tab === 'share' && !state.loaded.share) { loadShares(); }
  }

  function kv(dl, rows) {
    dl.textContent = '';
    rows.forEach(function (r) {
      var dt = document.createElement('dt');
      dt.textContent = r[0];
      var dd = document.createElement('dd');
      dd.textContent = r[1] == null || r[1] === '' ? '—' : String(r[1]);
      if (r[2]) { dd.className = r[2]; }
      dl.appendChild(dt);
      dl.appendChild(dd);
    });
  }

  function renderInfo() {
    var file = state.file || {};
    kv(el('v2-dr-info'), [
      ['路径', relPath(), 'mono'],
      ['类型', file.isDir ? '目录' : '文件'],
      ['卷', file.volume || '—'],
      ['大小', file.isDir ? '—' : f.formatSize(file.size), 'num'],
      ['修改时间', f.formatTime(file.modTime), 'num'],
      ['校验', file.checksum || '—', 'mono']
    ]);
  }

  function setPane(id, kind, text) {
    var box = el(id);
    if (!box) { return; }
    box.hidden = false;
    box.className = 'banner' + (kind === 'error' ? ' banner-warn' : '');
    box.textContent = text;
  }

  function clearPane(id) {
    var box = el(id);
    if (box) { box.hidden = true; box.textContent = ''; }
  }

  /** 保存 extra：mode merge/replace/delete；删除模式只取键名。 */
  function saveMeta() {
    var editor = el('v2-meta-edit');
    var modeSel = el('v2-meta-mode');
    var mode = modeSel ? modeSel.value : 'merge';
    var text = (editor && editor.value || '').trim();
    if (!text) { setPane('v2-meta-state', 'error', 'extra 不能为空'); return; }
    var parsed;
    try { parsed = JSON.parse(text); } catch (e) { setPane('v2-meta-state', 'error', 'JSON 非法：' + e.message); return; }
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      setPane('v2-meta-state', 'error', 'extra 必须是 JSON 对象');
      return;
    }
    var payload = { extra: parsed };
    if (mode === 'delete') {
      var keys = {};
      Object.keys(parsed).forEach(function (k) { keys[k] = null; });
      payload.extra = keys;
    }
    if (mode !== 'merge') { payload.mode = mode; }
    clearPane('v2-meta-state');
    api.json('PUT', '/api/files/meta?filename=' + enc(relPath()), {
      body: new TextEncoder().encode(JSON.stringify(payload)),
      contentType: 'application/json'
    }).then(function () {
      if (window.v2toast) { window.v2toast('ok', 'meta 已保存（' + mode + '）'); }
      state.loaded.meta = false;
      var ed = el('v2-meta-edit'); if (ed) { ed.value = ''; }
      loadMeta();
    }).catch(function (e) { setPane('v2-meta-state', 'error', '保存失败：' + (e.message || e)); });
  }

  function loadMeta() {
    state.loaded.meta = true;
    var pre = el('v2-meta-json');
    pre.textContent = '加载中…';
    clearPane('v2-meta-state');
    api.json('GET', '/api/files/meta?filename=' + enc(relPath()))
      .then(function (body) {
        var extra = body && body.extra ? body.extra : body;
        pre.textContent = JSON.stringify(extra, null, 2);
        var editor = el('v2-meta-edit');
        if (editor && !editor.value) {
          try { editor.value = JSON.stringify(extra || {}, null, 2); } catch (e) { editor.value = '{}'; }
        }
      })
      .catch(function (err) {
        pre.textContent = '—';
        setPane('v2-meta-state', 'error', 'Meta 加载失败：' + (err.message || err));
      });
  }

  function loadVersions() {
    state.loaded.ver = true;
    var box = el('v2-ver-list');
    box.textContent = '加载中…';
    api.json('GET', '/api/versions?filename=' + enc(relPath()))
      .then(function (body) {
        var list = (body && (body.versions || body.items)) || [];
        box.textContent = '';
        if (!list.length) { box.textContent = '暂无历史版本（需服务端开启 versioning）。'; box.className = 'muted'; return; }
        box.className = '';
        list.forEach(function (v) {
          var id = v.version_id || v.id || v.version;
          var row = document.createElement('div');
          row.className = 'ver-row';
          var meta = document.createElement('div');
          meta.className = 'mono';
          meta.textContent = 'v' + id + ' · ' + f.formatTime(v.mod_time || v.created_at) + ' · ' + f.formatSize(v.size);
          var acts = document.createElement('div');
          var restore = document.createElement('button');
          restore.className = 'btn btn-sm';
          restore.type = 'button';
          restore.textContent = '恢复';
          restore.addEventListener('click', function () {
            api.json('POST', '/api/versions/restore?filename=' + enc(relPath()) + '&version_id=' + enc(id), {})
              .then(function () { if (window.v2toast) { window.v2toast('ok', '已恢复 v' + id); } state.loaded.ver = false; loadVersions(); })
              .catch(function (e) { if (window.v2toast) { window.v2toast('bad', '恢复失败：' + e.message); } });
          });
          var del = document.createElement('button');
          del.className = 'btn btn-sm btn-ghost';
          del.type = 'button';
          del.textContent = '删除';
          del.addEventListener('click', function () {
            api.json('DELETE', '/api/versions?filename=' + enc(relPath()) + '&version_id=' + enc(id))
              .then(function () { if (window.v2toast) { window.v2toast('ok', '已删除 v' + id); } state.loaded.ver = false; loadVersions(); })
              .catch(function (e) { if (window.v2toast) { window.v2toast('bad', '删除失败：' + e.message); } });
          });
          acts.appendChild(restore);
          acts.appendChild(del);
          row.appendChild(meta);
          row.appendChild(acts);
          box.appendChild(row);
        });
      })
      .catch(function (err) {
        box.textContent = '版本加载失败：' + (err.message || err);
        box.className = 'muted';
      });
  }

  function loadShares() {
    state.loaded.share = true;
    var box = el('v2-share-list');
    box.textContent = '加载中…';
    api.json('GET', '/api/shares')
      .then(function (body) {
        var list = (body && (body.shares || body.items)) || [];
        var mine = list.filter(function (s) { return s && s.filename === relPath(); });
        box.textContent = '';
        if (!mine.length) { box.textContent = '暂无分享链接。'; box.className = 'muted'; return; }
        box.className = '';
        mine.forEach(function (s) {
          var row = document.createElement('div');
          row.className = 'ver-row';
          var link = location.origin + '/s/' + (s.token || s.id || '');
          var a = document.createElement('a');
          a.href = link; a.textContent = link; a.className = 'mono';
          var meta = document.createElement('div');
          meta.className = 'muted';
          meta.style.fontSize = '11.5px';
          meta.textContent = '有效期 ' + (s.expire_at || s.expires_at || '—') + ' · 下载 ' + (s.downloads || 0);
          var revoke = document.createElement('button');
          revoke.className = 'btn btn-sm btn-ghost';
          revoke.type = 'button';
          revoke.textContent = '撤销';
          revoke.addEventListener('click', function () {
            api.json('DELETE', '/api/shares/' + enc(s.token || s.id))
              .then(function () { if (window.v2toast) { window.v2toast('ok', '已撤销分享'); } state.loaded.share = false; loadShares(); })
              .catch(function (e) { if (window.v2toast) { window.v2toast('bad', '撤销失败：' + e.message); } });
          });
          row.appendChild(a);
          row.appendChild(meta);
          row.appendChild(revoke);
          box.appendChild(row);
        });
      })
      .catch(function (err) {
        box.textContent = '分享列表加载失败：' + (err.message || err);
        box.className = 'muted';
      });
  }

  function createShare() {
    var ttl = Number(el('v2-share-ttl').value) || 0;
    var maxDl = Number(el('v2-share-max').value) || 0;
    var one = el('v2-share-one').checked;
    var body = { filename: relPath() };
    if (ttl) { body.ttl = ttl; }
    if (maxDl) { body.max_downloads = maxDl; }
    if (one) { body.one_time = true; }
    api.json('POST', '/api/shares', {
      body: new TextEncoder().encode(JSON.stringify(body)),
      contentType: 'application/json'
    }).then(function (resp) {
      var token = (resp && (resp.token || (resp.share && resp.share.token))) || '';
      var out = el('v2-share-result');
      if (!token) { out.textContent = '已创建，但响应中没有 token'; return; }
      var link = location.origin + '/s/' + token;
      out.textContent = '';
      var a = document.createElement('a');
      a.href = link; a.textContent = link; a.className = 'mono';
      out.appendChild(document.createTextNode('已创建：'));
      out.appendChild(a);
      state.loaded.share = false;
      loadShares();
    }).catch(function (e) {
      if (window.v2toast) { window.v2toast('bad', '创建分享失败：' + e.message); }
    });
  }

  function open(name) {
    state.name = name;
    state.file = view() ? view().file(name) : null;
    state.loaded = {};
    var drawer = el('v2-drawer');
    if (!drawer) { return; }
    el('v2-dr-title').textContent = name;
    renderInfo();
    showTab('info');
    clearPane('v2-meta-state');
    drawer.classList.add('show');
    drawer.setAttribute('aria-hidden', 'false');
    el('v2-dr-backdrop').hidden = false;
    document.body.classList.add('dr-open');
  }

  function close() {
    var drawer = el('v2-drawer');
    if (!drawer) { return; }
    drawer.classList.remove('show');
    drawer.setAttribute('aria-hidden', 'true');
    el('v2-dr-backdrop').hidden = true;
    document.body.classList.remove('dr-open');
  }

  function init() {
    Array.prototype.forEach.call(document.querySelectorAll('[data-drtab]'), function (b) {
      b.addEventListener('click', function () { showTab(b.getAttribute('data-drtab')); });
    });
    el('v2-dr-close').addEventListener('click', close);
    el('v2-dr-backdrop').addEventListener('click', close);
    el('v2-meta-reload').addEventListener('click', function () { state.loaded.meta = false; loadMeta(); });
    var save = el('v2-meta-save');
    if (save) { save.addEventListener('click', saveMeta); }
    el('v2-share-create').addEventListener('click', createShare);
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { close(); } });
  }

  window.v2inspector = { open: open, close: close };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
