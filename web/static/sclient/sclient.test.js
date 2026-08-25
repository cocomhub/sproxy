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
const sig = require('./sig.js');

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
    // 恢复原描述符（保持「readLocalOverride 成功路径」后续用例互不污染）。
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

// ==================== sig.js 追加用例（任务 3） ====================

test('sig.signHeader unsigned canonical 对齐 Go sproxysig 已知向量', async () => {
  // 注入值逐字使用（见 hmacCanonicalInput 顶部向量）：SK=BASE_SK，AK/ts/exp/nonce
  // 固定，body_sha256='UNSIGNED'。signHeader 内部用 hmacSHA256Hex（baseSK 编码为
  // UTF-8）计算 canonical 的 HMAC——与 Go hmac.New(sha256.New, []byte(sk)) 同一字节。
  const header = await sig.signHeader('POST', '/tunnel', null, {
    ak: HMAC_FIXTURE.ak,
    ts: HMAC_FIXTURE.ts,
    exp: HMAC_FIXTURE.exp,
    nonce: HMAC_FIXTURE.nonce,
    secret: BASE_SK,
    unsigned: true,
  });
  assert.strictEqual(header, 'SproxySig v=1 ak=' + HMAC_FIXTURE.ak + ' ts=' + HMAC_FIXTURE.ts + ' exp=' + HMAC_FIXTURE.exp + ' nonce=' + HMAC_FIXTURE.nonce + ' body_sha256=UNSIGNED sig=' + HMAC_FIXTURE.sigHex);
});

test('sig.buildCanonical 分段拼接与 Go Header.Canonical 一致', () => {
  const c = sig.buildCanonical('POST', '/tunnel', {
    ak: HMAC_FIXTURE.ak,
    ts: HMAC_FIXTURE.ts,
    exp: HMAC_FIXTURE.exp,
    nonce: HMAC_FIXTURE.nonce,
    bodySha256: 'UNSIGNED',
  });
  // 9 段（sproxy-sig/v1、ak、ts、exp、nonce、method、path、query、body_sha256）
  const seg = c.split('\n');
  assert.strictEqual(seg.length, 9);
  assert.strictEqual(c, hmacCanonicalInput(HMAC_FIXTURE));
});

test('sig.signHeader 常规 body：sha256 hashing 正确 + 完整头部', async () => {
  const bodyStr = 'hello sproxy sig body';
  const body = new TextEncoder().encode(bodyStr);
  // 短 body 的 SHA-256 hex（等价于 Go BodyHash(body)）。
  const bodyHashHex = await cryptoLib.sha256Hex(body);
  // 独立构造 canonical（与 signHeader 内部同一拼接；signHeader 会把
  // '/upload?q=1' 拆成 path='/upload' 与 query='q=1'——对齐 Go
  // EscapedPath/RawQuery 语义）。
  const expectedCanonical =
    'sproxy-sig/v1\n' + HMAC_FIXTURE.ak + '\n' + HMAC_FIXTURE.ts + '\n' + HMAC_FIXTURE.exp + '\n' +
    HMAC_FIXTURE.nonce + '\nPOST\n/upload\nq=1\n' + bodyHashHex;
  const header = await sig.signHeader('POST', '/upload?q=1', body, {
    ak: HMAC_FIXTURE.ak,
    ts: HMAC_FIXTURE.ts,
    exp: HMAC_FIXTURE.exp,
    nonce: HMAC_FIXTURE.nonce,
    secret: BASE_SK,
  });
  // 完整头部格式 + body 哈希字段
  assert.ok(header.startsWith('SproxySig v=1 ak=' + HMAC_FIXTURE.ak), header);
  assert.ok(header.indexOf('body_sha256=' + bodyHashHex) >= 0, '头部必须携带 body 的 sha256 hex');
  // 提取 sig 并与独立复算的 canonical HMAC 一致（验证 canonical 分段/哈希拼接）
  const bodySig = header.split(' sig=')[1];
  assert.strictEqual(bodySig, await cryptoLib.hmacSHA256Hex(BASE_SK, expectedCanonical));
  // 非 unsigned → body_sha256 不是 UNSIGNED
  assert.ok(header.indexOf('body_sha256=UNSIGNED') < 0);
  // body_sha256 分段占 canonical 的第 9 段（校验用 buildCanonical 复算）
  const seg = sig.buildCanonical('POST', '/upload?q=1', { ak: HMAC_FIXTURE.ak, ts: HMAC_FIXTURE.ts, exp: HMAC_FIXTURE.exp, nonce: HMAC_FIXTURE.nonce, bodySha256: bodyHashHex });
  assert.strictEqual(seg, expectedCanonical);
});

// ==================== transport.js 追加用例（任务 4） ====================
// 隧道模式：类服务端响应帧由测试借助 transport.js 内部导出的帧编码原语
// + WebCrypto 精确构造（含 AAD 上下文、4B 大端长度前缀），与 Go handler
// 的 Encrypt* 帧字节完全同构——请求/响应两向都在同一帧协议上闭环。

const transport = require('./transport.js');
const sclientConfig = require('./config.js');

function dec(s) {
  return new TextEncoder().encode(s);
}

function concatBytes() {
  let total = 0;
  for (const a of arguments) total += (a && a.length) || 0;
  const out = new Uint8Array(total);
  let off = 0;
  for (const a of arguments) {
    if (a && a.length) { out.set(a, off); off += a.length; }
  }
  return out;
}

function u32be(n) {
  const b = new Uint8Array(4);
  new DataView(b.buffer).setUint32(0, n, false);
  return b;
}

async function encryptFrameAAD(secretHex, plainUtf8, aad) {
  const key = await cryptoLib.importAesGcmKey(secretHex);
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv, additionalData: dec(aad) }, key, dec(plainUtf8));
  return concatBytes(u32be(12 + ct.byteLength), iv, new Uint8Array(ct));
}

test('accessKeyMesh 与 Go AccessKeyMesh 语义一致（隧道密钥派生 mesh 段）', () => {
  const { accessKeyMesh } = transport;
  assert.strictEqual(accessKeyMesh('sk-prod-1234567890abcdef'), 'prod');
  assert.strictEqual(accessKeyMesh('sk-prod-eu-1234567890abcdef'), 'prod-eu');
  assert.strictEqual(accessKeyMesh('sk-meshA-3f8a1234abcd5678'), 'meshA');
  assert.strictEqual(accessKeyMesh('sk-1234567890abcdef'), '');
  assert.strictEqual(accessKeyMesh('other'), '');
  assert.strictEqual(accessKeyMesh('sk-'), '');
  assert.strictEqual(accessKeyMesh('sk-prod-1234567890abcde'), '');
  assert.strictEqual(accessKeyMesh(''), '');
});

test('effectiveMode 服务端开关 × local override 全三态', () => {
  // 服务端开：默认隧道；本地 override 为显式值时直接覆盖
  transport.configure({ accessKey: AK, accessKeySecret: SK, tunnelDefault: true });
  setLocalStorageValue(null);
  assert.strictEqual(transport.effectiveMode(), 'tunnel');

  transport.configure({ accessKey: AK, accessKeySecret: SK, tunnelDefault: false });
  assert.strictEqual(transport.effectiveMode(), 'direct');

  // override 覆盖服务端状态（双显式分支）
  setLocalStorageValue({ transport: 'direct' });
  transport.configure({ accessKey: AK, accessKeySecret: SK, tunnelDefault: true });
  assert.strictEqual(transport.effectiveMode(), 'direct');
  transport.configure({ accessKey: AK, accessKeySecret: SK, tunnelDefault: false });
  assert.strictEqual(transport.effectiveMode(), 'direct');

  // override 'tunnel' 同样生效
  setLocalStorageValue({ transport: 'tunnel' });
  transport.configure({ accessKey: AK, accessKeySecret: SK, tunnelDefault: false });
  assert.strictEqual(transport.effectiveMode(), 'tunnel');

  setLocalStorageValue(null);
  transport.configure({ accessKey: '', accessKeySecret: '' });
  // 恢复基态
  transport.configure({ tunnelDefault: true });
});

test('隧道模式 coreRequest：帧发送 + SproxySig(UNSIGNED) 外层 + 响应解密', async () => {
  const origFetch = globalThis.fetch;
  try {
    transport.configure({ mode: 'tunnel', accessKey: AK, accessKeySecret: SK, tunnelDefault: true });

    const requests = [];
    globalThis.fetch = async (url, init) => {
      requests.push({ url, init });
      return new Response(await fakeTunnelBytes, {
        status: 200,
        headers: { 'Content-Type': 'application/x-tunnel-frame' },
      });
    };

    const out = await transport.coreRequest('GET', '/tunnel', { headers: { 'X-Test': '1' }, bodyBytes: null });
    // GET 无 body：请求帧 = metadata 帧 + 零 body 帧。走 /tunnel 且带 Content-Type。
    assert.ok(requests[0].init.body instanceof Uint8Array, '隧道请求体应为 Uint8Array');

    assert.strictEqual(requests.length, 1);
    assert.strictEqual(requests[0].url, '/tunnel');
    assert.strictEqual(requests[0].init.method, 'POST');
    assert.strictEqual(requests[0].init.headers['Content-Type'], 'application/x-tunnel-frame');
    const auth = requests[0].init.headers['Authorization'] || requests[0].init.headers['authorization'];
    assert.ok(auth, '隧道请求必须携带 SproxySig 头');
    assert.ok(auth.indexOf('body_sha256=UNSIGNED') >= 0, '隧道外层 body_sha256=UNSIGNED；实际: ' + auth);
    // 响应帧才是携带实际负载的载体：
    assert.strictEqual(out.status, 200);
    assert.strictEqual(out.headers['content-type'], 'application/json');
    assert.strictEqual(decodeText(out.body), 'HTTP body payload from tunnel');
  } finally {
    globalThis.fetch = origFetch;
  }
});

test('直连模式 coreRequest：GET 携带 SproxySig(sha256)', async () => {
  const origFetch = globalThis.fetch;
  try {
    transport.configure({ mode: 'direct', accessKey: AK, accessKeySecret: SK });
    const requests = [];
    globalThis.fetch = async (_url, init) => { requests.push(init); return new Response('direct body', { status: 201 }); };

    const out = await transport.coreRequest('GET', '/api/files?subdir=foo', { bodyBytes: null });

    assert.strictEqual(requests.length, 1);
    assert.strictEqual(requests[0].method, 'GET');
    const auth = requests[0].headers['Authorization'];
    assert.ok(auth, '直连请求必须携带 SproxySig 头');
    assert.ok(auth.startsWith('SproxySig v=1 ak=' + AK));
    assert.ok(auth.indexOf('body_sha256=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855') >= 0, '直连无 body 签空串 sha256: ' + auth);
    assert.ok(auth.indexOf('body_sha256=UNSIGNED') < 0);
    assert.strictEqual(out.status, 201);
    assert.strictEqual(decodeText(out.body), 'direct body');
  } finally {
    globalThis.fetch = origFetch;
  }
});

test('direct coreRequest 有 body 时签名其 SHA-256 且带 body（canonical 复核）', async () => {
  const origFetch = globalThis.fetch;
  try {
    transport.configure({ mode: 'direct', accessKey: AK, accessKeySecret: SK });
    const body = dec('direct-request-payload');
    const requests = [];
    globalThis.fetch = async (_url, init) => { requests.push(init); return new Response('ok', { status: 200 }); };

    await transport.coreRequest('POST', '/api/batch/rename', { bodyBytes: body });

    const auth = requests[0].headers['Authorization'];
    const bodySha = auth.split(' body_sha256=')[1].split(' sig=')[0];
    assert.strictEqual(bodySha, await cryptoLib.sha256Hex(body), 'body_sha256 应为 body 的 SHA-256');
    const ts = auth.split(' ts=')[1].split(' exp=')[0];
    const exp = auth.split(' exp=')[1].split(' nonce=')[0];
    const nonce = auth.split(' nonce=')[1].split(' body_sha256=')[0];
    const canonical = 'sproxy-sig/v1\n' + AK + '\n' + ts + '\n' + exp + '\n' + nonce + '\nPOST\n/api/batch/rename\n\n' + bodySha;
    const expectSig = await cryptoLib.hmacSHA256Hex(SK, canonical);
    assert.strictEqual(auth.split(' sig=')[1], expectSig);
    assert.deepStrictEqual(new Uint8Array(requests[0].body), body);
  } finally {
    globalThis.fetch = origFetch;
  }
});

test('错误路径：401 → E_AUTH 且保留 status；网络错误 → E_NETWORK', async () => {
  const origFetch = globalThis.fetch;
  try {
    transport.configure({ mode: 'direct', accessKey: AK, accessKeySecret: SK });
    globalThis.fetch = async () => new Response('nope', { status: 401 });
    let caught = null;
    try {
      await transport.coreRequest('GET', '/api/files', {});
    } catch (e) { caught = e; }
    assert.ok(caught && caught.code === 'E_AUTH' && caught.status === 401, JSON.stringify(caught));
    assert.ok(caught instanceof Error && caught.name === 'SclientError', 'SclientError 应继承 Error');

    globalThis.fetch = async () => { throw new TypeError('fetch failed'); };
    try {
      await transport.coreRequest('GET', '/api/files', {});
    } catch (e2) { caught = e2; }
    assert.ok(caught && caught.code === 'E_NETWORK', JSON.stringify(caught));
    assert.ok(caught && caught.cause, '网络错误保留原始 cause');
  } finally {
    globalThis.fetch = origFetch;
  }
});

test('隧道响应帧解密失败 → E_DECRYPT', async () => {
  const origFetch = globalThis.fetch;
  try {
    transport.configure({ mode: 'tunnel', accessKey: AK, accessKeySecret: SK, tunnelDefault: true });
    // 返回帧（meta 长度 0 → 解析失败）
    globalThis.fetch = async () => new Response(new Uint8Array([0, 0, 0, 0]), { status: 200 });
    let caught = null;
    try {
      await transport.coreRequest('GET', '/tunnel', {});
    } catch (e) { caught = e; }
    assert.ok(caught && caught.code === 'E_DECRYPT', JSON.stringify(caught));
  } finally {
    globalThis.fetch = origFetch;
  }
});

test('隧道外层签名路径与请求 URL 一致（锁定 /tunnel + 路径守卫，C1）', async () => {
  const origFetch = globalThis.fetch;
  try {
    transport.configure({ mode: 'tunnel', accessKey: AK, accessKeySecret: SK, tunnelDefault: true });
    const requests = [];
    globalThis.fetch = async (url, init) => {
      requests.push({ url, init });
      return new Response(await fakeTunnelBytes, { status: 200, headers: { 'Content-Type': 'application/x-tunnel-frame' } });
    };
    // 有效入口（pathWithQuery 带 query——C1 正是此场景：签名路径与 fetch URL 分岔则
    // 服务端按 r.URL.Path=/tunnel 验签必 401）：签名头 canonical 必须由 TUNNEL_PATH
    // /tunnel 驱动。注意：the guard 拦截使本调用不合法——但 C1 要点是确认签名头路径段
    // 与请求 URL 一致；此处用 `transport.TUNNEL_PATH` + sig.buildCanonical 复算证明。
    await transport.coreRequest('GET', transport.TUNNEL_PATH, {});
    assert.strictEqual(requests.length, 1);
    assert.strictEqual(requests[0].url, '/tunnel', 'fetch URL 必须是 /tunnel');
    const auth = requests[0].init.headers['Authorization'] || requests[0].init.headers['authorization'];
    // 从头中抽取字段，复算 canonical，断言其 path 段（第 7 段）为 /tunnel。
    const ak = auth.split(' ak=')[1].split(' ')[0];
    const ts = auth.split(' ts=')[1].split(' exp=')[0];
    const exp = auth.split(' exp=')[1].split(' nonce=')[0];
    const nonce = auth.split(' nonce=')[1].split(' body_sha256=')[0];
    const canonical = sig.buildCanonical('POST', transport.TUNNEL_PATH, { ak, ts, exp, nonce, bodySha256: 'UNSIGNED' });
    assert.strictEqual(canonical.split('\n')[6], '/tunnel', 'canonical 第 7 段（signHeader path）必须为 /tunnel');
    // 且该 canonical 正是头部 sig 的输入（证明 transport 用 /tunnel 而不是 pathWithQuery 签名）。
    assert.strictEqual(auth.split(' sig=')[1], await cryptoLib.hmacSHA256Hex(SK, canonical), '签名输入 canonical 的 path 段 = /tunnel');
  } finally {
    globalThis.fetch = origFetch;
  }
});

test('隧道模式路径守卫：非 /tunnel 的 pathWithQuery 抛 E_INTERNAL（C1）', async () => {
  const origFetch = globalThis.fetch;
  let fetched = false;
  try {
    transport.configure({ mode: 'tunnel', accessKey: AK, accessKeySecret: SK, tunnelDefault: true });
    globalThis.fetch = async () => { fetched = true; throw new Error('不应发出请求'); };
    for (const bad of ['/api/files?q=1', '/tunnel?x=1', '/other']) {
      let caught = null;
      try {
        await transport.coreRequest('GET', bad, {});
      } catch (e) { caught = e; }
      assert.ok(caught && caught.code === 'E_INTERNAL', 'pathWithQuery=' + bad + ' 应抛 E_INTERNAL: ' + JSON.stringify(caught));
    }
    assert.strictEqual(fetched, false, '守卫应在发起 fetch 前拦截');
  } finally {
    globalThis.fetch = origFetch;
  }
});

// ---- setup helpers（供上方用例使用） ----
// localStorage override 状态：模块内单一可注入的「当前 override 值」，供
// readLocalOverride 读取；空串/未设置 = 无覆盖（试用真实现，避免在 test runner
// 并发环境反复 defineProperty 全局对象互相踩踏）。
let __overrideValue = null;
function setLocalStorageValue(val) {
  __overrideValue = val === null ? null : val;
}

function __overrideGetItem(k) {
  if (k === transport.overrideKey() && __overrideValue) return JSON.stringify(__overrideValue);
  return null;
}

// 注入最小 localStorage 探针（Node 默认无此全局；config.readLocalOverride 在
// globalThis.localStorage 存在时调用其 getItem）。
// 注意：describe 级（文件级）已注入一次；此处重复注入会覆盖 readLocalOverride
// 成功路径测试的 restore-underlay？不会——本声明只在执行到该行时执行一次，
// 把 localStorage 设为 __overrideGetItem。
(function ensureTestLocalStorage() {
  const desc = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    writable: true,
    value: { getItem: __overrideGetItem },
  });
})();
const PREV_LOCALSTORAGE_GET_ITEM = null;

function decodeText(u8) {
  if (u8 == null) return null;
  if (typeof u8 === 'string') return u8;
  if (u8 instanceof ArrayBuffer) u8 = new Uint8Array(u8);
  if (u8.byteLength !== undefined) return new TextDecoder().decode(u8);
  return String(u8);
}

const AK = 'sk-meshA-1234567890abcdef';
const SK = '2b40d5b60e6792134f07b44b46e2e19fb72f967136868015cb922d720c1aa6f5';
// 与 transport.js 内部 AAD 常量相同的上下文标签（Go AADMeta/AADStream）。
const AAD_META = 'tunnel:meta:v1';
const AAD_STREAM = 'tunnel:stream:v1';

// 预生成隧道响应帧（用 deriveTunnelKey 派生密钥加密 meta+body，模拟服务端/网关）。
// 注意：Promise 求值时 transport.accessKeyMesh / cryptoLib.deriveTunnelKey 均为纯函数，
// 与下方状态的 configure 无关。
const fakeTunnelBytes = (async () => {
  const derivedKeyHex = cryptoLib.bytesToHex(
    await cryptoLib.deriveTunnelKey(SK, transport.accessKeyMesh(AK))
  );
  const metaJSON = JSON.stringify({ proto: 'HTTP/1.1', status: 200, headers: { 'content-type': 'application/json' }, content_length: -1 });
  const metaFrame = await encryptFrameAAD(derivedKeyHex, metaJSON, AAD_META);
  const bodyFrame = await encryptFrameAAD(derivedKeyHex, 'HTTP body payload from tunnel', AAD_STREAM);
  return concatBytes(metaFrame, bodyFrame);
})();

// ==================== 领域 API（任务 5：files/cloud/share/config/hub） ====================
// 用注入 ctx 的 createApi 测试：mock coreRequest 捕获 (method, path, opts)，
// 断言各领域方法的 (method, path) 映射与 JSON/multipart 编解码正确。

const apiIndex = require('./api/index.js');
const apiUtil = require('./util.js');

function makeMockCore(results) {
  const calls = [];
  const fn = async (method, path, opts) => {
    calls.push({ method, path, opts: opts || {} });
    const r = results && results.length ? results.shift() : { status: 200, headers: {}, body: new Uint8Array(0) };
    if (r && r._throw) throw (r.err instanceof Error ? r.err : new Error(String(r.err)));
    return r;
  };
  fn.calls = calls;
  return fn;
}

function jsonBody(body) {
  return body == null ? {} : JSON.parse(new TextDecoder().decode(body));
}

function mockCtx(core, overrides) {
  return Object.assign({
    coreRequest: core,
    config: { chunkThreshold: 8 * 1024 * 1024 },
    log: undefined,
    crypto: cryptoLib,
    util: apiUtil,
  }, overrides || {});
}

function makeApi(core, overrides) {
  return apiIndex.createApi(mockCtx(core, overrides));
}

function okResp(obj) {
  return { status: 200, headers: {}, body: new TextEncoder().encode(JSON.stringify(obj)) };
}

// ---- files：列表/搜索/stat/download/delete/rename/mkdir/rmdir 映射 ----
test('files.list 映射 GET /api/files（subdir/offset/limit 参数）', async () => {
  const core = makeMockCore([okResp({ files: [{ name: 'a.txt', size: 3, checksum: 'c' }], total: 1 })]);
  const api = makeApi(core);
  const d = await api.files.list('dir/sub', { offset: 0, limit: 500 });
  assert.strictEqual(core.calls[0].method, 'GET');
  assert.strictEqual(core.calls[0].path, '/api/files?subdir=dir%2Fsub&offset=0&limit=500');
  assert.strictEqual(d.files.length, 1);
  assert.strictEqual(d.total, 1);
});

test('files.search / stat / download 映射', async () => {
  const core = makeMockCore([
    okResp({ files: [], total: 0 }),
    { status: 200, headers: { 'X-File-Size': '3' }, body: new Uint8Array(0) },
    { status: 200, headers: { 'content-type': 'application/octet-stream' }, body: new TextEncoder().encode('abc') },
  ]);
  const api = makeApi(core);
  await api.files.search('q1');
  assert.strictEqual(core.calls[0].path, '/api/files/search?q=q1');
  assert.strictEqual(core.calls[0].method, 'GET');
  await api.files.stat('x/y.txt');
  assert.strictEqual(core.calls[1].method, 'HEAD');
  assert.strictEqual(core.calls[1].path, '/api/files/stat?filename=x%2Fy.txt');
  const blob = await api.files.download('x/y.txt');
  assert.strictEqual(core.calls[2].method, 'GET');
  assert.strictEqual(core.calls[2].path, '/download?filename=x%2Fy.txt');
  assert.strictEqual(core.calls[2].opts.download, true);
  assert.ok(blob instanceof Blob);
  assert.strictEqual(new TextDecoder().decode(await blob.arrayBuffer()), 'abc');
});

test('files deleteFile/rename 带 X-File-Checksum 头映射', async () => {
  const core = makeMockCore([okResp({ success: true }), okResp({ success: true })]);
  const api = makeApi(core);
  await api.files.deleteFile('a.txt', 'abc123');
  assert.strictEqual(core.calls[0].method, 'POST');
  assert.strictEqual(core.calls[0].path, '/delete?filename=a.txt');
  assert.strictEqual(core.calls[0].opts.headers['X-File-Checksum'], 'abc123');
  await api.files.rename('old.txt', 'new.txt', 'cafebeef');
  assert.strictEqual(core.calls[1].method, 'POST');
  assert.strictEqual(core.calls[1].path, '/rename?from=old.txt&to=new.txt');
  assert.strictEqual(core.calls[1].opts.headers['X-File-Checksum'], 'cafebeef');
});

test('files mkdir/rmdir 映射', async () => {
  const core = makeMockCore([okResp({ success: true }), okResp({ success: true })]);
  const api = makeApi(core);
  await api.files.mkdir('a b');
  assert.strictEqual(core.calls[0].method, 'POST');
  assert.strictEqual(core.calls[0].path, '/mkdir?dirname=a%20b');
  await api.files.rmdir('d');
  assert.strictEqual(core.calls[1].path, '/rmdir?dirname=d');
});

test('files batchDelete/batchRename JSON body 编解码', async () => {
  const core = makeMockCore([okResp({ results: [{ filename: 'a', success: true }] }), okResp({ results: [] })]);
  const api = makeApi(core);
  await api.files.batchDelete([{ filename: 'a.txt', checksum: 'cc' }]);
  assert.strictEqual(core.calls[0].path, '/api/batch/delete');
  assert.deepStrictEqual(jsonBody(core.calls[0].opts.bodyBytes), { files: [{ filename: 'a.txt', checksum: 'cc' }] });
  assert.strictEqual(core.calls[0].opts.headers['Content-Type'], 'application/json');
  await api.files.batchRename([{ from: 'a', to: 'b', checksum: 'c' }]);
  assert.strictEqual(core.calls[1].path, '/api/batch/rename');
  assert.deepStrictEqual(jsonBody(core.calls[1].opts.bodyBytes), { operations: [{ from: 'a', to: 'b', checksum: 'c' }] });
});

test('files archive/archiveDir 返回 blob + Content-Disposition 文件名', async () => {
  const core = makeMockCore([
    { status: 200, headers: { 'Content-Disposition': 'attachment; filename="pkg.tar.gz"' }, body: new TextEncoder().encode('gzipbytes') },
    { status: 200, headers: {}, body: new TextEncoder().encode('more') },
  ]);
  const api = makeApi(core);
  const a1 = await api.files.archive(['f1']);
  assert.strictEqual(core.calls[0].method, 'POST');
  assert.strictEqual(core.calls[0].path, '/api/archive');
  assert.deepStrictEqual(jsonBody(core.calls[0].opts.bodyBytes), { files: ['f1'] });
  assert.strictEqual(core.calls[0].opts.download, true);
  assert.strictEqual(a1.filename, 'pkg.tar.gz');
  assert.ok(a1.blob instanceof Blob);
  const a2 = await api.files.archiveDir('d');
  assert.strictEqual(core.calls[1].path, '/api/archive-dir?dirname=d');
  assert.strictEqual(core.calls[1].opts.download, true);
  assert.strictEqual(a2.filename, 'archive.tar.gz');
});

test('files versions.list/restore/delete 映射', async () => {
  const core = makeMockCore([
    okResp({ versions: [{ version_id: 1, size: 2 }] }),
    okResp({ success: true }),
    okResp({ success: true }),
  ]);
  const api = makeApi(core);
  const v = await api.files.versions.list('f.txt');
  assert.strictEqual(core.calls[0].path, '/api/versions?filename=f.txt');
  assert.strictEqual(core.calls[0].method, 'GET');
  assert.strictEqual(v.versions.length, 1);
  await api.files.versions.restore('f.txt', '7');
  assert.strictEqual(core.calls[1].path, '/api/versions/restore?filename=f.txt&version_id=7');
  assert.strictEqual(core.calls[1].method, 'POST');
  await api.files.versions.delete('f.txt', '7');
  assert.strictEqual(core.calls[2].path, '/api/versions?filename=f.txt&version_id=7');
  assert.strictEqual(core.calls[2].method, 'DELETE');
});

// ---- files.upload：小文件简单上传（multipart + 头）与分块上传（init/chunk/complete）----

test('files.upload 小文件走简单 POST /upload（multipart + X-File-Checksum/Path/MTime）', async () => {
  const core = makeMockCore([okResp({ success: true, file_checksum: 'abc' })]);
  const api = makeApi(core, { config: { chunkThreshold: 1 << 20 } });
  const smallTxt = new TextEncoder().encode('abcd');
  const file = {
    name: 'small.txt',
    size: 4,
    lastModified: 1700000000000,
    slice: (s, e) => { const b = smallTxt.slice(s, e); return { arrayBuffer: async () => b.slice().buffer }; },
    arrayBuffer: async () => smallTxt.slice().buffer,
  };
  const res = await api.files.upload(file, { subdir: '', onProgress: undefined });
  assert.strictEqual(core.calls.length, 1);
  assert.strictEqual(core.calls[0].method, 'POST');
  assert.strictEqual(core.calls[0].path, '/upload');
  assert.strictEqual(core.calls[0].opts.headers['X-File-Path'], 'small.txt');
  assert.strictEqual(core.calls[0].opts.headers['X-File-MTime'], String(1700000000000 * 1e6));
  const chk = core.calls[0].opts.headers['X-File-Checksum'];
  assert.ok(/^[0-9a-f]{64}$/.test(chk), 'checksum 应为 64 hex: ' + chk);
  assert.strictEqual(chk, await cryptoLib.sha256Hex('abcd'));
  assert.ok(core.calls[0].opts.headers['Content-Type'].indexOf('multipart/form-data; boundary=') === 0);
  assert.ok(core.calls[0].opts.bodyBytes instanceof Uint8Array);
  assert.ok(core.calls[0].opts.bodyBytes.length > 0);
  assert.ok(res.success);
});

test('files.upload 分块流程 init→status→chunk→complete（forceChunked 强制）', async () => {
  const core = makeMockCore([
    okResp({ success: true, upload_id: 'u123', chunk_size: 4, message: 'ok' }),
    okResp({ success: true, missing_chunks: [0], upload_id: 'u123' }),
    okResp({ success: true, message: 'ok' }),
    okResp({ success: true, filename: 'big.bin', file_checksum: 'ff' }),
  ]);
  const api = makeApi(core);
  const content = new Uint8Array(16);
  for (let i = 0; i < 16; i++) content[i] = i;
  const overall = content.slice();
  const file = {
    name: 'big.bin',
    size: 16,
    lastModified: 1700000000000,
    slice: (s, e) => { const b = overall.slice(s, e); return { arrayBuffer: async () => b.slice().buffer }; },
    arrayBuffer: async () => overall.slice().buffer,
  };
  const res = await api.files.upload(file, { subdir: 's', forceChunked: true });
  assert.strictEqual(core.calls.length, 4);
  assert.strictEqual(core.calls[0].path, '/upload/init');
  assert.strictEqual(core.calls[1].path, '/upload/status?upload_id=u123');
  assert.strictEqual(core.calls[2].path, '/upload/chunk');
  assert.strictEqual(core.calls[2].opts.headers['Content-Type'].indexOf('multipart/form-data; boundary=') === 0, true);
  assert.strictEqual(core.calls[3].path, '/upload/complete');
  assert.strictEqual(res.success, true);
});

// ---- cloud：任务/组 ----
test('cloud 任务映射（create/batch/list/get/cancel/delete/resume/archive）', async () => {
  const core = makeMockCore([
    okResp({ id: 't1', status: 'pending' }),
    okResp({ tasks: [{ id: 't2', status: 'completed' }] }),
    okResp({ tasks: [], total: 0 }),
    okResp({ id: 't3', status: 'downloading' }),
    okResp({ status: 'cancelled' }),
    okResp({ status: 'deleted' }),
    okResp({ status: 'resumed' }),
    okResp({ success: true, file: '.__cloud_archives__/x.tar.gz' }),
    okResp({ success: true, file: 'b.tar.gz' }),
  ]);
  const api = makeApi(core);
  await api.cloud.createDownload('http://x/a', 'a.jpg');
  assert.strictEqual(core.calls[0].path, '/api/cloud/download');
  assert.deepStrictEqual(jsonBody(core.calls[0].opts.bodyBytes), { url: 'http://x/a', filename: 'a.jpg' });
  await api.cloud.createBatch([{ url: 'http://x/b', filename: 'b.jpg' }]);
  assert.strictEqual(core.calls[1].path, '/api/cloud/download/batch');
  assert.deepStrictEqual(jsonBody(core.calls[1].opts.bodyBytes), { urls: [{ url: 'http://x/b', filename: 'b.jpg' }] });
  await api.cloud.listTasks({ status: 'downloading' });
  assert.strictEqual(core.calls[2].path, '/api/cloud/tasks?status=downloading');
  assert.strictEqual(core.calls[2].method, 'GET');
  await api.cloud.getTask('t3');
  assert.strictEqual(core.calls[3].path, '/api/cloud/tasks/t3');
  await api.cloud.cancelTask('t4');
  assert.strictEqual(core.calls[4].path, '/api/cloud/tasks/t4/cancel');
  assert.strictEqual(core.calls[4].method, 'POST');
  await api.cloud.deleteTask('t5');
  assert.strictEqual(core.calls[5].path, '/api/cloud/tasks/t5');
  assert.strictEqual(core.calls[5].method, 'DELETE');
  await api.cloud.resumeTask('t6', true);
  assert.strictEqual(core.calls[6].path, '/api/cloud/tasks/t6/resume');
  assert.deepStrictEqual(jsonBody(core.calls[6].opts.bodyBytes), { force: true });
  await api.cloud.archiveTask('t7', 'x.tar.gz');
  assert.strictEqual(core.calls[7].path, '/api/cloud/tasks/t7/archive');
  assert.deepStrictEqual(jsonBody(core.calls[7].opts.bodyBytes), { archive_name: 'x.tar.gz' });
  await api.cloud.archiveBatch(['a', 'b']);
  assert.strictEqual(core.calls[8].path, '/api/cloud/archive');
  assert.deepStrictEqual(jsonBody(core.calls[8].opts.bodyBytes), { task_ids: ['a', 'b'] });
});

test('cloud 组映射（create/list/get/cancel/delete/resume/archive）', async () => {
  const core = makeMockCore([
    okResp({ id: 'g1', total_tasks: 1 }),
    okResp({ groups: [], total: 0 }),
    okResp({ group: { id: 'g1' }, tasks: [] }),
    okResp({ status: 'cancelled' }),
    okResp({ status: 'deleted' }),
    okResp({ status: 'resumed' }),
    okResp({ success: true, file: 'g.tar.gz' }),
  ]);
  const api = makeApi(core);
  await api.cloud.createGroup('grp', [{ url: 'http://x/a', filename: 'a.jpg' }]);
  assert.strictEqual(core.calls[0].path, '/api/cloud/groups');
  assert.deepStrictEqual(jsonBody(core.calls[0].opts.bodyBytes), { name: 'grp', urls: [{ url: 'http://x/a', filename: 'a.jpg' }] });
  await api.cloud.listGroups({});
  assert.strictEqual(core.calls[1].path, '/api/cloud/groups');
  await api.cloud.getGroup('g1');
  assert.strictEqual(core.calls[2].path, '/api/cloud/groups/g1');
  await api.cloud.cancelGroup('g1');
  assert.strictEqual(core.calls[3].path, '/api/cloud/groups/g1/cancel');
  await api.cloud.deleteGroup('g1');
  assert.strictEqual(core.calls[4].path, '/api/cloud/groups/g1');
  assert.strictEqual(core.calls[4].method, 'DELETE');
  await api.cloud.resumeGroup('g1', false);
  assert.strictEqual(core.calls[5].path, '/api/cloud/groups/g1/resume');
  assert.deepStrictEqual(jsonBody(core.calls[5].opts.bodyBytes), { force: false });
  await api.cloud.archiveGroup('g1', 'g.tar.gz');
  assert.strictEqual(core.calls[6].path, '/api/cloud/groups/g1/archive');
  assert.deepStrictEqual(jsonBody(core.calls[6].opts.bodyBytes), { archive_name: 'g.tar.gz' });
});

// ---- share ----
test('share create/list/revoke 映射', async () => {
  const core = makeMockCore([
    okResp({ success: true, token: 'T' }),
    okResp({ success: true, token: 'T' }),
    okResp({ shares: [{ token: 'T', filename: 'a.txt' }] }),
    okResp({ success: true }),
  ]);
  const api = makeApi(core);
  await api.share.create({ filename: 'a.txt', ttl: '24h', max_downloads: 3, one_time: false });
  assert.strictEqual(core.calls[0].path, '/api/share');
  assert.strictEqual(core.calls[0].method, 'POST');
  assert.deepStrictEqual(jsonBody(core.calls[0].opts.bodyBytes), { filename: 'a.txt', ttl: '24h', max_downloads: 3, one_time: false });
  const noBody = await api.share.create({ filename: 'a.txt' });
  assert.strictEqual(noBody.token, 'T');
  assert.deepStrictEqual(jsonBody(core.calls[1].opts.bodyBytes), { filename: 'a.txt', ttl: '24h', max_downloads: 0, one_time: false });
  const list = await api.share.list();
  assert.strictEqual(core.calls[2].path, '/api/shares');
  assert.strictEqual(list.shares.length, 1);
  await api.share.revoke('T');
  assert.strictEqual(core.calls[3].path, '/api/shares/T');
  assert.strictEqual(core.calls[3].method, 'DELETE');
});

// ---- config ----
test('config.get / update / updateStorage 映射', async () => {
  const core = makeMockCore([
    okResp({ log_level: 'info', web_tunnel: true }),
    okResp({ success: true, changed: true }),
    okResp({ success: true, max_storage_bytes: 1024 }),
  ]);
  const api = makeApi(core);
  const g = await api.config.get();
  assert.strictEqual(core.calls[0].path, '/api/config');
  assert.strictEqual(core.calls[0].method, 'GET');
  assert.strictEqual(g.web_tunnel, true);
  await api.config.update({ log_level: 'debug', web_tunnel: false });
  assert.strictEqual(core.calls[1].path, '/api/config');
  assert.strictEqual(core.calls[1].method, 'PUT');
  assert.deepStrictEqual(jsonBody(core.calls[1].opts.bodyBytes), { log_level: 'debug', web_tunnel: false });
  await api.config.updateStorage(2048);

  assert.strictEqual(core.calls[2].path, '/api/storage/config');
  assert.strictEqual(core.calls[2].method, 'PUT');
  assert.deepStrictEqual(jsonBody(core.calls[2].opts.bodyBytes), { max_storage_bytes: 2048 });
});

// ---- hub ----
test('hub nodes/stats/remove 映射（nodes 包装为 {nodes:[...]}）', async () => {
  const core = makeMockCore([
    // nodes 返回裸数组（服务端 json array）；领域把它统一为 {nodes, ...}
    { status: 200, headers: {}, body: new TextEncoder().encode(JSON.stringify([{ id: 'n1', addr: '127.0.0.1:1', connected: '2026-01-01T00:00:00Z' }])) },
    okResp({ nodes_connected: 3 }),
    okResp({ status: 'removed', node: 'n1' }),
  ]);
  const api = makeApi(core);
  const n = await api.hub.nodes();
  assert.strictEqual(core.calls[0].path, '/api/hub/nodes');
  assert.strictEqual(core.calls[0].method, 'GET');
  assert.ok(Array.isArray(n.nodes), 'nodes 应被包装为数组');
  assert.strictEqual(n.nodes[0].id, 'n1');
  const s = await api.hub.stats();
  assert.strictEqual(core.calls[1].path, '/api/hub/stats');
  assert.strictEqual(core.calls[1].method, 'GET');
  assert.strictEqual(s.nodes_connected, 3);
  await api.hub.remove('n1');
  assert.strictEqual(core.calls[2].path, '/api/hub/nodes/n1');
  assert.strictEqual(core.calls[2].method, 'DELETE');
});

test('util.buildMultipart 片段断言（boundary/字段/文件存在性）', () => {
  const mp = apiUtil.buildMultipart(
    { a: '1', upload_id: 'u', chunk_checksum: 'f'.repeat(64) },
    { name: 'file', filename: 'name.txt', contentType: 'text/plain', bytes: new TextEncoder().encode('hello') }
  );
  const text = new TextDecoder().decode(mp.body);
  const b = mp.contentType.split('boundary=')[1];
  assert.ok(b, 'contentType 应含 boundary');
  assert.ok(mp.body instanceof Uint8Array);
  assert.ok(text.indexOf('name="a"') >= 0, '普通字段存在');
  assert.ok(text.indexOf('name="file"; filename="name.txt"') >= 0, '文件字段存在');
  assert.ok(text.indexOf('[object Uint8Array]') < 0, '文件内容应为原始字节非字符串化');
  assert.ok(text.indexOf('hello' + String.fromCharCode(13) + String.fromCharCode(10)) >= 0, '文件字节与 CRLF 相邻');
});
