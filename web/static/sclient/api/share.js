/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * share.js —— sclient 领域 API：分享链接创建 / 列表 / 撤销。
 *
 * 闭包式工厂：api/share.js(ctx) → { ...方法 }，ctx = { coreRequest, config, log,
 * crypto, util }（由 api/index.js 组装传入）。领域方法一律 promise。
 *
 * 端点语义对齐 server 分享 handlers（share.go）：
 *   - create({filename, ttl, max_downloads, one_time}) → POST /api/share
 *   - list()              → GET  /api/shares
 *   - revoke(token)       → DELETE /api/shares/{token}
 *
 * 响应：create/list/revoke 返回服务端 JSON 对象（{token, ...}/{shares:[...]}/
 * {success,message}）。调用方可取 token 拼分享 URL（location.origin + '/s/' + token）。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('../util.js'));
  } else {
    root.sclientApiShare = factory(root.sclientUtil);
  }
})(typeof self !== 'undefined' ? self : this, function (utilLib) {
  'use strict';

  const TE = new TextEncoder();
  function encodeJSON(obj) { return TE.encode(JSON.stringify(obj)); }

  // ---- 领域方法工厂 ----
  return function createShareApi(ctx) {
    if (!ctx || typeof ctx.coreRequest !== 'function') throw new Error('api/share: ctx 需提供 coreRequest 函数');
    if (!ctx.util) throw new Error('api/share: ctx 需提供 util');
    const coreRequest = ctx.coreRequest;
    const util = ctx.util;

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

    // 创建分享链接。默认 24h、不限制下载次数、非一次性（对齐 UI shareFile 默认）。
    function create(opts) {
      const o = opts || {};
      const data = {
        filename: o.filename,
        ttl: o.ttl || '24h',
        max_downloads: o.max_downloads !== undefined ? o.max_downloads : 0,
        one_time: o.one_time === true,
      };
      return jsonRequest('POST', '/api/share', data);
    }
    function list() {
      return jsonRequest('GET', '/api/shares', undefined);
    }
    function revoke(token) {
      return jsonRequest('DELETE', '/api/shares/' + encodeURIComponent(token), undefined);
    }

    return { create, list, revoke };
  };
});
