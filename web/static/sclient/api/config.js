/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * config.js —— sclient 领域 API：运行时配置读写。
 *
 * 闭包式工厂：api/config.js(ctx) → { ...方法 }，ctx = { coreRequest, config, log,
 * crypto, util }（由 api/index.js 组装传入）。领域方法一律 promise。
 *
 * 端点语义对齐 server config_api.go / client hub.go：
 *   - get()            GET  /api/config（脱敏运行时配置，含 web_tunnel）
 *   - update(patch)    PUT  /api/config（可改 log_level / log_format /
 *     rate_limit_requests / rate_limit_window / max_storage_bytes / web_tunnel）
 *   - updateStorage(maxStorageBytes)  PUT  /api/storage/config {max_storage_bytes}
 *     注：服务端主 mux 另有 PUT /api/config 的 max_storage_bytes 支持；
 *     /api/storage/config 为 client SDK 既有路由（本领域一并封装）。
 *
 * get() 返回 {log_level, log_format, access_keys_set, tunnel_key_set, ...web_tunnel}。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('../util.js'));
  } else {
    root.sclientApiConfig = factory(root.sclientUtil);
  }
})(typeof self !== 'undefined' ? self : this, function (utilLib) {
  'use strict';

  const TE = new TextEncoder();
  function encodeJSON(obj) { return TE.encode(JSON.stringify(obj)); }

  // ---- 领域方法工厂 ----
  return function createConfigApi(ctx) {
    if (!ctx || typeof ctx.coreRequest !== 'function') throw new Error('api/config: ctx 需提供 coreRequest 函数');
    if (!ctx.util) throw new Error('api/config: ctx 需提供 util');
    const coreRequest = ctx.coreRequest;
    const util = ctx.util;
    const log = ctx.log;

    async function jsonRequest(method, path, data) {
      const headers = {};
      let bodyBytes = null;
      if (data !== undefined && data !== null) {
        bodyBytes = encodeJSON(data);
        headers['Content-Type'] = 'application/json';
      }
      const res = await coreRequest(method, path, { headers: headers, bodyBytes: bodyBytes });
      const parsed = util.decodeJSON(res.body);
      return Object.assign({ status: res.status, headers: res.headers }, parsed);
    }

    // GET /api/config：脱敏运行时配置。调用方可据此配置 transport.configure。
    function get() {
      return jsonRequest('GET', '/api/config', undefined);
    }

    // PUT /api/config：部分更新（仅请求体中出现的字段生效）。
    // patch 白名单对齐 updateConfigRequest（log_level/log_format/rate_limit_requests/
    // rate_limit_window/max_storage_bytes/web_tunnel），其余字段忽略。
    function update(patch) {
      const allowed = ['log_level', 'log_format', 'rate_limit_requests', 'rate_limit_window', 'max_storage_bytes', 'web_tunnel'];
      const body = {};
      if (patch && typeof patch === 'object') {
        for (const k of allowed) {
          if (patch[k] !== undefined && patch[k] !== null) body[k] = patch[k];
        }
      }
      return jsonRequest('PUT', '/api/config', body);
    }

    // PUT /api/storage/config：更新存储上限（客户端 SDK 既有路由）。
    function updateStorage(maxStorageBytes) {
      return jsonRequest('PUT', '/api/storage/config', { max_storage_bytes: maxStorageBytes });
    }

    return { get, update, updateStorage };
  };
});
