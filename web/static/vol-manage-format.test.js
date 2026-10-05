// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// vol-manage-format.test.js node --test 单测（卷管理面板渲染纯函数）。

'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

// vol-manage-format.js 引用共享 appRender.escHtml（禁自建 escHtml，见 CLAUDE.md 记录）——
// node 环境下 app-render.js 以 module 形态提供，须先注入全局。
global.appRender = require(path.join(__dirname, 'app-render.js'));

const { volManageFormHtml, volManageListHtml } = require('./vol-manage-format.js');

test('volManageFormHtml 按 schema 渲染字段', () => {
  const html = volManageFormHtml({
    type: 'secretdata',
    category: 'wrapper',
    fields: [
      { key: 'target', label: '底层卷', type: 'volume-select', required: true },
      { key: 'algorithm', label: '算法', type: 'enum', options: ['shardseal-high', 'shardseal-standard'] },
      { key: 'endpoint', label: '端点', type: 'text' },
      { key: 'enabled', label: '启用', type: 'bool' },
      { key: 'workers', label: '并发', type: 'number' },
    ],
    volumes: [
      { name: 'leaf', type: 'baidupcs', category: 'linked' },
      { name: 'outer', type: 'secretdata', category: 'wrapper' },
    ],
  });
  // volume-select 必填：name + required 属性（value-select 下拉）
  assert.match(html, /name="target"/);
  assert.match(html, /required/);
  // enum 选项
  assert.match(html, /shardseal-high/);
  assert.match(html, /shardseal-standard/);
  // text / bool / number 控件
  assert.match(html, /name="endpoint"/);
  assert.match(html, /type="checkbox"/);
  assert.match(html, /name="enabled"/);
  assert.match(html, /type="number"/);
  // data-type 隐藏字段（卷类型后端驱动，前端不硬编码）
  assert.match(html, /data-type="secretdata"/);
  assert.match(html, /name="type"/);
  // 卷名输入
  assert.match(html, /name="name"/);
});

test('volManageFormHtml volume-select 仅含非 wrapper 卷（allow_wrapper=false）', () => {
  const html = volManageFormHtml({
    type: 'foo',
    category: 'linked',
    fields: [{ key: 'target', label: '底层卷', type: 'volume-select' }],
    volumes: [
      { name: 'leaf', category: 'linked' },
      { name: 'outer', category: 'wrapper' },
    ],
  });
  // leaf 出现、wrapper 的 outer 不应出现在候选（allow_wrapper 缺省 falsy）
  assert.match(html, /value="leaf"/);
  assert.ok(!html.includes('value="outer"'), 'allow_wrapper=false 不应包含 wrapper 卷 outer');
});

test('volManageFormHtml volume-select allowWrapper=true 时含 wrapper 卷', () => {
  const html = volManageFormHtml({
    type: 'secretdata',
    category: 'wrapper',
    fields: [{ key: 'target', label: '底层卷', type: 'volume-select', allow_wrapper: true }],
    volumes: [{ name: 'leaf', category: 'linked' }, { name: 'outer', category: 'wrapper' }],
  });
  assert.match(html, /value="leaf"/);
  assert.match(html, /value="outer"/);
});

test('volManageListHtml 含卷名 + 类型 + category + 删除按钮', () => {
  const html = volManageListHtml({
    volumes: [{ name: 'vault', type: 'secretdata', category: 'wrapper', capacity: 0 }],
  });
  assert.match(html, /vault/);
  assert.match(html, /secretdata/);
  assert.match(html, /wrapper/);
  assert.match(html, /data-delete-volume/);
});

test('volManageListHtml 空列表显示空提示', () => {
  const html = volManageListHtml({ volumes: [] });
  assert.ok(html.includes('暂无用户卷'));
  assert.ok(volManageListHtml(null).includes('暂无用户卷'));
});

test('volManageListHtml 卷名转义（防 XSS）', () => {
  const html = volManageListHtml({ volumes: [{ name: 'a"b<script>', type: 'x', category: 'y' }] });
  assert.ok(html.includes('&quot;'));
  assert.ok(html.includes('&lt;script&gt;'));
  assert.ok(!html.includes('<script>'));
});