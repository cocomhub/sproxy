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

  /**
   * 解析列表响应里的分页元信息。
   *
   * 背景：前端曾固定 `limit=200` 且**忽略**响应里的 total —— 目录超过 200 项时静默截断，
   * 用户以为文件丢了。这里把 total/offset/limit 归一为「是否还有更多」。
   * 服务端不返回 total（搜索旧路径）时按「返回条数 == limit」推断。
   */
  function parseListMeta(payload) {
    var files = payload && Array.isArray(payload.files) ? payload.files : [];
    var limit = Number(payload && payload.limit) || 0;
    var offset = Number(payload && payload.offset) || 0;
    // 注意 Number(null) === 0：必须先判「字段是否存在」，否则「未返回 total」会被当成 total=0。
    var hasTotalField = payload && payload.total !== null && payload.total !== undefined;
    var total = hasTotalField ? Number(payload.total) : NaN;
    if (!isFinite(total) || total < 0) { total = -1; }
    var hasTotal = total >= 0;
    var hasMore;
    if (hasTotal) {
      hasMore = offset + files.length < total;
    } else if (limit > 0) {
      hasMore = files.length >= limit;
    } else {
      hasMore = false;
    }
    return { count: files.length, offset: offset, limit: limit, total: total, hasTotal: hasTotal, hasMore: hasMore };
  }

  /**
   * 列头排序切换：点同一列翻转方向，换列从 asc 开始。
   * @param {{sort:string, order:string}} cur
   * @param {string} field
   * @returns {{sort:string, order:string}}
   */
  function nextSort(cur, field) {
    var c = cur || {};
    var f = ['name', 'size', 'time'].indexOf(field) === -1 ? 'name' : field;
    if (c.sort === f && c.order === 'asc') { return { sort: f, order: 'desc' }; }
    return { sort: f, order: 'asc' };
  }

  /** 排序箭头符号（列头展示用）。 */
  function sortMark(cur, field) {
    var c = cur || {};
    if (c.sort !== field) { return ''; }
    return c.order === 'desc' ? '↓' : '↑';
  }

  /**
   * 预览里的上一个/下一个**可预览文件**（跳过目录），按当前列表顺序。
   * 到边界返回 ''（调用方据此禁用按钮 / 不响应按键）。
   */
  function neighborFile(files, currentName, delta) {
    var list = Array.isArray(files) ? files : [];
    var idx = -1;
    for (var i = 0; i < list.length; i += 1) {
      if (list[i] && list[i].name === currentName) { idx = i; break; }
    }
    if (idx < 0) { return ''; }
    var step = delta < 0 ? -1 : 1;
    for (var j = idx + step; j >= 0 && j < list.length; j += step) {
      if (list[j] && !list[j].isDir) { return list[j].name; }
    }
    return '';
  }

  /** 拼接列表请求路径（分页 + 排序 + 卷过滤 + 搜索）。 */
  function listURL(q) {
    var o = q || {};
    var params = [];
    var push = function (k, v) { if (v !== '' && v !== undefined && v !== null) { params.push(k + '=' + encodeURIComponent(v)); } };
    if (o.query) {
      push('q', o.query);
      push('limit', o.limit);
      push('offset', o.offset);
      return '/api/files/search?' + params.join('&');
    }
    push('subdir', o.subdir || '');
    push('volume', o.volume || '');
    push('sort', o.sort || '');
    push('order', o.sort ? (o.order || 'asc') : '');
    push('limit', o.limit);
    push('offset', o.offset);
    return '/api/files' + (params.length ? '?' + params.join('&') : '');
  }

  /**
   * 归一排序偏好（从存储/URL 读到的任意值）：非法字段回落 name、非法方向回落 asc。
   *
   * 排序是**瞬时视图偏好**（用户上次点过哪个列头），不是策略配置，所以只做本地记忆，
   * 不进 `/api/ui/prefs`——策略配置需要用户去设置页发现并维护，收益不抵成本。
   * @param {{sort?:string, order?:string}|null} raw
   * @returns {{sort:string, order:string}}
   */
  function normalizeSort(raw) {
    var r = raw || {};
    var sort = ['name', 'size', 'time'].indexOf(r.sort) >= 0 ? r.sort : 'name';
    var order = (r.order === 'asc' || r.order === 'desc') ? r.order : 'asc';
    return { sort: sort, order: order };
  }

  /**
   * 是否允许向当前视图上传：必须有**具体卷**。
   *
   * 「全部卷」是多卷**聚合视图**（同一子目录下多卷条目混排）⇒ 上传落哪张卷由服务端 placement
   * 自动决定，用户看到的是聚合结果，切卷过滤后文件就"消失"了。上传目标必须可见、可预期，
   * 因此聚合视图禁用上传，提示先选具体卷。
   * @param {string} volume 当前卷过滤（空 = 全部卷）
   * @returns {boolean}
   */
  function canUploadTo(volume) {
    return String(volume || '').trim() !== '';
  }

  return {
    formatSize: formatSize,
    formatTime: formatTime,
    normalizeFiles: normalizeFiles,
    crumbsFor: crumbsFor,
    joinSubdir: joinSubdir,
    resolveRel: resolveRel,
    parentSubdir: parentSubdir,
    parseListMeta: parseListMeta,
    nextSort: nextSort,
    sortMark: sortMark,
    neighborFile: neighborFile,
    listURL: listURL,
    normalizeSort: normalizeSort,
    canUploadTo: canUploadTo
  };
});
