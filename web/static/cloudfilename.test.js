/* SPDX-License-Identifier: Apache-2.0 */
/*
 * cloudfilename.test.js —— 浏览器端文件名生成/清理规则的单元测试。
 *
 * 运行方式（仓库根目录）：
 *   node --test web/static/cloudfilename.test.js
 *
 * 语料与 Go 端共享：pkg/cloudfilename/testdata/cases.json。
 * Go 测试 pkg/cloudfilename 与本测试都断言该语料，从而保证双端对同一
 * URL 推导出完全一致的默认文件名（wget 行为）。
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');
const fs = require('node:fs');

const { genDefaultFilename, filepathSafe, safeDefaultFromURL, validateEntry, validateEntries } = require('./cloudfilename.js');

const fixturePath = path.join(__dirname, '../../pkg/cloudfilename/testdata/cases.json');
const cases = JSON.parse(fs.readFileSync(fixturePath, 'utf8'));

test('safeDefaultFromURL 与 Go pkg/cloudfilename 共享语料一致', () => {
  for (const [url, want] of Object.entries(cases)) {
    assert.strictEqual(safeDefaultFromURL(url), want, `URL: ${url}`);
  }
});

test('filepathSafe 清理规则与 Go pkg/cloudfilename.Safe 一致', () => {
  const safeCases = [
    ['a/b.txt', 'a_b.txt'],
    ['a?b', 'a_b'],
    ['a:b', 'a_b'],
    ['..file.txt..', 'file.txt'],
    ['... ...', 'download'],
    ['', 'download'],
    ['index.html_a=v', 'index.html_a=v'],
    ['my file.txt', 'my file.txt'],
    ['a' + String.fromCharCode(0) + 'b', 'ab'],
    ['  file.txt  ', 'file.txt'],
    // Windows 保留设备名
    ['CON', '_CON'],
    ['con.txt', '_con.txt'],
    ['COM1', '_COM1'],
    ['lpt9', '_lpt9'],
    ['CONtext.txt', 'CONtext.txt'], // CON 后接其他字符不命中
    // 超长文件名按字节截断（保留扩展名，不劈开 UTF-8）
    ['a'.repeat(300) + '.txt', 'a'.repeat(250) + '.txt'],
    ['好'.repeat(100) + '.zip', '好'.repeat(83) + '.zip'],
  ];
  for (const [input, want] of safeCases) {
    assert.strictEqual(filepathSafe(input), want, `input: ${JSON.stringify(input)}`);
  }
});

test('genDefaultFilename + filepathSafe 完整链路', () => {
  const chainCases = [
    ['https://example.com/xx/?a=v', 'index.html_a=v'],
    ['https://example.com/a%2Fb.txt', 'b.txt'],
    ['https://example.com/my%20file.txt', 'my file.txt'],
    ['https://example.com/path/file.txt?x=1', 'file.txt_x=1'],
  ];
  for (const [url, want] of chainCases) {
    assert.strictEqual(filepathSafe(genDefaultFilename(url)), want, `URL: ${url}`);
  }
});

test('safeDefaultFromURL 安全语义（? 被替换为 _）', () => {
  assert.strictEqual(safeDefaultFromURL('https://example.com/xx/?a=v'), 'index.html_a=v');
  assert.strictEqual(safeDefaultFromURL('https://example.com/file.txt?x=1'), 'file.txt_x=1');
});

test('validateEntry URL 格式校验（结构化结果）', () => {
  assert.deepStrictEqual(validateEntry(''), { valid: false, code: 'EMPTY_URL', message: 'URL is empty' });
  assert.deepStrictEqual(validateEntry('ftp://e.com/a.zip'), { valid: false, code: 'BAD_SCHEME', message: 'unsupported URL scheme (only http/https)' });
  assert.deepStrictEqual(validateEntry('https://e.com/a.zip'), { valid: true, code: 'OK', message: '' });
  // 尾随空白并入路径，不影响校验（Go url.Parse 同样把尾随空白当作路径 → 合法）。
  assert.deepStrictEqual(validateEntry('https://e.com/a.zip '), { valid: true, code: 'OK', message: '' });
  // 前导空白：JS 的正则要求 scheme 位于开头 → BAD_SCHEME；
  // Go url.Parse 报错（first path segment…colon）→ 同样拒绝。两者 reject 语义一致。
  assert.strictEqual(validateEntry(' http://e.com/a.zip').valid, false);
});

test('validateEntries 同 URL 不同 filename 去重', () => {
  assert.strictEqual(validateEntries([{ url: 'https://e.com/a.zip' }]).valid, true);
  assert.strictEqual(
    validateEntries([
      { url: 'https://e.com/a.zip', filename: 'a.zip' },
      { url: 'https://e.com/a.zip', filename: 'a.zip' },
    ]).valid, true);
  const dup = validateEntries([
    { url: 'https://e.com/a.zip', filename: 'a.zip' },
    { url: 'https://e.com/a.zip', filename: 'b.zip' },
  ]);
  assert.strictEqual(dup.valid, false);
  assert.strictEqual(dup.code, 'DUP_URL');
  // 单条目 URL 非法时返回该条目的错误
  const bad = validateEntries([{ url: '' }]);
  assert.strictEqual(bad.valid, false);
  assert.strictEqual(bad.code, 'EMPTY_URL');
});
