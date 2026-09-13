/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * mesh.js —— sclient 领域 API：跨节点（mesh）只读运维视图。
 *
 * 闭包式工厂：api/mesh.js(ctx) → { ...方法 }，ctx = { coreRequest, config, log,
 * crypto, util }（由 api/index.js 组装传入）。领域方法一律 promise。
 *
 * 端点语义对齐 server mesh_status.go：
 *   - status()  GET /api/mesh/status
 *       返回 { remote_read?, remote_write?, node?, hub_url?, signaling_enabled }：
 *       · remote_read/write: { enabled, addr, pinned }（addr 为 listener 实际监听地址）
 *       · node: { running, node_id, hub_url, webrtc, services }（running=false = 配置启用但未起）
 *         **不含任何秘密**（指纹为公开标识）。
 *
 * 该端点同时注册在主 mux（SproxySig 认证）与 localMux（隧道内层，加密即认证）
 * ⇒ direct 与隧道两种模式都可访问（与 /api/hub/* 仅主 mux 的情况不同）。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('../util.js'));
  } else {
    root.sclientApiMesh = factory(root.sclientUtil);
  }
})(typeof self !== 'undefined' ? self : this, function (utilLib) {
  'use strict';

  // ---- 领域方法工厂 ----
  return function createMeshApi(ctx) {
    if (!ctx || typeof ctx.coreRequest !== 'function') throw new Error('api/mesh: ctx 需提供 coreRequest 函数');
    if (!ctx.util) throw new Error('api/mesh: ctx 需提供 util');
    const coreRequest = ctx.coreRequest;
    const util = ctx.util;

    function status() {
      return coreRequest('GET', '/api/mesh/status', {}).then(function (res) {
        const parsed = util.decodeJSON(res.body);
        return Object.assign({ status: res.status, headers: res.headers }, parsed);
      });
    }

    return { status };
  };
});
