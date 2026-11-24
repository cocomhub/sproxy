// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 API 客户端（P1c）：统一签名 + fetch + JSON 错误处理，供视图/操作/检查器复用。
// 依赖全局：v2auth、sclientSig（浏览器）。纯逻辑很少，主要保证只有一处请求签名。
'use strict';

(function () {
  if (typeof window === 'undefined') { return; }
  var auth = window.v2auth;
  var sig = window.sclientSig;
  if (!auth || !sig) { return; }

  function sign(method, path, bodyBytes) {
    var creds = auth.loadCreds(window.sessionStorage);
    return auth.authHeaders(method, path, bodyBytes || null, creds, sig.signHeader);
  }

  /**
   * 发起已签名请求。
   * @param {string} method
   * @param {string} path 含 query 的相对路径
   * @param {{body?:Uint8Array, contentType?:string, headers?:object}} [opts]
   * @returns {Promise<Response>}
   */
  function request(method, path, opts) {
    var o = opts || {};
    return sign(method, path, o.body).then(function (h) {
      var headers = Object.assign({}, o.headers || {}, h);
      if (o.contentType) { headers['Content-Type'] = o.contentType; }
      return fetch(path, {
        method: method,
        headers: headers,
        body: o.body || undefined,
        credentials: 'same-origin'
      });
    });
  }

  function errorText(resp) {
    return resp.text().then(function (txt) {
      try { var j = JSON.parse(txt); return j.message || j.error || ('HTTP ' + resp.status); }
      catch (e) { return txt || ('HTTP ' + resp.status); }
    });
  }

  /** 断言 2xx，否则抛出带服务端文案的错误。 */
  function ensureOk(resp) {
    if (resp.ok) { return resp; }
    return errorText(resp).then(function (msg) { throw new Error(msg); });
  }

  /** 便捷 JSON：ensureOk + 解析（空体返回 {}）。 */
  function json(method, path, opts) {
    return request(method, path, opts).then(ensureOk).then(function (resp) {
      return resp.text().then(function (txt) {
        if (!txt) { return {}; }
        try { return JSON.parse(txt); } catch (e) { throw new Error('响应不是合法 JSON'); }
      });
    });
  }

  window.v2api = { request: request, ensureOk: ensureOk, errorText: errorText, json: json };
})();
