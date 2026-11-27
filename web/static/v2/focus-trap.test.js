// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { nextIndex, trap, focusables } = require('./focus-trap.js');

test('nextIndex：循环前进/后退与边界回绕', () => {
  // 3 个元素：0→1→2→0（前进），0→2→1→0（后退）
  assert.strictEqual(nextIndex(-1, 3, false), 0, '焦点不在容器内 ⇒ 前进到首个');
  assert.strictEqual(nextIndex(-1, 3, true), 2, '焦点不在容器内 ⇒ 后退到末尾');
  assert.strictEqual(nextIndex(0, 3, false), 1);
  assert.strictEqual(nextIndex(2, 3, false), 0, '末尾前进回绕到首个');
  assert.strictEqual(nextIndex(0, 3, true), 2, '首个后退回绕到末尾');
  assert.strictEqual(nextIndex(1, 1, false), 0, '单元素恒为自身');
  assert.strictEqual(nextIndex(0, 0, false), -1, '空容器返回 -1');
});

// --- 最小 DOM 替身（纯 node 环境，不引入 jsdom） ---
function fakeEl(opts) {
  const listeners = {};
  const el = {
    hidden: !!opts.hidden,
    offsetParent: opts.hidden ? null : {},
    children: opts.children || [],
    getClientRects: () => (opts.hidden ? [] : [{}]),
    getAttribute: (k) => (opts.ariaHidden && k === 'aria-hidden' ? 'true' : null),
    querySelectorAll: () => el.children,
    addEventListener: (t, fn) => { (listeners[t] = listeners[t] || []).push(fn); },
    removeEventListener: (t, fn) => { listeners[t] = (listeners[t] || []).filter((f) => f !== fn); },
    dispatch: (t, ev) => (listeners[t] || []).forEach((f) => f(ev)),
    focus: () => { doc.activeElement = el; },
    isConnected: true,
  };
  return el;
}

let doc;
function makeDoc() {
  doc = { activeElement: null };
  return doc;
}

test('focusables：排除 hidden / aria-hidden / 不可见元素', () => {
  const visible = fakeEl({});
  const hidden = fakeEl({ hidden: true });
  const ariaHidden = fakeEl({ ariaHidden: true });
  const zeroRect = fakeEl({ hidden: true }); // offsetParent null + 无 rect
  const box = fakeEl({});
  box.children = [visible, hidden, ariaHidden, zeroRect];
  const got = focusables(box);
  assert.deepStrictEqual(got, [visible], '只保留真正可见可聚焦的元素');
  assert.deepStrictEqual(focusables(null), []);
});

test('trap：Tab 在容器内循环，Shift+Tab 反向，release 恢复原焦点', () => {
  makeDoc();
  const a = fakeEl({});
  const b = fakeEl({});
  const c = fakeEl({});
  const box = fakeEl({});
  box.children = [a, b, c];
  const opener = fakeEl({});
  doc.activeElement = opener;

  let prevented = 0;
  const ev = (shift) => ({ key: 'Tab', shiftKey: shift, preventDefault: () => { prevented += 1; } });

  const release = trap(box, doc);

  // 焦点在容器外 ⇒ Tab 落到首个（并阻止默认行为，防止逃到背后页面）。
  doc.activeElement = opener;
  box.dispatch('keydown', ev(false));
  assert.strictEqual(doc.activeElement, a);
  assert.strictEqual(prevented, 1);

  // a → b → c → 回绕 a
  box.dispatch('keydown', ev(false));
  assert.strictEqual(doc.activeElement, b);
  box.dispatch('keydown', ev(false));
  assert.strictEqual(doc.activeElement, c);
  box.dispatch('keydown', ev(false));
  assert.strictEqual(doc.activeElement, a, 'Tab 到末尾应回绕到首个');

  // Shift+Tab：a → 回绕 c
  box.dispatch('keydown', ev(true));
  assert.strictEqual(doc.activeElement, c, 'Shift+Tab 在首个应回绕到末尾');

  // 非 Tab 键不干预
  const before = prevented;
  box.dispatch('keydown', { key: 'a', preventDefault: () => { prevented += 1; } });
  assert.strictEqual(prevented, before);

  // release：恢复打开前的焦点，并解绑（再按 Tab 不再被劫持）。
  release();
  assert.strictEqual(doc.activeElement, opener, 'release 应恢复到打开前的焦点');
  const cnt = prevented;
  box.dispatch('keydown', ev(false));
  assert.strictEqual(prevented, cnt, 'release 后不应再劫持 Tab');
});

test('trap：容器内无可聚焦元素时把焦点留在容器（不逃到背后页面）', () => {
  makeDoc();
  const box = fakeEl({});
  box.children = [];
  let prevented = 0;
  trap(box, doc);
  box.dispatch('keydown', { key: 'Tab', shiftKey: false, preventDefault: () => { prevented += 1; } });
  assert.strictEqual(prevented, 1, '必须阻止默认行为');
});
