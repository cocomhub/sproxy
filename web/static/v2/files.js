// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 文件列表（P1a）：解析/格式化/排序为纯函数；DOM 渲染与请求单独实现。
'use strict';

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.v2files = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  var UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];

  /** 人类可读大小；负数/非法按 0。 */
  function formatSize(n) {
    var v = Number(n);
    if (!isFinite(v) || v <= 0) { return v === 0 ? '0 B' : '—'; }
    var i = 0;
    while (v >= 1024 && i < UNITS.length - 1) { v /= 1024; i += 1; }
    return (i === 0 ? String(Math.round(v)) : v.toFixed(2)) + ' ' + UNITS[i];
  }

  /** UnixNano → 本地 MM-DD HH:mm；0/非法返回 '—'。 */
  function formatTime(ns) {
    var v = Number(ns);
    if (!isFinite(v) || v <= 0) { return '—'; }
    var d = new Date(Math.floor(v / 1e6));
    if (isNaN(d.getTime())) { return '—'; }
    var p = function (x) { return x < 10 ? '0' + x : String(x); };
    return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  /** 规范化列表响应：目录在前、名称自然排序；坏数据返回空列表。 */
  function normalizeFiles(payload) {
    var files = payload && Array.isArray(payload.files) ? payload.files : [];
    var out = files.filter(function (f) { return f && typeof f.name === 'string' && f.name; }).map(function (f) {
      return {
        name: f.name,
        size: Number(f.size) || 0,
        checksum: f.checksum || '',
        modTime: Number(f.mod_time) || 0,
        isDir: !!f.is_dir,
        volume: f.volume || '',
        volumeCategory: f.volume_category || ''
      };
    });
    out.sort(function (a, b) {
      if (a.isDir !== b.isDir) { return a.isDir ? -1 : 1; }
      return a.name.localeCompare(b.name, 'zh-CN', { numeric: true });
    });
    return out;
  }

  /** 面包屑：全部卷 + 每级目录（path 为累计 subdir）。 */
  function crumbsFor(subdir) {
    var parts = String(subdir || '').split('/').filter(Boolean);
    var out = [{ label: '全部卷', path: '' }];
    var acc = [];
    parts.forEach(function (p) {
      acc.push(p);
      out.push({ label: p, path: acc.join('/') });
    });
    return out;
  }

  /** 子目录拼接（拒绝 .. 与绝对路径，保持服务端 ValidateFilePath 语义）。 */
  function joinSubdir(base, name) {
    var segs = String(base || '').split('/').filter(Boolean);
    String(name || '').split('/').forEach(function (s) {
      if (!s || s === '.' || s === '..') { return; }
      segs.push(s);
    });
    return segs.join('/');
  }

  function parentSubdir(subdir) {
    var segs = String(subdir || '').split('/').filter(Boolean);
    segs.pop();
    return segs.join('/');
  }

  return {
    formatSize: formatSize,
    formatTime: formatTime,
    normalizeFiles: normalizeFiles,
    crumbsFor: crumbsFor,
    joinSubdir: joinSubdir,
    parentSubdir: parentSubdir
  };
});
