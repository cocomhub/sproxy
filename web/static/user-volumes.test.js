/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * user-volumes.test.js —— user-volumes.js 用户卷面板纯渲染模块单测。
 *
 * 运行：node --test web/static/user-volumes.test.js（已并入 make web-test）。
 * 覆盖：表格渲染 / 创建表单 / extra JSON 解析。
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

const r = require(path.join(__dirname, 'user-volumes.js'));

// ---- userVolumesTableHtml ----
test('userVolumesTableHtml 空列表渲染空提示', () => {
  const html = r.userVolumesTableHtml([]);
  assert.ok(html.includes('暂无用户卷'));
  assert.ok(html.includes('创建'));
});

test('userVolumesTableHtml 渲染卷行（name/type/capacity/删除按钮）', () => {
  const html = r.userVolumesTableHtml([
    { name: 'disk1', type: 'baidupcs', capacity: 107374182400, extra: { bduss: 'x' } },
  ]);
  assert.ok(html.includes('disk1'));
  assert.ok(html.includes('baidupcs'));
  assert.ok(html.includes('100.00 GB'));
  assert.ok(html.includes('data-name="disk1"'));
  assert.ok(html.includes('删除'));
});

test('userVolumesTableHtml 无限容量显示不限', () => {
  const html = r.userVolumesTableHtml([{ name: 'd2', type: 'baidupcs', capacity: 0 }]);
  assert.ok(html.includes('不限'));
});

test('userVolumesTableHtml 卷名 HTML 转义（防注入）', () => {
  const html = r.userVolumesTableHtml([{ name: '<script>x</script>', type: 'baidupcs', capacity: 0 }]);
  assert.ok(!html.includes('<script>'));
  assert.ok(html.includes('&lt;script&gt;'));
});

// ---- createUserVolumeFormHtml ----
test('createUserVolumeFormHtml 含 name/type/extra/capacity 字段', () => {
  const html = r.createUserVolumeFormHtml(['baidupcs']);
  assert.ok(html.includes('name'));
  assert.ok(html.includes('type'));
  assert.ok(html.includes('extra'));
  assert.ok(html.includes('capacity'));
  assert.ok(html.includes('baidupcs'));
});

test('createUserVolumeFormHtml type 下拉含已注册 backend 选项', () => {
  const html = r.createUserVolumeFormHtml(['baidupcs', 's3']);
  assert.ok(html.includes('<option value="baidupcs">baidupcs</option>'));
  assert.ok(html.includes('<option value="s3">s3</option>'));
});

// ---- parseExtra ----
test('parseExtra 解析合法 JSON 对象', () => {
  assert.deepStrictEqual(
    r.parseExtra('{"bduss":"abc","baidu_root":"/disk1"}'),
    { bduss: 'abc', baidu_root: '/disk1' }
  );
});

test('parseExtra 空串/空白返回空对象', () => {
  assert.deepStrictEqual(r.parseExtra(''), {});
  assert.deepStrictEqual(r.parseExtra('  '), {});
});

test('parseExtra 非法 JSON 返回错误', () => {
  const got = r.parseExtra('{bad json');
  assert.ok(typeof got === 'object' && got !== null && got.error, '应返回 {error:...}');
});

test('parseExtra 非对象（数组/标量）返回错误', () => {
  assert.ok(r.parseExtra('[1,2]').error, '数组应报错');
  assert.ok(r.parseExtra('"str"').error, '字符串应报错');
  assert.ok(r.parseExtra('42').error, '数字应报错');
});
