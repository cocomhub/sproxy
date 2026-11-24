// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 认证复用：读取旧版写入的 SproxySig 凭据（同源 sessionStorage），
// 用 sclient/sig.js 生成 Authorization。纯函数可注入 signFn，便于 Node 单测。
'use strict';

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.v2auth = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  var KEYS = {
    ak: 'sproxy_access_key',
    sk: 'sproxy_access_key_secret',
    id: 'sproxy_access_key_id'
  };

  /** 读取凭据；任一缺失返回 null（匿名请求）。 */
  function loadCreds(storage) {
    if (!storage) { return null; }
    var ak = '', sk = '', id = '';
    try {
      ak = storage.getItem(KEYS.ak) || '';
      sk = storage.getItem(KEYS.sk) || '';
      id = storage.getItem(KEYS.id) || '';
    } catch (e) {
      return null;
    }
    if (!ak || !sk) { return null; }
    return { ak: ak, sk: sk, entryID: id };
  }

  /** 生成签名头；无凭据返回空对象（由服务端 401 或匿名兜底决定）。 */
  function authHeaders(method, pathWithQuery, body, creds, signFn) {
    if (!creds) { return Promise.resolve({}); }
    if (typeof signFn !== 'function') { return Promise.reject(new TypeError('authHeaders 需要 signFn')); }
    return signFn(method, pathWithQuery, body, {
      ak: creds.ak,
      secret: creds.sk,
      entryID: creds.entryID || undefined
    }).then(function (auth) { return { Authorization: auth }; });
  }

  return { KEYS: KEYS, loadCreds: loadCreds, authHeaders: authHeaders };
});
