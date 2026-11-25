// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 控制台「分享」分区：全部分享链接列表（状态/口令/一次性/水印）+ 复制 + 撤销。
// 旧页有「管理分享」全局视图，v2 此前只能在单文件检查器里看该文件的链接 —— 页面级缺口补齐。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;
  var fmt = typeof window === 'undefined' ? null : window.sharedFormat;

  function el(id) { return document.getElementById(id); }

  function banner(text, kind) {
    var box = el('v2-sh-banner');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  /** 归一分享项：服务端 GET /api/shares 的字段 + 派生的「有无口令/剩余次数」。 */
  function normalizeShares(body) {
    var list = body && Array.isArray(body.shares) ? body.shares : [];
    return list.filter(function (s) { return s && s.token; }).map(function (s) {
      var max = Number(s.max_downloads) || 0;
      var used = Number(s.downloads) || 0;
      return {
        token: s.token, filename: s.filename || '', volume: s.volume || '',
        createdAt: s.created_at || '', expiresAt: s.expires_at || '',
        maxDownloads: max, downloads: used, oneTime: !!s.one_time,
        readonly: !!s.readonly, watermark: s.watermark || '',
        expired: !!s.expired,
        hasPassword: !!s.password_protected
      };
    });
  }

  /** 状态标签与 pill 类（口令保护不一定过期，单独一维）。 */
  function statusOf(s) {
    if (s.expired) { return { text: '已失效', cls: 'pill-mute' }; }
    if (s.oneTime) { return { text: '一次性', cls: 'pill-warn' }; }
    return { text: '有效', cls: 'pill-ok' };
  }

  function matches(s, filter) {
    if (!filter) { return true; }
    if (filter === 'active') { return !s.expired; }
    if (filter === 'expired') { return s.expired; }
    if (filter === 'ot') { return s.oneTime; }
    if (filter === 'pw') { return s.hasPassword; }
    return true;
  }

  function render(all) {
    var tbody = el('v2-sh-body');
    if (!tbody) { return; }
    var filter = ((el('v2-sh-state') || {}).value) || '';
    var list = all.filter(function (s) { return matches(s, filter); });
    tbody.textContent = '';
    list.forEach(function (s) {
      var tr = document.createElement('tr');
      var name = document.createElement('td');
      name.className = 'mono';
      name.textContent = s.filename || '—';
      tr.appendChild(name);

      var vol = document.createElement('td');
      vol.textContent = s.volume || '—';
      tr.appendChild(vol);

      var st = document.createElement('td');
      var info = statusOf(s);
      var pill = document.createElement('span');
      pill.className = 'pill ' + info.cls;
      pill.textContent = info.text;
      st.appendChild(pill);
      if (s.hasPassword) {
        var pw = document.createElement('span');
        pw.className = 'pill pill-info';
        pw.style.marginLeft = '4px';
        pw.textContent = '口令';
        st.appendChild(pw);
      }
      tr.appendChild(st);

      var dl = document.createElement('td');
      dl.className = 'r num';
      dl.textContent = String(s.downloads) + (s.maxDownloads > 0 ? '/' + s.maxDownloads : '');
      tr.appendChild(dl);

      var exp = document.createElement('td');
      exp.className = 'num muted';
      exp.textContent = s.expiresAt && fmt ? fmt.formatTime(Date.parse(s.expiresAt)) : '—';
      tr.appendChild(exp);

      var act = document.createElement('td');
      act.className = 'r actions';
      act.appendChild(btn('复制', function () { copyLink(s); }));
      act.appendChild(btn('撤销', function () { revoke(s); }));
      tr.appendChild(act);

      tbody.appendChild(tr);
    });
    var empty = el('v2-sh-empty');
    if (empty) { empty.hidden = list.length > 0; }
  }

  function btn(text, handler) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn btn-sm btn-ghost';
    b.textContent = text;
    b.addEventListener('click', handler);
    return b;
  }

  function shareURL(token) {
    return window.location.origin + '/s/' + token;
  }

  function copyLink(s) {
    var url = shareURL(s.token);
    if (navigator.clipboard) {
      navigator.clipboard.writeText(url).then(function () {
        if (window.v2toast) { window.v2toast('ok', s.hasPassword ? '链接已复制（需口令）' : '链接已复制'); }
      }).catch(function () { if (window.v2toast) { window.v2toast('warn', url); } });
      return;
    }
    if (window.v2toast) { window.v2toast('warn', url); }
  }

  function revoke(s) {
    if (!window.confirm('撤销分享 ' + (s.filename || s.token) + '？撤销后链接立即失效。')) { return; }
    api.json('DELETE', '/api/shares/' + encodeURIComponent(s.token))
      .then(function () {
        if (window.v2toast) { window.v2toast('ok', '已撤销'); }
        load();
      })
      .catch(function (e) { banner('撤销失败：' + (e.message || e), 'warn'); });
  }

  var all = [];

  function load() {
    if (!api) { return Promise.resolve(); }
    banner('');
    return api.json('GET', '/api/shares')
      .then(function (body) {
        all = normalizeShares(body);
        render(all);
      })
      .catch(function (e) {
        all = [];
        render(all);
        banner('分享列表加载失败：' + (e.message || e), 'warn');
      });
  }

  var inited = false;

  function init() {
    if (inited) { load(); return; }
    inited = true;
    var r = el('v2-sh-refresh'); if (r) { r.addEventListener('click', load); }
    var f = el('v2-sh-state'); if (f) { f.addEventListener('change', function () { render(all); }); }
    if (el('v2-sh-body')) { load(); }
  }

  if (typeof window !== 'undefined') { window.v2shares = { init: init, load: load }; }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { normalizeShares: normalizeShares, statusOf: statusOf, matches: matches };
  }
})();
