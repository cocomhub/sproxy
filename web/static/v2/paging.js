// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 列表分页的通用纯函数（云任务 / 任务组 / 其它 {items, total} 形态的接口共用）。
//
// 背景（2026-11-26）：传输页固定 `limit=100`（云任务）与 `limit=50`（任务组）并**完全忽略**
// 响应里的 `total` ⇒ 条目超过一页时静默截断，筛选 chip 的计数也只是「已加载子集」——
// 用户看到「失败 0」却实际有失败任务。这里把它们收敛为同一套「已加载 / 共 N + 加载更多」。
'use strict';

(function (root, factory) {
  var api = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  } else {
    root.v2paging = api;
  }
})(typeof self !== 'undefined' ? self : this, function () {
  /**
   * 解析列表响应的分页元信息。
   *
   * `total === -1` 表示**服务端未返回 total**（老契约）：此时只能按「本页满页 ⇒ 可能还有」推断，
   * 不编造总数（UI 上不得显示「共 N」）。
   *
   * @param {object|null} body 响应体（{items|tasks|groups, total}）
   * @param {string} key 数组字段名
   * @param {number} loadedBefore 本次响应之前已加载的条数（追加时为外层的已有长度）
   * @returns {{pageCount:number, loaded:number, total:number, hasTotal:boolean, hasMore:boolean}}
   */
  function pageMeta(body, key, loadedBefore) {
    var list = body && Array.isArray(body[key]) ? body[key] : [];
    var before = Number(loadedBefore) || 0;
    var loaded = before + list.length;
    var raw = body ? body.total : undefined;
    var hasTotalField = raw !== null && raw !== undefined && raw !== '';
    var total = hasTotalField ? Number(raw) : NaN;
    if (!isFinite(total) || total < 0) { total = -1; }
    var hasTotal = total >= 0;
    var hasMore;
    if (hasTotal) {
      hasMore = loaded < total;
    } else {
      // 无 total：本页是否满页由调用方通过 pageSize 判断（见 inferHasMore）。
      hasMore = false;
    }
    return { pageCount: list.length, loaded: loaded, total: total, hasTotal: hasTotal, hasMore: hasMore };
  }

  /**
   * 没有 total 时的保守判断：本页条数 == 请求的 limit ⇒ 认为可能还有更多。
   * @param {number} pageCount 本页条数
   * @param {number} limit 请求的 limit
   * @returns {boolean}
   */
  function inferHasMore(pageCount, limit) {
    return Number(limit) > 0 && Number(pageCount) >= Number(limit);
  }

  /**
   * 构造带分页的列表 URL（保持既有查询参数不变，只追加 offset/limit）。
   * @param {string} path 已含查询串或以 ? 结尾的路径
   * @param {number} offset
   * @param {number} limit
   * @returns {string}
   */
  function pageURL(path, offset, limit) {
    var p = String(path || '');
    // 路径已以 '?' 结尾（空查询串）⇒ 直接拼，避免出现 '/x?&limit=10'。
    var sep = p.indexOf('?') < 0 ? '?' : (p.charAt(p.length - 1) === '?' ? '' : '&');
    var q = [];
    if (Number(limit) > 0) { q.push('limit=' + encodeURIComponent(String(limit))); }
    if (Number(offset) > 0) { q.push('offset=' + encodeURIComponent(String(offset))); }
    return q.length ? (p + sep + q.join('&')) : p;
  }

  /**
   * 「已加载 X / 共 Y」文案；无 total 时只说已加载条数（**不编造**总数）。
   * @param {{loaded:number, total:number, hasTotal:boolean}} meta
   * @returns {string}
   */
  function loadedText(meta) {
    var m = meta || {};
    var loaded = Number(m.loaded) || 0;
    if (!m.hasTotal) { return loaded + ' 条'; }
    return loaded + ' / ' + Number(m.total) + ' 条';
  }

  /**
   * 筛选计数是否需要标注「仅已加载」——未全部加载时，筛选后的计数会低估真实值，
   * 必须显式提示，否则就是新的假数据。
   * @param {{hasTotal:boolean, hasMore:boolean}} meta
   * @returns {boolean}
   */
  function partial(meta) {
    var m = meta || {};
    return !!(m.hasMore || !m.hasTotal);
  }

  return {
    pageMeta: pageMeta,
    inferHasMore: inferHasMore,
    pageURL: pageURL,
    loadedText: loadedText,
    partial: partial
  };
});
