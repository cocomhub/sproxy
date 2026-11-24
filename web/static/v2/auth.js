// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 认证复用：读取旧版写入的 SproxySig 凭据（同源 sessionStorage），
// 用 sclient/sig.js 生成 Authorization。纯函数可注入 signFn，便于 Node 单测。
'use strict';

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('../shared/creds.js'));
  } else {
    root.v2auth = factory(root.sharedCreds);
  }
})(typeof self !== 'undefined' ? self : this, function (creds) {
  if (!creds) { throw new Error('auth.js 需要 shared/creds.js（请在之前加载）'); }
  var KEYS = creds.KEYS;
  var loadCreds = creds.loadCreds;

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
