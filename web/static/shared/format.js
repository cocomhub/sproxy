// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// shared/format.js —— 前端共用的展示格式化（**单一事实源**）。
//
// 历史背景：`app-render.js` / `quota-format.js` / `user-volumes.js` 各有一份
// formatSize（精度/上限不同）。本模块是 v2 及后续新代码的唯一实现：
//   - 2 位小数（B 档取整）、支持到 PB；
//   - formatTime 接受 UnixNano（服务端 mod_time 形态）与毫秒时间戳。
// 旧模块暂不强制迁移（改显示精度会影响旧页快照），在旧页下线批次统一收编。
//
// UMD：浏览器挂 `sharedFormat`，Node 可 require。
'use strict';

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.sharedFormat = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  var UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];

  /** 人类可读大小；null/负数/非法 → '—'（0 → '0 B'）。 */
  function formatSize(n) {
    if (n == null) { return '—'; }
    var v = Number(n);
    if (!isFinite(v) || v < 0) { return '—'; }
    if (v === 0) { return '0 B'; }
    var i = 0;
    while (v >= 1024 && i < UNITS.length - 1) { v /= 1024; i += 1; }
    return (i === 0 ? String(Math.round(v)) : v.toFixed(2)) + ' ' + UNITS[i];
  }

  /** 时间戳 → 本地 MM-DD HH:mm；自动识别 UnixNano / 毫秒；0/非法 → '—'。 */
  function formatTime(ts) {
    var v = Number(ts);
    if (!isFinite(v) || v <= 0) { return '—'; }
    var ms = v > 1e15 ? Math.floor(v / 1e6) : v; // > 1e15 视为纳秒
    var d = new Date(ms);
    if (isNaN(d.getTime())) { return '—'; }
    var p = function (x) { return x < 10 ? '0' + x : String(x); };
    return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  return { formatSize: formatSize, formatTime: formatTime };
});
