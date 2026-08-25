/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * hub.js —— sclient 领域 API：Hub 中继节点 / 统计。
 *
 * 闭包式工厂：api/hub.js(ctx) → { ...方法 }，ctx = { coreRequest, config, log,
 * crypto, util }（由 api/index.js 组装传入）。领域方法一律 promise。
 *
 * 端点语义对齐 server hub_handler.go：
 *   - nodes()         GET  /api/hub/nodes（按调用方 mesh 过滤的节点列表）
 *   - stats()         GET  /api/hub/stats（{nodes_connected}）
 *   - remove(id)      DELETE /api/hub/nodes/{id}（移除节点）
 *
 * 说明：/api/hub/nodes 与 /api/hub/stats 仅经主 mux（Bearer auth）暴露，未在
 * localMux（隧道内层）注册——direct SproxySig 即可访问；隧道模式下经 /tunnel
 * 转发会命中 localMux 无此路由 → 404。故本领域方法对隧道 mode 语义受限：
 * transport 隧道层仅 /tunnel 有效，此 endpoint 在隧道 mode 下将 E_INTERNAL
 *（coreRequest guarded）。属传输层既有约束，由调用方保证 direct mode 访问。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('../util.js'));
  } else {
    root.sclientApiHub = factory(root.sclientUtil);
  }
})(typeof self !== 'undefined' ? self : this, function (utilLib) {
  'use strict';

  // ---- 领域方法工厂 ----
  return function createHubApi(ctx) {
    if (!ctx || typeof ctx.coreRequest !== 'function') throw new Error('api/hub: ctx 需提供 coreRequest 函数');
    if (!ctx.util) throw new Error('api/hub: ctx 需提供 util');
    const coreRequest = ctx.coreRequest;
    const util = ctx.util;
    const log = ctx.log;

    function nodes() {
      return coreRequest('GET', '/api/hub/nodes', {}).then(function (res) {
        const parsed = util.decodeJSON(res.body);
        return Object.assign({ status: res.status, headers: res.headers }, Array.isArray(parsed) ? { nodes: parsed } : parsed);
      });
    }
    function stats() {
      return coreRequest('GET', '/api/hub/stats', {}).then(function (res) {
        const parsed = util.decodeJSON(res.body);
        return Object.assign({ status: res.status, headers: res.headers }, parsed);
      });
    }
    function remove(nodeId) {
      const path = '/api/hub/nodes/' + encodeURIComponent(nodeId);
      return coreRequest('DELETE', path, {}).then(function (res) {
        const parsed = util.decodeJSON(res.body);
        return Object.assign({ status: res.status, headers: res.headers }, parsed);
      });
    }

    return { nodes, stats, remove };
  };
});
