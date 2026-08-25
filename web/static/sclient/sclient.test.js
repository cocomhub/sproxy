/* SPDX-License-Identifier: Apache-2.0 */
/*
 * sclient.test.js —— sclient 前端库基础设施（crypto/log/config）的单元测试。
 *
 * 运行方式（仓库根目录）：
 *   node --test web/static/sclient/sclient.test.js
 *
 * 用 node:test + assert/strict，不用第三方断言。
 * 覆盖：
 *   - HKDF 派生隧道密钥与 Go tunnel.DeriveTunnelKey 已知向量完全一致
 *   - SHA-256 / HMAC-SHA256 canonical 签名与 Go sproxysig 已知向量一致
 *   - AES-GCM 加解密往返 + 默认配置 / localStorage override 读写
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const cryptoLib = require('./crypto.js');
const { defaultConfig, applyOverride, readLocalOverride } = require('./config.js');
const log = require('./log.js');

// ---- 已知向量（来自 Go 实测，任务 2 注入值，必须逐字使用） ----
const BASE_SK = '2b40d5b60e6792134f07b44b46e2e19fb72f967136868015cb922d720c1aa6f5';
const HKDF_FIXTURES = [
  { mesh: 'meshA', keyHex: '59318bd8a04fe849669d6a0fe20f4b4011bcf759f603b02203509bdc1081c5ae' },
  { mesh: 'meshB', keyHex: 'ef8f94900b59957412ecfc07d76d4279057ca1f3ea9567df245eadc8b0e28232' },
  { mesh: '', keyHex: 'f39c19bc54f273052bf8425665dee1f5b5802798b739c1bfbf968786ab703d1c' },
];

const HMAC_FIXTURE = {
  ak: 'sk-meshA-1234567890abcdef',
  ts: '1700000000000',
  exp: '1700000300000',
  nonce: '00112233445566778899aabbccddeeff',
  method: 'POST',
  path: '/tunnel',
  query: '',
  bodySha256: 'UNSIGNED',
  sigHex: 'a2aae8dddefaed41efc38d8d0173b9c7100f967df27075766cb0312689588ada',
};

function hkdfCanonicalInput(mesh) {
  // HKDF 输入 = salt "sproxy-tunnel-key-v1" + info = mesh 字符串，secret = 原始密钥字节
  return { saltStr: 'sproxy-tunnel-key-v1', infoStr: mesh };
}

function hmacCanonicalInput(f) {
  // canonical 拼接："sproxy-sig/v1\n" + ak + \n + ts + \n + exp + \n + nonce + \n + method + \n + path + \n + query + \n + body_sha256
  return 'sproxy-sig/v1\n' + f.ak + '\n' + f.ts + '\n' + f.exp + '\n' + f.nonce + '\n' + f.method + '\n' + f.path + '\n' + f.query + '\n' + f.bodySha256;
}

test('HKDF deriveTunnelKey 与 Go tunnel.DeriveTunnelKey 向量一致', async () => {
  for (const fx of HKDF_FIXTURES) {
    const key = await cryptoLib.deriveTunnelKey(BASE_SK, fx.mesh);
    assert.strictEqual(cryptoLib.bytesToHex(key), fx.keyHex, `mesh=${JSON.stringify(fx.mesh)}`);
  }
});

test('hmacSHA256Hex canonical 与 Go sproxysig 向量一致', async () => {
  const canonical = hmacCanonicalInput(HMAC_FIXTURE);
  const sig = await cryptoLib.hmacSHA256Hex(BASE_SK, canonical);
  assert.strictEqual(sig, HMAC_FIXTURE.sigHex);
});

test('sha256Hex 基础正确（RFC 6234 向量）', async () => {
  assert.strictEqual(await cryptoLib.sha256Hex(''), 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855');
  assert.strictEqual(await cryptoLib.sha256Hex('abc'), 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
});

test('hexToBytes / bytesToHex 往返一致', () => {
  const raw = '00112233aabbccddEEff';
  assert.strictEqual(cryptoLib.bytesToHex(cryptoLib.hexToBytes(raw)), raw.toLowerCase());
  assert.deepStrictEqual(cryptoLib.hexToBytes('a0b1c2'), new Uint8Array([0xa0, 0xb1, 0xc2]));
  assert.deepStrictEqual(cryptoLib.hexToBytes(''), new Uint8Array(0));
  // 长度不足/非法字符时按 0 解析（与 Go 侧 hex.Decode 的宽松兼容不在本层承诺，JS 由调用方保证）
});

test('AES-GCM 加解密往返', async () => {
  const key = await cryptoLib.importAesGcmKey(BASE_SK);
  const plain = new TextEncoder().encode('hello sclient aes-gcm');
  const ct = await cryptoLib.aesGcmEncrypt(key, plain);
  const back = await cryptoLib.aesGcmDecrypt(key, ct.iv, ct.ciphertext);
  assert.deepStrictEqual(back, plain);
  // 篡改密文导致解密失败
  const bad = ct.ciphertext.slice();
  bad[0] ^= 0xFF;
  // WebCrypto 认证失败抛 OperationError（Node v24）；拒绝匹配不绑定特定文案，捕获/断言其被拒绝即可。
  await assert.rejects(cryptoLib.aesGcmDecrypt(key, ct.iv, bad));
}, { timeout: 10000 });

test('config 默认值 + applyOverride/readLocalOverride', () => {
  const def = defaultConfig();
  assert.strictEqual(def.baseUrl, '');
  assert.strictEqual(def.accessKey, '');
  assert.strictEqual(def.accessKeySecret, '');
  assert.strictEqual(def.transport, 'auto');
  assert.strictEqual(def.tunnelDefault, true);
  assert.strictEqual(def.overrideKey, 'sproxy_web_transport_override');
  assert.ok(Number.isFinite(def.chunkThreshold));
  assert.ok(Number.isInteger(def.chunkThreshold));
  // applyOverride：非法值保留默认
  assert.strictEqual(applyOverride({ baseUrl: 'http://x' }).baseUrl, 'http://x');
  assert.strictEqual(applyOverride({ transport: 'quic' }).transport, 'auto');
  // readLocalOverride：回退为默认 overrideKey
  const o1 = readLocalOverride();
  assert.strictEqual(o1.overrideKey, 'sproxy_web_transport_override');
  assert.strictEqual(o1.transport, 'auto');
});

test('readLocalOverride 成功路径（localStorage 已注入值）', () => {
  // 探测并保存原有 localStorage 描述符（Node 默认无），在 finally 恢复
  const desc = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  const backup = desc ? desc : { value: undefined, configurable: true, writable: true };
  try {
    Object.defineProperty(globalThis, 'localStorage', {
      configurable: true,
      value: {
        getItem: (k) => (k === 'sproxy_web_transport_override' ? JSON.stringify({ transport: 'direct' }) : null),
      },
    });
    const o = readLocalOverride();
    assert.strictEqual(o.transport, 'direct');
    assert.strictEqual(o.overrideKey, 'sproxy_web_transport_override');
    // 既有字段正确合并：其余选项保持默认值
    assert.strictEqual(o.applied.baseUrl, '');
    assert.strictEqual(o.applied.accessKey, '');
    assert.strictEqual(o.applied.tunnelDefault, true);
    assert.strictEqual(o.applied.chunkThreshold, 8 * 1024 * 1024);
  } finally {
    Object.defineProperty(globalThis, 'localStorage', backup);
  }
});

test('log.js 基本功能', () => {
  const levels = ['debug', 'info', 'warn', 'error'];
  const messages = [];
  const fakeConsole = {};
  for (const lvl of levels) {
    fakeConsole[lvl] = (...args) => messages.push([lvl, ...args]);
  }
  const old = log.setConsole(fakeConsole);
  try {
    log.setLevel('debug');
    log.debug('d1', { a: 1 });
    log.info('i1');
    log.warn('w1');
    log.error('e1');
    assert.ok(messages.length >= 4);
    assert.ok(messages.some(([lvl]) => lvl === 'debug'));
    assert.ok(messages.some(([lvl]) => lvl === 'info'));
    assert.ok(messages.some(([lvl]) => lvl === 'warn'));
    assert.ok(messages.some(([lvl]) => lvl === 'error'));
  } finally {
    log.setConsole(old);
  }
});

test('log.js 级别过滤', () => {
  const messages = [];
  const fakeConsole = { debug: (...a) => messages.push(a), info: (...a) => messages.push(a), warn: (...a) => messages.push(a), error: (...a) => messages.push(a) };
  const oldConsole = log.setConsole(fakeConsole);
  const oldLevel = log.setLevel('warn');
  try {
    log.debug('no');
    log.info('no');
    log.warn('yes');
    log.error('yes');
    assert.deepStrictEqual(messages.map((m) => m[0]), ['yes', 'yes']);
  } finally {
    log.setLevel(oldLevel);
    log.setConsole(oldConsole);
  }
});
