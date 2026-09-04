/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * audit.js —— sclient 领域 API：审计日志查看（Web UI 审计面板）。
 *
 * 闭包式工厂：api/audit.js(ctx) → { ...方法 }，ctx = { coreRequest, config, log,
 * crypto, util }（由 api/index.js 组装传入）。领域方法一律 promise。
 *
 * 端点语义对齐 server audit_handler.go：
 *   - list(opts)   GET /api/audit?limit=&action=&actor=&mesh=&since=
 *     opts 可选：limit（int，服务端 clamp ≤500）、action/actor/mesh（精确相等过滤）、
 *     since（RFC3339 时间串）。响应 {events:[...], total}，events 时间倒序。
 *
 * 说明：/api/audit 同时注册主 mux（authMiddleware/SproxySig）与 localMux（隧道
 * 内层，隧道加密即认证）——**隧道与 direct 模式均可达**，浏览器审计 tab 两条
 * 传输路径都能正常拉取。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('../util.js'));
  } else {
    root.sclientApiAudit = factory(root.sclientUtil);
  }
})(typeof self !== 'undefined' ? self : this, function (utilLib) {
  'use strict';

  // ---- 领域方法工厂 ----
  return function createAuditApi(ctx) {
    if (!ctx || typeof ctx.coreRequest !== 'function') throw new Error('api/audit: ctx 需提供 coreRequest 函数');
    if (!ctx.util) throw new Error('api/audit: ctx 需提供 util');
    const coreRequest = ctx.coreRequest;
    const util = ctx.util;

    function list(opts) {
      // 内联拼接 query（util.js 无 query 助手）：只拼非空/非 undefined 的可枚举键。
      const parts = [];
      for (const k in (opts || {})) {
        if (!Object.prototype.hasOwnProperty.call(opts, k)) continue;
        const v = opts[k];
        if (v === undefined || v === null || v === '') continue;
        parts.push(encodeURIComponent(k) + '=' + encodeURIComponent(v));
      }
      const qs = parts.join('&');
      const path = qs ? '/api/audit?' + qs : '/api/audit';
      return coreRequest('GET', path, {}).then(function (res) {
        const parsed = util.decodeJSON(res.body);
        return Object.assign({ status: res.status, headers: res.headers }, parsed);
      });
    }

    return { list: list };
  };
});
