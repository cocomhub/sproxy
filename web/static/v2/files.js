// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 文件列表（P1a）：解析/格式化/排序为纯函数；DOM 渲染与请求单独实现。
'use strict';

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('../shared/format.js'));
  } else {
    root.v2files = factory(root.sharedFormat);
  }
})(typeof self !== 'undefined' ? self : this, function (fmt) {
  if (!fmt) { throw new Error('files.js 需要 shared/format.js（请在之前加载）'); }
  var formatSize = fmt.formatSize;
  var formatTime = fmt.formatTime;

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

  /**
   * 行名 → 服务端相对路径。
   * 搜索结果的 name 已是**完整相对路径**（服务端按租户根返回），不得再拼 subdir
   * （否则 v2demo + v2demo/e2e.txt 变成 v2demo/v2demo/e2e.txt ⇒ 删除/预览/详情全部 404）。
   */
  function resolveRel(subdir, name, isSearch) {
    if (isSearch) { return joinSubdir('', name); }
    return joinSubdir(subdir, name);
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
    resolveRel: resolveRel,
    parentSubdir: parentSubdir
  };
});
