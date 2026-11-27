// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 通用对话框（确认 / 单行输入 / 只读展示）：v2 里唯一的模态实现。
//
// 为什么需要：`window.prompt` / `window.confirm` / `window.alert` 是**同步阻塞**的原生弹窗，
// 三个问题在 v2 上都是实打实的：
//   1. 阻断整个渲染进程（连 toast/进度都停）；
//   2. 无样式、无 CSP 友好性、无法复用既有 `modal-backdrop` 视觉（v2 里唯一"不像同一个产品"
//      的地方——实测就是这三处原生弹窗）；
//   3. 无法做输入校验（URL/名称的合法性只能靠服务端 400），也无法给出"为什么不行"的提示。
//
// 用法（都返回 Promise，取消则 resolve(null)）：
//   v2confirm.open({title, message})                  → true | null
//   v2confirm.input({title, message, value, validate}) → string | null
//   v2confirm.show({title, message, value})            → 只读展示（复制用）
// 兼容旧签名 `v2confirm(opts, cb)`：确认时调 cb()（既有调用点不动）。
'use strict';

(function () {
  if (typeof window === 'undefined') { return; }

  var box = null;
  var state = { mode: 'confirm', onOk: null, resolve: null, validate: null };

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
      '<label class="field" id="v2-confirm-field"><span id="v2-confirm-label"></span>' +
      '<input id="v2-confirm-input" autocomplete="off"></label>' +
      '<p id="v2-confirm-err" class="muted" hidden></p>' +
      '<div class="modal-actions">' +
      '<button type="button" class="btn" data-act="cancel"></button>' +
      '<button type="button" class="btn" data-act="ok"></button>' +
      '</div></div>';
    document.body.appendChild(box);

    box.addEventListener('click', function (e) {
      var act = e.target && e.target.getAttribute && e.target.getAttribute('data-act');
      if (act === 'ok') { submit(); return; }
      if (act === 'cancel' || e.target === box) { close(null); }
    });
    document.addEventListener('keydown', function (e) {
      if (box.hidden) { return; }
      if (e.key === 'Escape') { e.preventDefault(); close(null); }
      if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); }
    });
  }

  function el(id) { return box.querySelector('#' + id); }

  /** 校验失败时显示原因并保持打开（不静默丢输入）。 */
  function showErr(msg) {
    var err = el('v2-confirm-err');
    err.textContent = msg || '';
    err.hidden = !msg;
  }

  function submit() {
    var input = el('v2-confirm-input');
    if (state.mode === 'show') { close(true); return; }
    if (state.mode === 'input') {
      var v = String(input.value || '').trim();
      if (!v) { showErr('不能为空'); input.focus(); return; }
      if (typeof state.validate === 'function') {
        var bad = state.validate(v);
        if (bad) { showErr(bad); input.focus(); return; }
      }
      close(v);
      return;
    }
    close(true);
  }

  function close(value) {
    if (!box) { return; }
    box.hidden = true;
    box.setAttribute('aria-hidden', 'true');
    showErr('');
    var res = state.resolve;
    var cb = state.onOk;
    state = { mode: 'confirm', onOk: null, resolve: null, validate: null };
    if (value === true && typeof cb === 'function') { cb(); }
    if (typeof res === 'function') { res(value === true ? true : value); }
  }

  /**
   * 打开对话框。
   * @param {object} opts {title, message, okText, cancelText, danger, mode, value, label, validate}
   * @param {function()} [cb] 兼容旧签名：确认时调用
   * @returns {Promise<true|string|null>}
   */
  function open(opts, cb) {
    build();
    var o = opts || {};
    var mode = o.mode || (o.danger || !o.value ? 'confirm' : 'input');
    state.mode = mode === 'show' ? 'show' : (mode === 'input' ? 'input' : 'confirm');
    state.onOk = typeof cb === 'function' ? cb : null;
    state.validate = o.validate || null;

    el('v2-confirm-title').textContent = o.title || '确认操作？';
    el('v2-confirm-msg').textContent = o.message || '';
    showErr('');
    var field = el('v2-confirm-field');
    var input = el('v2-confirm-input');
    field.hidden = state.mode === 'confirm';
    el('v2-confirm-label').textContent = o.label || '';
    input.value = o.value || '';
    input.readOnly = state.mode === 'show';
    var ok = box.querySelector('[data-act=ok]');
    ok.textContent = o.okText || (state.mode === 'show' ? '关闭' : '确定');
    ok.className = 'btn ' + (o.danger ? 'btn-danger' : 'btn-primary');
    box.querySelector('[data-act=cancel]').textContent = o.cancelText || '取消';
    // 只读展示没有"取消"语义（只有一个关闭按钮）。
    box.querySelector('[data-act=cancel]').hidden = state.mode === 'show';
    box.hidden = false;
    box.setAttribute('aria-hidden', 'false');
    if (state.mode === 'confirm') { ok.focus(); } else { input.focus(); if (o.value) { input.select(); } }

    return new Promise(function (resolve) { state.resolve = resolve; });
  }

  /** 兼容旧签名：`v2confirm(opts, cb)`。 */
  function v2confirm(opts, cb) { return open(opts, cb); }

  v2confirm.open = open;
  v2confirm.isOpen = function () { return !!(box && !box.hidden); };
  /** 单行输入：resolve 输入值或 null（取消）。 */
  v2confirm.input = function (opts) {
    var o = opts || {};
    o.mode = 'input';
    return open(o);
  };
  /** 只读展示（密钥等一次性内容）：resolve 后由调用方自行清理。 */
  v2confirm.show = function (opts) {
    var o = opts || {};
    o.mode = 'show';
    return open(o);
  };

  window.v2confirm = v2confirm;
})();
