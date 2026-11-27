// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 模态焦点陷阱 + 焦点恢复（唯一实现）。
//
// 为什么需要（2026-11-27）：v2 的模态（`#v2-confirm`、文件操作对话框）都带
// `role="dialog" aria-modal="true"`，但**没有焦点陷阱也没有焦点恢复**：
//   - 键盘用户按 Tab 会把焦点移到模态**背后**的页面（aria-modal 只是声明，不强制）；
//   - 关闭后焦点落回 `<body>`（读屏用户失去上下文，要重新从头 Tab）。
// 这两点是 WCAG 2.4.3（焦点顺序）/ 2.1.2（键盘陷阱反向要求：模态内应可控）的基本项。
//
// 设计要点：
//   - `trap(el)` 返回 `release()`；release 时**恢复**到打开前的 activeElement（若仍在文档里）；
//   - Tab/Shift+Tab 在模态内的可聚焦元素之间循环（首/尾回绕）；
//   - 只在**可聚焦且可见**的元素间循环（`disabled`/`hidden`/`display:none` 排除）——否则
//     焦点会跑到看不见的按钮上（比不陷阱更糟）。
'use strict';

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.v2focusTrap = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  /** focusableSelector 是"可聚焦元素"的 CSS 选择器（不含 tabindex="-1"）。 */
  var FOCUSABLE = [
    'a[href]', 'button:not([disabled])', 'input:not([disabled]):not([type=hidden])',
    'select:not([disabled])', 'textarea:not([disabled])', '[tabindex]:not([tabindex="-1"])'
  ].join(',');

  /**
   * focusables 返回容器内**当前可见**的可聚焦元素（文档顺序）。
   * @param {HTMLElement} el
   * @returns {HTMLElement[]}
   */
  function focusables(el) {
    if (!el || !el.querySelectorAll) { return []; }
    var all = Array.prototype.slice.call(el.querySelectorAll(FOCUSABLE));
    return all.filter(function (n) {
      if (n.hidden) { return false; }
      if (n.getAttribute && n.getAttribute('aria-hidden') === 'true') { return false; }
      // 用 offsetParent 判断可见性（display:none 及其祖先为 none ⇒ null）；
      // position:fixed 的元素 offsetParent 也是 null ⇒ 额外用 getClientRects 兜底。
      if (n.offsetParent === null && n.getClientRects && n.getClientRects().length === 0) { return false; }
      return true;
    });
  }

  /**
   * nextIndex 计算循环焦点索引（纯函数，便于单测）。
   *
   * @param {number} cur 当前索引（-1 = 焦点不在容器内）
   * @param {number} count 可聚焦元素个数
   * @param {boolean} shift 是否 Shift+Tab（反向）
   * @returns {number} 目标索引（count<=0 时返回 -1）
   */
  function nextIndex(cur, count, shift) {
    if (count <= 0) { return -1; }
    if (cur < 0) { return shift ? count - 1 : 0; }
    var n = shift ? cur - 1 : cur + 1;
    if (n < 0) { return count - 1; }
    if (n >= count) { return 0; }
    return n;
  }

  /**
   * trap 在容器内建立焦点陷阱，返回 release()。
   *
   * @param {HTMLElement} el 模态容器（其内部可聚焦元素参与循环）
   * @param {Document} [doc] 便于测试注入
   * @returns {function()} 释放（移除监听 + 恢复焦点）
   */
  function trap(el, doc) {
    var d = doc || (typeof document !== 'undefined' ? document : null);
    if (!el || !d) { return function () {}; }
    var previous = d.activeElement;

    var onKey = function (e) {
      if (e.key !== 'Tab') { return; }
      var list = focusables(el);
      if (!list.length) {
        // 容器内没有可聚焦元素：把焦点留在容器上（避免逃到背后的页面）。
        e.preventDefault();
        return;
      }
      var cur = list.indexOf(d.activeElement);
      var idx = nextIndex(cur, list.length, e.shiftKey);
      if (idx < 0) { return; }
      e.preventDefault();
      list[idx].focus();
    };

    el.addEventListener('keydown', onKey);
    return function release() {
      el.removeEventListener('keydown', onKey);
      // 恢复到打开前的焦点（元素可能已被移除 ⇒ 检查 isConnected）。
      if (previous && previous.focus && previous.isConnected !== false) {
        try { previous.focus(); } catch (err) { /* 元素已不可聚焦：忽略 */ }
      }
    };
  }

  return { trap: trap, focusables: focusables, nextIndex: nextIndex, FOCUSABLE: FOCUSABLE };
});
