// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 通用确认框（无输入）：破坏性操作执行前的唯一确认实现。
//
// 为什么单独成文件：`files-actions.js` 里那套对话框**带输入框**（新建目录/重命名），
// 与「纯确认」是两种能力。任务动作（覆盖写入）与文件删除都只需要纯确认，
// 且分别位于不同脚本 —— 若不抽出来，就会出现第二、第三份确认实现（本仓已多次踩到
// 「同一语义两份实现后语义漂移」）。
//
// 用法：`window.v2confirm({title, message, okText, danger}, function () { ... })`。
// 取消 / Esc / 点遮罩 都**不**调用回调（fail-safe：破坏性动作默认不执行）。
'use strict';

(function () {
  if (typeof window === 'undefined') { return; }

  var box = null;
  var onOk = null;

  function build() {
    if (box) { return; }
    box = document.createElement('div');
    box.id = 'v2-confirm';
    box.className = 'modal-backdrop';
    box.hidden = true;
    box.setAttribute('aria-hidden', 'true');
    box.innerHTML =
      '<div class="modal" role="dialog" aria-modal="true" aria-labelledby="v2-confirm-title">' +
      '<h3 id="v2-confirm-title"></h3>' +
      '<p id="v2-confirm-msg"></p>' +
      '<div class="modal-actions">' +
      '<button type="button" class="btn" data-act="cancel"></button>' +
      '<button type="button" class="btn" data-act="ok"></button>' +
      '</div></div>';
    document.body.appendChild(box);

    box.addEventListener('click', function (e) {
      var act = e.target && e.target.getAttribute && e.target.getAttribute('data-act');
      if (act === 'ok') { close(true); return; }
      if (act === 'cancel' || e.target === box) { close(false); }
    });
    document.addEventListener('keydown', function (e) {
      if (box.hidden) { return; }
      if (e.key === 'Escape') { e.preventDefault(); close(false); }
      if (e.key === 'Enter') { e.preventDefault(); close(true); }
    });
  }

  function close(ok) {
    if (!box) { return; }
    box.hidden = true;
    box.setAttribute('aria-hidden', 'true');
    var cb = onOk;
    onOk = null;
    if (ok && typeof cb === 'function') { cb(); }
  }

  /**
   * 打开确认框。
   * @param {{title?:string,message?:string,okText?:string,cancelText?:string,danger?:boolean}} opts
   * @param {function()} cb 确认后执行（取消不调用）
   */
  window.v2confirm = function (opts, cb) {
    build();
    var o = opts || {};
    onOk = cb;
    box.querySelector('#v2-confirm-title').textContent = o.title || '确认操作？';
    box.querySelector('#v2-confirm-msg').textContent = o.message || '';
    var ok = box.querySelector('[data-act=ok]');
    ok.textContent = o.okText || '确定';
    ok.className = 'btn ' + (o.danger ? 'btn-danger' : 'btn-primary');
    box.querySelector('[data-act=cancel]').textContent = o.cancelText || '取消';
    box.hidden = false;
    box.setAttribute('aria-hidden', 'false');
    ok.focus();
  };

  /** 是否正打开（e2e / 其它脚本判定用）。 */
  window.v2confirm.isOpen = function () { return !!(box && !box.hidden); };
})();
