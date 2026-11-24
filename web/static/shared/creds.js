// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// shared/creds.js —— 旧版 Web UI 凭据存储契约（sessionStorage 键名）+ 读取。
//
// 键名由 `web/static/app.js` 写入（登录/保存 AccessKey 时）；v2 复用同一契约，
// 不复制密钥、不写新存储。旧页迁移时这些键名应保持兼容（见
// docs/designs/2026-11-24-webui-modernization.md「前端复用与维护」）。
'use strict';

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.sharedCreds = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  var KEYS = {
    ak: 'sproxy_access_key',
    sk: 'sproxy_access_key_secret',
    id: 'sproxy_access_key_id'
  };

  /**
   * 从给定 Storage 读取凭据；缺少 AK/SK 或读取异常返回 null（匿名）。
   * @param {Storage|null} storage
   * @returns {{ak:string, sk:string, entryID:string}|null}
   */
  function loadCreds(storage) {
    if (!storage) { return null; }
    var ak = '', sk = '', id = '';
    try {
      ak = storage.getItem(KEYS.ak) || '';
      sk = storage.getItem(KEYS.sk) || '';
      id = storage.getItem(KEYS.id) || '';
    } catch (e) {
      return null; // 隐私模式/权限拒绝
    }
    if (!ak || !sk) { return null; }
    return { ak: ak, sk: sk, entryID: id };
  }

  return { KEYS: KEYS, loadCreds: loadCreds };
});
