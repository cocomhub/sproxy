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

const { volManageFormHtml, volManageListHtml, volOccupiedText, composeNestedTarget, composeCapacityText, capacityInputHtml } = require('./vol-manage-format.js');

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

test('volManageFormHtml volume-select 候选含本地 config 卷（allow_wrapper=false 仍保留本地卷）', () => {
  // I1/D3 修复契约：底层卷候选源改 GET /api/volumes（全量可见卷，含本地 config 卷——
  // 默认安装只有本地基座卷时 secretdata/secrets 建卷的 target 必须能选中 main）。
  const html = volManageFormHtml({
    type: 'secretdata',
    category: 'wrapper',
    fields: [{ key: 'target', label: '底层卷', type: 'volume-select', allow_wrapper: true }],
    volumes: [
      { name: 'main', type: 'local', category: 'mt-local' },
      { name: 'vault', type: 'secretdata', category: 'wrapper' },
    ],
  });
  assert.match(html, /value="main"/, '本地 config 卷 main 应在底层卷候选');
  assert.match(html, /value="vault"/, 'allow_wrapper=true 时封装卷 vault 应保留在候选');
});

test('volManageFormHtml volume-select allow_wrapper=false 时本地卷保留、wrapper 卷过滤', () => {
  const html = volManageFormHtml({
    type: 'foo',
    category: 'linked',
    fields: [{ key: 'target', label: '底层卷', type: 'volume-select' }],
    volumes: [
      { name: 'main', type: 'local', category: 'mt-local' },
      { name: 'outer', type: 'secretdata', category: 'wrapper' },
    ],
  });
  assert.match(html, /value="main"/);
  assert.ok(!html.includes('value="outer"'), 'allow_wrapper=false 不应包含 wrapper 卷 outer');
});

test('volManageFormHtml 渲染 secret_url 文本字段（D1：secretdata 建卷 schema 补密钥引用）', () => {
  const html = volManageFormHtml({
    type: 'secretdata',
    category: 'wrapper',
    fields: [
      { key: 'target', label: '底层卷', type: 'volume-select', required: true, allow_wrapper: true },
      { key: 'secret_url', label: '密钥引用', type: 'text', required: true },
    ],
  });
  assert.match(html, /name="secret_url"/);
  assert.match(html, /type="text"/);
  assert.match(html, /密钥引用/);
});

test('volManageFormHtml 渲染 linked 后端 url 文本字段（D2：sftp/webdav 建卷表单有 url 输入）', () => {
  const html = volManageFormHtml({
    type: 'sftp',
    category: 'linked',
    fields: [
      { key: 'url', label: 'SFTP 地址', type: 'text', required: true },
      { key: 'password', label: '密码', type: 'text' },
    ],
  });
  assert.match(html, /name="url"/);
  assert.match(html, /required/);
  assert.match(html, /name="password"/);
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

test('volManageFormHtml volume-select 渲染嵌套子目录输入（target_subdir）', () => {
  const html = volManageFormHtml({
    type: 'secretdata',
    category: 'wrapper',
    fields: [{ key: 'target', label: '底层卷', type: 'volume-select', required: true, allow_wrapper: true }],
    volumes: [{ name: 'main', category: 'mt-local' }],
  });
  assert.match(html, /name="target_subdir"/);
  assert.match(html, /嵌套子目录/);
});

test('composeNestedTarget 卷/子目录 拼装（嵌套封装 target）', () => {
  assert.equal(composeNestedTarget('main', 'videos'), 'main/videos');
  assert.equal(composeNestedTarget('main', ''), 'main'); // 子目录空 → 传统整卷语义
  assert.equal(composeNestedTarget('', 'videos'), ''); // 未选底层卷 → 空（必填由后端兜底）
  assert.equal(composeNestedTarget('main', 'a/b'), 'main/a/b'); // 多级子目录
  assert.equal(composeNestedTarget(' main ', ' videos '), 'main/videos'); // 去空白
  assert.equal(composeNestedTarget(null, 'x'), ''); // 卷名缺失
});

test('volManageListHtml 卷名转义（防 XSS）', () => {
  const html = volManageListHtml({ volumes: [{ name: 'a"b<script>', type: 'x', category: 'y' }] });
  assert.ok(html.includes('&quot;'));
  assert.ok(html.includes('&lt;script&gt;'));
  assert.ok(!html.includes('<script>'));
});

test('volManageListHtml 占用列显示嵌套封装 target（底层卷+子目录，占用只读可见）', () => {
  const html = volManageListHtml({
    volumes: [
      { name: 'vault', type: 'secretdata', category: 'wrapper', capacity: 0, extra: { target: 'main/videos' } },
      { name: 'plain', type: 'baidupcs', category: 'linked', capacity: 0 },
      { name: 'empty', type: 'secretdata', category: 'wrapper', capacity: 0, extra: {} },
    ],
  });
  assert.match(html, /底层 main\/videos/, '嵌套封装 target 应显示「底层 <卷>/<子目录>」占用');
  assert.match(html, /占用/, '列表应有「占用」列头');
  assert.ok(html.includes('>main/videos</span>') || html.includes('底层 main/videos'), '占用列含 main/videos');
});

test('volOccupiedText 纯函数：嵌套 target → 底层文案；非嵌套/无 target → 空', () => {
  assert.equal(volOccupiedText({ extra: { target: 'main/videos' } }), '底层 main/videos');
  assert.equal(volOccupiedText({ extra: { target: 'main' } }), 'main'); // 非嵌套 wrapper target 显示卷名
  assert.equal(volOccupiedText({ extra: {} }), '');
  assert.equal(volOccupiedText({}), '');
  assert.equal(volOccupiedText({ extra: { target: '  main/videos  ' } }), '底层 main/videos'); // 去空白
  assert.equal(volOccupiedText(null), '');
});

// ---- 容量单位下拉（方案B 前端，2026-10-06）----

test('volManageFormHtml 容量字段含单位下拉（MB/GB/TB/MiB/GiB/TiB）', () => {
  const html = volManageFormHtml({
    type: 'foo', category: 'linked',
    fields: [{ key: 'target', label: '底层卷', type: 'volume-select' }],
    volumes: [{ name: 'main', category: 'mt-local' }],
  });
  assert.match(html, /name="capacity"/);
  assert.match(html, /name="capacity_unit"/);
  ['MB', 'GB', 'TB', 'MiB', 'GiB', 'TiB'].forEach(function (u) {
    assert.ok(html.indexOf('value="' + u + '"') >= 0, '单位下拉缺 ' + u);
  });
});

test('capacityInputHtml 含容量输入 + 单位下拉', () => {
  const html = capacityInputHtml();
  assert.match(html, /name="capacity"/);
  assert.match(html, /name="capacity_unit"/);
  assert.match(html, /value="GiB"/);
  assert.match(html, /value="TiB"/);
});

test('composeCapacityText 数字 + 单位拼接', () => {
  assert.equal(composeCapacityText('100', 'GiB'), '100GiB');
  assert.equal(composeCapacityText('2.5', 'TB'), '2.5TB');
  assert.equal(composeCapacityText('100', ''), '100'); // 无单位 → 纯数字字节
  assert.equal(composeCapacityText('', 'GiB'), ''); // 数字空 → 空串（容量留空不限）
  assert.equal(composeCapacityText('  100 ', ' GiB '), '100GiB'); // 去空白
  assert.equal(composeCapacityText(' 100GiB ', 'MB'), '100GiB'); // 已带单位：不覆盖原输入
  assert.equal(composeCapacityText('abc', 'GiB'), 'abc'); // 非数字：原样透传（parse 兜底报错）
  assert.equal(composeCapacityText(null, 'MB'), ''); // 未填
});

test('appRender.parseSizeText 解析各单位（容量单位下拉对应）', () => {
  const ar = global.appRender;
  assert.equal(ar.parseSizeText(''), 0);
  assert.equal(ar.parseSizeText('100'), 100); // 纯数字字节
  assert.equal(ar.parseSizeText('1KB'), 1000);
  assert.equal(ar.parseSizeText('1MB'), 1000 * 1000);
  assert.equal(ar.parseSizeText('1GB'), 1000 * 1000 * 1000);
  assert.equal(ar.parseSizeText('1TB'), 1000 * 1000 * 1000 * 1000);
  assert.equal(ar.parseSizeText('1MiB'), 1024 * 1024);
  assert.equal(ar.parseSizeText('1GiB'), 1024 * 1024 * 1024);
  assert.equal(ar.parseSizeText('1TiB'), 1024 * 1024 * 1024 * 1024);
  assert.equal(ar.parseSizeText('100GiB'), 100 * 1024 * 1024 * 1024);
  assert.equal(ar.parseSizeText('2.5TB'), Math.round(2.5 * 1000 * 1000 * 1000 * 1000));
  // M4：裸 K/M/G/T（与 Go sizex.ParseSize 对齐，1000·based）+ 裸 Ki/Mi/Gi/Ti（1024·based）。
  assert.equal(ar.parseSizeText('100G'), 100 * 1000 * 1000 * 1000);
  assert.equal(ar.parseSizeText('1K'), 1000);
  assert.equal(ar.parseSizeText('1M'), 1000 * 1000);
  assert.equal(ar.parseSizeText('1T'), 1000 * 1000 * 1000 * 1000);
  assert.equal(ar.parseSizeText('1Ki'), 1024);
  assert.equal(ar.parseSizeText('1Mi'), 1024 * 1024);
  assert.equal(ar.parseSizeText('1Gi'), 1024 * 1024 * 1024);
  assert.equal(ar.parseSizeText('2Ti'), 2 * 1024 * 1024 * 1024 * 1024);
  assert.throws(() => ar.parseSizeText('abc'), /无法解析大小/);
});