/* SPDX-License-Identifier: Apache-2.0 */
/*
 * i18n.test.js —— web/static/i18n.js 的单元测试（node --test）。
 *
 * 覆盖 docs/designs/2026-09-24-webui-i18n.md「测试与变异点」：
 *   1. zh/en 字典键集完全一致（深比较）——变异：删 en 任一 key → 红
 *   2. t() 命中 / 回落 zh / 返回 key 原文 三态——变异：删回落逻辑 → 红
 *   3. fmt() 插值、缺变量、多变量——变异：改插值实现 → 红
 *   4. lang() 优先级（localStorage > navigator.language > 默认 zh）——变异：颠倒优先级 → 红
 *   5. setLang 持久化 + html lang 同步 + i18n:changed 派发
 *   6. applyStaticI18n 批量替换 data-i18n / data-i18n-placeholder
 *
 * node 环境无真实 localStorage/window/document：用例用最小 stub 模拟，
 * afterEach 恢复全局，避免相互污染。
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert');

const I18N = require('./i18n.js');

const ORIG = {
  localStorage: global.localStorage,
  window: global.window,
  navigator: global.navigator,
  document: global.document,
};

function makeStorage(init) {
  const m = new Map(Object.entries(init || {}));
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: (k) => { m.delete(k); },
  };
}

function setLocalStorage(store) {
  Object.defineProperty(global, 'localStorage', { value: store, configurable: true });
}
function setWindow(w) {
  Object.defineProperty(global, 'window', { value: w, configurable: true });
}
function setNavigator(n) {
  Object.defineProperty(global, 'navigator', { value: n, configurable: true });
}

test.afterEach(() => {
  setLocalStorage(ORIG.localStorage);
  setWindow(ORIG.window);
  setNavigator(ORIG.navigator);
  global.document = ORIG.document;
});

test('zh/en 字典键集完全一致（变异：删 en 任一 key → 红）', () => {
  const zhKeys = Object.keys(I18N.dicts.zh).sort();
  const enKeys = Object.keys(I18N.dicts.en).sort();
  assert.deepStrictEqual(
    enKeys,
    zhKeys,
    'en 缺失 key: ' + zhKeys.filter((k) => !enKeys.includes(k)).join(', '),
  );
});

test('t() 命中当前语言（localStorage=en → 英文）', () => {
  setLocalStorage(makeStorage({ sproxy_lang: 'en' }));
  assert.strictEqual(I18N.t('upload_files'), 'Upload Files');
});

test('t() 命中 zh（默认语言）', () => {
  setLocalStorage(makeStorage({}));
  setNavigator({ language: 'zh-CN' });
  assert.strictEqual(I18N.t('upload_files'), '上传文件');
});

test('t() 当前语言缺 key 回落 zh（变异：删回落逻辑 → 红）', () => {
  setLocalStorage(makeStorage({ sproxy_lang: 'en' }));
  const key = 'upload_files';
  const savedEn = I18N.dicts.en[key];
  delete I18N.dicts.en[key];
  try {
    assert.strictEqual(I18N.t(key), '上传文件', 'en 缺 key 应回落 zh 值');
  } finally {
    I18N.dicts.en[key] = savedEn;
  }
});

test('t() 两语言均缺 key 返回 key 原文（可见可修）', () => {
  setLocalStorage(makeStorage({ sproxy_lang: 'en' }));
  assert.strictEqual(I18N.t('no_such_key_xyz'), 'no_such_key_xyz');
});

test('t() 支持 {name} 插值', () => {
  setLocalStorage(makeStorage({}));
  setNavigator({ language: 'zh-CN' });
  assert.strictEqual(I18N.t('confirm_delete', { name: 'a.txt' }), '确认删除 "a.txt"?');
});

test('fmt() 插值 / 缺变量保留原文 / 多变量（变异：改插值实现 → 红）', () => {
  assert.strictEqual(I18N.fmt('删除 {name}?', { name: 'a.txt' }), '删除 a.txt?');
  assert.strictEqual(I18N.fmt('删除 {name}?', {}), '删除 {name}?');
  assert.strictEqual(I18N.fmt('{a} + {b}', { a: 1, b: 2 }), '1 + 2');
  assert.strictEqual(I18N.fmt('纯文本', null), '纯文本');
});

test('lang()：localStorage 优先（变异：颠倒优先级 → 红）', () => {
  setLocalStorage(makeStorage({ sproxy_lang: 'en' }));
  setWindow({});
  setNavigator({ language: 'zh-CN' });
  assert.strictEqual(I18N.lang(), 'en', 'localStorage 应优先于 navigator.language');
});

test('lang()：localStorage 无值 → navigator.language（zh*→zh，其余→en）', () => {
  setLocalStorage(makeStorage({}));
  setWindow({});
  setNavigator({ language: 'fr-FR' });
  assert.strictEqual(I18N.lang(), 'en');
  setNavigator({ language: 'zh-TW' });
  assert.strictEqual(I18N.lang(), 'zh');
});

test('lang()：无 localStorage/window → 默认 zh（node 测试环境基线）', () => {
  setLocalStorage(undefined);
  setWindow(undefined);
  setNavigator(undefined);
  assert.strictEqual(I18N.lang(), 'zh');
});

test('setLang 持久化 + html lang 同步 + 派发 i18n:changed', () => {
  const store = makeStorage({});
  setLocalStorage(store);
  let htmlLang = '';
  let dispatched = 0;
  setWindow({ dispatchEvent: () => { dispatched++; } });
  global.document = {
    documentElement: { set lang(v) { htmlLang = v; } },
    querySelectorAll: () => [],
  };
  const ret = I18N.setLang('en');
  assert.strictEqual(ret, 'en');
  assert.strictEqual(store.getItem('sproxy_lang'), 'en');
  assert.strictEqual(htmlLang, 'en');
  assert.strictEqual(dispatched, 1);
  // 非法值归一 zh
  const ret2 = I18N.setLang('fr');
  assert.strictEqual(ret2, 'zh');
  assert.strictEqual(store.getItem('sproxy_lang'), 'zh');
});

test('applyStaticI18n 批量替换 data-i18n 文本与 placeholder（变异：摘 data-i18n → 不翻译）', () => {
  const textNode = {
    getAttribute: (a) => (a === 'data-i18n' ? 'upload_files' : null),
    _text: '',
    get textContent() { return this._text; },
    set textContent(v) { this._text = v; },
  };
  const phNode = {
    getAttribute: (a) => (a === 'data-i18n-placeholder' ? 'search_placeholder' : null),
    setAttribute(k, v) { this._ph = v; },
  };
  let htmlLang = '';
  global.document = {
    documentElement: { set lang(v) { htmlLang = v; } },
    querySelectorAll: (sel) => (sel === '[data-i18n]' ? [textNode] : [phNode]),
  };
  setWindow({ dispatchEvent: () => {} });
  setLocalStorage(makeStorage({ sproxy_lang: 'en' }));
  I18N.setLang('en');
  I18N.applyStaticI18n();
  assert.strictEqual(textNode.textContent, 'Upload Files');
  assert.strictEqual(phNode._ph, 'Search files…');
  assert.strictEqual(htmlLang, 'en');
});
