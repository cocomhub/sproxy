// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 有限并发执行池（上传队列用）。
//
// 为什么单独成模块：`files-actions.js` 是 DOM 绑定的 IIFE，池的语义（并发上限、
// 失败不阻断、按完成补位）纯逻辑且易错，需要 node --test 直接覆盖。
'use strict';

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.v2uploadPool = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  /**
   * 以最多 `limit` 个并发执行 `task(item)`，全部结束后 resolve。
   *
   * 契约：
   * - `task` 的 reject 被**吞掉**（失败由调用方在 task 内部收敛，不阻断队列）；
   * - 启动顺序确定（队首先启动），完成顺序不定；
   * - 返回 promise **恒 resolve**（不因单个失败 reject）——队列语义是「跑完」而非「全成功」；
   * - `items` 为空 ⇒ 立即 resolve；`limit <= 0` ⇒ 视为 1（绝不无限并发）。
   *
   * @param {Array} items 待处理项
   * @param {(item:any)=>Promise<any>} task
   * @param {number} limit 并发上限
   * @returns {Promise<void>}
   */
  function runPool(items, task, limit) {
    var list = Array.prototype.slice.call(items || []);
    if (!list.length) { return Promise.resolve(); }
    var cap = Math.max(1, Math.min(Number(limit) || 1, list.length));
    var next = 0;
    function lane() {
      if (next >= list.length) { return Promise.resolve(); }
      var item = list[next];
      next += 1;
      return Promise.resolve()
        .then(function () { return task(item); })
        .then(function () { return null; }, function () { return null; })
        .then(lane);
    }
    var lanes = [];
    for (var i = 0; i < cap; i += 1) { lanes.push(lane()); }
    return Promise.all(lanes).then(function () { return undefined; });
  }

  return { runPool: runPool };
});
