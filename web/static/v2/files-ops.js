// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 写操作（P1b）纯逻辑：名称校验与请求参数构造，便于 Node 单测。
// 服务端仍是最终裁决（ValidateFilePath）；这里只做前置拦截，减少无谓请求。
'use strict';

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.v2ops = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  var BAD_CHARS = /[\\/:*?"<>|\u0000-\u001f]/;

  /** 校验单段名称（目录/重命名新名）：空、含分隔符/保留字符、. / .. 均拒绝。 */
  function validateName(name) {
    var s = String(name == null ? '' : name).trim();
    if (!s) { return { ok: false, reason: '名称不能为空' }; }
    if (s === '.' || s === '..') { return { ok: false, reason: '名称不能是 . 或 ..' }; }
    if (BAD_CHARS.test(s)) { return { ok: false, reason: '名称不能包含 \\ / : * ? " < > | 或控制字符' }; }
    if (s.length > 255) { return { ok: false, reason: '名称过长（>255）' }; }
    return { ok: true, name: s };
  }

  /** 简单上传请求头（与服务端 /upload 契约一致）。 */
  function uploadHeaders(relPath, checksum, mtimeMs) {
    return {
      'X-File-Checksum': checksum,
      'X-File-Path': relPath,
      'X-File-MTime': String((Number(mtimeMs) || Date.now()) * 1e6)
    };
  }

  /** 删除/重命名共用：目录无 checksum 时传空串（服务端对目录不校验内容）。 */
  function checksumHeader(checksum) {
    return { 'X-File-Checksum': checksum || '' };
  }

  /** 选择集合 → 待删除路径列表（保持稳定顺序，去重）。 */
  function selectedPaths(names) {
    var seen = Object.create(null);
    var out = [];
    (names || []).forEach(function (n) {
      var s = String(n || '');
      if (!s || seen[s]) { return; }
      seen[s] = true;
      out.push(s);
    });
    return out;
  }

  return {
    validateName: validateName,
    uploadHeaders: uploadHeaders,
    checksumHeader: checksumHeader,
    selectedPaths: selectedPaths
  };
});
