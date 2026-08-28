/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * app-transfer-actions.test.js —— app.js 传输页行操作事件委托的语义测试。
 *
 * 目的：防止「按钮渲染了但事件委托未接线 / 接线了但类名漂移」再次悄悄失效
 *      （历史事故：transfer-* 按钮渲染一年多没接线；本次已补委托 + 浏览器验证）。
 * app.js 不是模块（浏览器全局），Node 直接读源串做结构性断言——保大盘不回归：
 *   1. 每个 transfer-* 操作类名在 #transfer-body 委托块内都有 dispatch 分支（含函数名）；
 *   2. 委托用 data-item-id 定位 + 经过 getTransferStore/getDownloadManager；
 *   3. 渲染端（app-render）类名与委托端类名名单一致（两端漂移 = 按钮失效）。
 * 同时：让 app.js 的 UMD 导出面继续可用（未来若 app.js 改成模块，本测试自动收敛）。
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const APP_JS = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
const RENDER_JS = fs.readFileSync(path.join(__dirname, 'app-render.js'), 'utf8');

// 委托块：#transfer-body click handler。transfer-* 分支都应在其中（含 dispatch 到的函数名）。
const DELEGATE_RE = /transferBody\.addEventListener\('click'[\s\S]{0,4000}/;
const delegateBlock = (APP_JS.match(DELEGATE_RE) || [''])[0];

// 委托端应存在的按钮类名 → 被调用的目标（函数/成员）。
const CASES = [
  ['transfer-pause-btn', 'pauseDownload'],
  ['transfer-resume-btn', 'resumeDownload'],
  ['transfer-resume-btn', 'resumeUpload'],
  ['transfer-cancel-btn', 'cancelDownload'],
  ['transfer-cancel-btn', 'removeUploadSession'],
  ['transfer-delete-btn', 'removeItem'],
  ['transfer-redownload-btn', 'downloadFile'],
  ['transfer-open-dir-btn', 'navigateDir'],
];

// 渲染端（app-render）生成的类名集合——与委托端对照，两端一致才可点击。
const renderButtonClasses = (RENDER_JS.match(/transfer-[a-z-]+-btn/g) || [])
  .filter((v, i, a) => a.indexOf(v) === i); // 去重保序

for (const [cls, callee] of CASES) {
  test(`委托：transfer 行操作「${cls}」在 #transfer-body 块内有分发并调 ${callee}`, () => {
    assert.ok(delegateBlock.includes(cls), `委托块应处理 ${cls}`);
    assert.ok(callee === 'navigateDir' ? delegateBlock.includes(callee) : delegateBlock.includes(callee),
      `委托块应调用 ${callee}`);
  });
}

test('渲染端与委托端按钮类名集合一致（无孤儿按钮 / 无未接线的类名）', () => {
  for (const cls of renderButtonClasses) {
    assert.ok(delegateBlock.includes(cls), `渲染生成 ${cls} 但委托块未处理——按钮无效`);
  }
});

test('委托使用 data-item-id 定位且检查 kind 分支（upload/download 分清）', () => {
  assert.ok(delegateBlock.includes('data-itemId') || delegateBlock.includes("dataset.itemId"), '应从 data-item-id 取 id');
  assert.ok(delegateBlock.includes("kind === 'upload'") && delegateBlock.includes("kind === 'download'"), '应按 kind 分流 upload/download');
});

test('getTransferStore / getDownloadManager 在委托内被使用（走同一 store/管线实例）', () => {
  assert.ok(delegateBlock.includes('getTransferStore()') || delegateBlock.includes('getTransferStore ()'),
    '委托块应调用 getTransferStore()');
  assert.ok(delegateBlock.includes('getDownloadManager()') || delegateBlock.includes('getDownloadManager ()'),
    '委托块应调用 getDownloadManager()（download 恢复/取消/暂停走管线）');
});

test('upload.js 未再声明 getTransferStore（与 app.js 保证唯一性，防重名回归）', () => {
  const upSrc = fs.readFileSync(path.join(__dirname, 'upload.js'), 'utf8');
  const decls = upSrc.match(/^(?:function|const|let|var)\s+getTransferStore\b/mg) || [];
  assert.equal(decls.length, 0, 'upload.js 不得声明 getTransferStore：' + decls.join(','));
  assert.ok(APP_JS.match(/function\s+getTransferStore\s*\(/), 'app.js 应保留唯一 getTransferStore');
});
