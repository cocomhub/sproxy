// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 链式下载：贴 N 个链接 → 建任务组 → 轮询组状态 → 全部完成后归档（全有或全无）。
// 编排在后端既有能力上（group + retry + archive require_all），本模块只管
// 「解析输入 / 建组 / 轮询 / 收尾动作」，关掉页面不影响服务端任务。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;

  /** 解析每行 `URL` 或 `URL<TAB>文件名`（与旧页/CLI 一致）；空行与 # 注释忽略。 */
  function parseLines(text) {
    var out = [];
    String(text == null ? '' : text).split(/\r?\n/).forEach(function (line) {
      var t = line.replace(/\s+$/, '');
      if (!t.trim() || t.trim().charAt(0) === '#') { return; }
      var parts = t.split('\t');
      var url = (parts[0] || '').trim();
      if (!url) { return; }
      out.push({ url: url, filename: (parts[1] || '').trim() });
    });
    return out;
  }

  /** 组进度摘要（completed/total + 是否可归档）。 */
  function groupProgress(g) {
    var total = Number(g && g.total_tasks) || 0;
    var done = Number(g && g.completed) || 0;
    var failed = Number(g && g.failed) || 0;
    var pct = total ? Math.round(done / total * 100) : 0;
    return { total: total, done: done, failed: failed, pct: pct, ready: total > 0 && done === total };
  }

  /** 归档名默认值：chain-<yyyymmdd-hhmmss>.tar.gz。 */
  function defaultArchiveName(now) {
    var d = now || new Date();
    var p = function (n) { return String(n).padStart(2, '0'); };
    return 'chain-' + d.getFullYear() + p(d.getMonth() + 1) + p(d.getDate()) + '-' +
      p(d.getHours()) + p(d.getMinutes()) + p(d.getSeconds()) + '.tar.gz';
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { parseLines: parseLines, groupProgress: groupProgress, defaultArchiveName: defaultArchiveName };
  }
  if (typeof window === 'undefined') { return; }

  function el(id) { return document.getElementById(id); }
  function toast(kind, msg) { if (window.v2toast) { window.v2toast(kind, msg); } }

  function openDialog() {
    var d = el('v2-chain-backdrop');
    if (!d) { return; }
    d.hidden = false;
    var a = el('v2-chain-archive');
    if (a && !a.value) { a.placeholder = defaultArchiveName(); }
    var u = el('v2-chain-urls');
    if (u) { u.focus(); }
  }

  function closeDialog() {
    var d = el('v2-chain-backdrop');
    if (d) { d.hidden = true; }
  }

  function submit() {
    var rows = parseLines((el('v2-chain-urls') || {}).value || '');
    if (!rows.length) { toast('warn', '至少一个下载链接'); return; }
    var name = 'chain-' + Date.now();
    // 服务端 urls 是对象数组（cloudfilename.Entry{url, filename}）；空 filename 表示自动命名。
    var body = { name: name, urls: rows.map(function (r) { return { url: r.url, filename: r.filename || '' }; }) };
    var vol = ((el('v2-chain-volume') || {}).value || '').trim();
    if (vol) { body.transfer = { volume: vol, path: '' }; }
    var archiveName = ((el('v2-chain-archive') || {}).value || '').trim() || defaultArchiveName();
    var autoArchive = !!(el('v2-chain-autoarchive') || {}).checked;
    api.json('POST', '/api/cloud/groups', {
      body: new TextEncoder().encode(JSON.stringify(body)), contentType: 'application/json'
    }).then(function (res) {
      closeDialog();
      var gid = (res && (res.id || res.group_id)) || '';
      toast('ok', '已创建链式下载组：' + name);
      if (window.v2chainRun) { window.v2chainRun({ groupID: gid, archiveName: archiveName, autoArchive: autoArchive }); }
      var ta = el('v2-chain-urls'); if (ta) { ta.value = ''; }
    }).catch(function (e) { toast('bad', '创建失败：' + (e.message || e)); });
  }

  var inited = false;
  function init() {
    if (inited) { return; }
    inited = true;
    var o = el('v2-chain-open'); if (o) { o.addEventListener('click', openDialog); }
    var c = el('v2-chain-cancel'); if (c) { c.addEventListener('click', closeDialog); }
    var k = el('v2-chain-ok'); if (k) { k.addEventListener('click', submit); }
    var back = el('v2-chain-backdrop');
    if (back) { back.addEventListener('click', function (e) { if (e.target === back) { closeDialog(); } }); }
  }

  window.v2chain = { init: init, open: openDialog, close: closeDialog, submit: submit,
    parseLines: parseLines, groupProgress: groupProgress, defaultArchiveName: defaultArchiveName };
  if (document.readyState === 'loading') { document.addEventListener('DOMContentLoaded', init); }
  init();
})();
