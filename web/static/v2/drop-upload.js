// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 拖拽上传：把文件拖到页面上即上传到**当前目录/当前卷**。
//
// 旧页有该能力（`app.js` 的 initDragAndDrop），v2 此前缺失。富交互（提示层/计数器）
// 走 DOM，判定「是否接受这次拖拽」是纯函数，由 node --test 覆盖。
'use strict';

(function (root, factory) {
  var api = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  } else {
    root.v2dropUpload = api;
  }
})(typeof self !== 'undefined' ? self : this, function () {
  /**
   * 判断一次拖拽是否**携带文件**（而非拖选文本/链接）。
   * DataTransfer.types 在 dragenter 阶段就可用（files 列表要到 drop 才有）。
   * @param {string[]|null|undefined} types
   * @returns {boolean}
   */
  function acceptsDrag(types) {
    if (!types || typeof types.length !== 'number') { return false; }
    for (var i = 0; i < types.length; i += 1) {
      if (types[i] === 'Files') { return true; }
    }
    return false;
  }

  /**
   * 拖拽计数器归零判定：dragenter/dragleave 会因**子元素**反复触发，
   * 用计数抵消；计数不得为负（否则一次漏掉 leave 就永久显示提示层）。
   * @param {number} n
   * @param {number} delta
   * @returns {number}
   */
  function bump(n, delta) {
    var v = (Number(n) || 0) + (delta < 0 ? -1 : 1);
    return v < 0 ? 0 : v;
  }

  /** 提示文案（带目标位置，避免用户不知道传到哪）。 */
  function hintText(subdir, volume) {
    var where = subdir ? ('/' + subdir) : '/';
    var vol = volume ? (' · 卷 ' + volume) : '';
    return '上传到 ' + where + vol;
  }

  return { acceptsDrag: acceptsDrag, bump: bump, hintText: hintText };
});
