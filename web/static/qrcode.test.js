/* SPDX-License-Identifier: Apache-2.0 */
/*
 * qrcode.test.js —— sproxyQR 包装层（vendored qrcode-generator）的结构性断言。
 *
 * 结构性断言（简报约定）：尺寸 ≥21 且 17+4n、四角 finder 在位、内容位非全零、幂等，
 * renderToSVG 含 <svg + xmlns。不做逐位比对（底层是久经测试的 MIT 库；本层只保证
 * 包装契约与渲染输出形态正确）。
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

const qr = require(path.join(__dirname, 'qrcode.js'));
const { renderToMatrix, renderToSVG } = qr;

test('renderToMatrix 尺寸满足 17+4n 且 ≥21（自动版本）', () => {
  const texts = ['a', 'x'.repeat(50), 'x'.repeat(100), 'x'.repeat(300)];
  for (const text of texts) {
    const m = renderToMatrix(text);
    const n = m.length;
    assert.ok(n >= 21, '尺寸 ≥21，实际 ' + n);
    assert.strictEqual((n - 17) % 4, 0, '尺寸 17+4n，实际 ' + n);
    // 自动版本随内容增长（粗略单调不减）
  }
  const small = renderToMatrix('a').length;
  const large = renderToMatrix('x'.repeat(300)).length;
  assert.ok(large >= small, '更长内容应选择≥的版本');
});

test('renderToMatrix 四角 finder 图案在位（7x7 空心矩形 + 中心点）且分隔带存在', () => {
  const m = renderToMatrix('otpauth://totp/test?secret=JBSWY3DPEHPK3PXP');
  const n = m.length;
  // 三个 7x7 finder：外圈暗、旁心亮、中心 3x3 暗（r0/c0 = 各自左上角）。
  for (const [r0, c0] of [[0, 0], [0, n - 7], [n - 7, 0]]) {
    assert.strictEqual(m[r0][c0], 1, 'finder 左上角应为暗（外圈）');
    assert.strictEqual(m[r0 + 1][c0 + 1], 0, 'finder 内圈应亮');
    assert.strictEqual(m[r0 + 2][c0 + 2], 1, 'finder 中心 3x3 应为暗');
    assert.strictEqual(m[r0 + 3][c0 + 3], 1, 'finder 中心应为暗');
    assert.strictEqual(m[r0 + 6][c0], 1, 'finder 左缘下端点暗');
    assert.strictEqual(m[r0 + 6][c0 + 6], 1, 'finder 右下角暗');
  }
  // 分隔带：左上 finder 的右侧（col 7）与下方（row 7）均为亮。
  for (let i = 0; i < 8; i++) {
    assert.strictEqual(m[i][7], 0, '左上 finder 右侧分隔带 (' + i + ',7) 应亮');
    assert.strictEqual(m[7][i], 0, '左上 finder 下方分隔带 (7,' + i + ') 应亮');
  }
});

test('renderToMatrix 内容位非全零且密度合理', () => {
  const m = renderToMatrix('otpauth://totp/demo?secret=JBSWY3DPEHPK3PXP&issuer=x');
  const n = m.length;
  let ones = 0;
  for (let y = 0; y < n; y++) for (let x = 0; x < n; x++) if (m[y][x]) ones++;
  assert.ok(ones > 0, '矩阵不得全零');
  const ratio = ones / (n * n);
  assert.ok(ratio > 0.1 && ratio < 0.9, '密度应在合理区间（ones=' + ones + '/' + n * n + '=' + ratio.toFixed(2) + '）');
});

test('renderToMatrix 相同输入幂等（两次调用矩阵完全一致）', () => {
  const text = 'otpauth://totp/idem?secret=JBSWY3DPEHPK3PXP';
  const a = renderToMatrix(text);
  const b = renderToMatrix(text);
  assert.strictEqual(a.length, b.length);
  for (let y = 0; y < a.length; y++) assert.deepStrictEqual(Array.from(a[y]), Array.from(b[y]));
});

test('renderToSVG 返回内联 SVG（含 xmlns + 与矩阵同尺寸）', () => {
  const text = 'otpauth://totp/svg?secret=JBSWY3DPEHPK3PXP';
  const svg = renderToSVG(text);
  assert.ok(svg.startsWith('<svg'), '应以内联 <svg 开头');
  assert.ok(svg.includes('xmlns="http://www.w3.org/2000/svg"'), '应含 xmlns');
  assert.ok(svg.includes('viewBox="0 0 ' + renderToMatrix(text).length), 'viewBox 应与矩阵同尺寸');
  assert.ok(svg.includes('<rect'), '应含深色模块 rect');
  assert.ok(!svg.includes('</svg>') || svg.endsWith('</svg>'), '应闭合 </svg>');
});
