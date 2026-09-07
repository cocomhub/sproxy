/* SPDX-License-Identifier: Apache-2.0 */
/*
 * login.test.js —— Web 注册/登录页（login.js）功能测试。
 *
 * 运行方式：node --test web/static/login.test.js
 * 用 node:test + assert/strict。
 *
 * 覆盖（简报步骤 1）：
 *   - 注册表单提交 → RegisterTOTP 被调用（path /api/credentials/register、body owner）；
 *   - 注册响应渲染：base32_secret 文本、otpauth_uri 链接、admin:true 提示、QR 矩阵生成；
 *   - 登录表单 → 先 RequestTOTPNonce 后 LoginTOTP（顺序、login_type='web'、ak 正确）；
 *   - 登录成功 sessionStorage 三键回填 + sproxy_last_ak 写入；
 *   - 解密成功路径：真实 TOTP wrap 信封固定向量（Go 实测）→ 解出 session SK hex 回填；
 *   - app.js sproxy_access_key_id 存取接线（结构性断言）。
 *
 * 跨文件全局纪律：login.js 依赖 app.js 的 showToast/sclientTransport 等服务端可注入，
 * 测试用注入桩（globalThis 上挂最小 showToast + sclientTransport.configure 捕获）。
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

// ---- Node 下需要的浏览器全局探针（atob/base64/sessionStorage） ----
function b64ToBytes(b64) {
  if (typeof Buffer !== 'undefined') return Buffer.from(b64, 'base64');
  return Uint8Array.from(Buffer.from(b64, 'base64'));
}

// 最小 sessionStorage stub（Node 无默认）：setItem 记录进 map，getItem 读取。
function makeSessionStorage(seed) {
  const map = new Map(Object.entries(seed || {}));
  return {
    _map: map,
    getItem(k) { return map.has(k) ? map.get(k) : null; },
    setItem(k, v) { map.set(k, String(v)); },
    removeItem(k) { map.delete(k); },
  };
}

// 捕获式的 transport.configure 桩（不真正发请求）——login.js 依赖的 sclientTransport。
function makeTransport() {
  const calls = [];
  return {
    calls,
    configure(opts) { calls.push(opts || {}); },
  };
}

// 每次测试独立的 app 全局注入（showToast / sclientTransport / sessionStorage），
// finally 恢复，保持用例互不污染。注意：必须 await fn() 使 async 流程完成后再恢复
// 全局——否则恢复先于登录流程内部的 sessionStorage 写入（历史修复）。
async function withLoginGlobals({ transport, session } = {}, fn) {
  const prev = {
    sessionStorage: Object.getOwnPropertyDescriptor(globalThis, 'sessionStorage'),
    sclientTransport: globalThis.sclientTransport,
    showToast: globalThis.showToast,
  };
  try {
    Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, writable: true, value: session });
    globalThis.sclientTransport = transport;
    globalThis.showToast = () => {};
    return await fn();
  } finally {
    globalThis.showToast = prev.showToast;
    globalThis.sclientTransport = prev.sclientTransport;
    if (prev.sessionStorage) Object.defineProperty(globalThis, 'sessionStorage', prev.sessionStorage);
    else delete globalThis.sessionStorage;
  }
}

// ---- RegisterTOTP / RequestTOTPNonce / LoginTOTP / 流控入口（login.js 导出的领域 API） ----
const loginLib = require(path.join(__dirname, 'login.js'));

// okResp：构造领域层消费的 coreRequest 响应（json body）。
function okResp(obj) {
  return { status: 200, headers: {}, body: new TextEncoder().encode(JSON.stringify(obj)) };
}

function makeCore(calls, results) {
  const fn = async (method, p, opts) => {
    calls.push({ method, p, opts: opts || {} });
    const r = (results && results.length) ? results.shift() : okResp({});
    if (r._throw) throw new Error(String(r.err));
    return r;
  };
  fn.calls = calls;
  return fn;
}

// 固定 TOTP wrap 信封向量（Go <DeriveTOTPWrapKey + EncryptSecretKind> 实测，简报注入值）。
const WRAP_FIXTURE = {
  code: '123456',
  ak: 'ak-test-00112233445566778899aabbccddeeff',
  nonce: 'abcd1234ef56789012abcdef',
  sessionSKHex: '000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f',
  envelope: {
    kind: 'totp_wrap',
    wrap_key_id: 'ak-test-00112233445566778899aabbccddeeff',
    nonce: 'REoIv33rrZwMMbMi',
    ciphertext: 'JCVvN5UyNALPAzektgYIUKPEmz37uWCDKeuNa2yGSyLMxQ3Bkzru4ohwF6DOCJQt',
  },
};

test('login 注册表单提交 → RegisterTOTP（POST /api/credentials/register, body owner）', async () => {
  const core = makeCore([], [okResp({
    ak: 'ak-new-abcdef', owner: 'alice', admin: true, otpauth_uri: 'otpauth://totp/alice?secret=JBSWY3DPEHPK3PXP', base32_secret: 'JBSWY3DPEHPK3PXP',
  })]);
  const out = await loginLib.registerTOTPByCore({ core }, 'alice');
  assert.strictEqual(out.ak, 'ak-new-abcdef');
  assert.strictEqual(core.calls[0].method, 'POST');
  assert.strictEqual(core.calls[0].p, '/api/credentials/register');
  const body = JSON.parse(new TextDecoder().decode(core.calls[0].opts.bodyBytes));
  assert.deepStrictEqual(body, { owner: 'alice' });
});

test('登录流程 → 先 RequestTOTPNonce 后 LoginTOTP（顺序、login_type=web、ak 正确）', async () => {
  const core = makeCore([], [
    okResp({ nonce: WRAP_FIXTURE.nonce, expires_at: '2026-09-07T00:00:00Z' }),
    okResp({
      ak: WRAP_FIXTURE.ak,
      session_skey_id: 'sk-abc123',
      session_expires_at: '2026-09-08T00:00:00Z',
      wrapped_session_secret: WRAP_FIXTURE.envelope,
    }),
  ]);
  const out = await loginLib.loginTOTPByCore({ core }, WRAP_FIXTURE.ak, WRAP_FIXTURE.code, 'web');
  assert.strictEqual(core.calls.length, 2, '应恰好两次请求（nonce 再 login）');
  assert.strictEqual(core.calls[0].method, 'POST');
  assert.strictEqual(core.calls[0].p, '/api/credentials/nonce');
  assert.strictEqual(core.calls[1].method, 'POST');
  assert.strictEqual(core.calls[1].p, '/api/credentials/login');
  const body = JSON.parse(new TextDecoder().decode(core.calls[1].opts.bodyBytes));
  assert.deepStrictEqual(body, { ak: WRAP_FIXTURE.ak, nonce: WRAP_FIXTURE.nonce, code: WRAP_FIXTURE.code, login_type: 'web' });
  // 解出的 session SK hex（Go 对齐硬性验收）
  assert.strictEqual(out.sessionSKHex, WRAP_FIXTURE.sessionSKHex);
  assert.strictEqual(out.sessionSkeyID, 'sk-abc123');
});

test('登录成功 → sessionStorage 三键回填 + sproxy_last_ak 写入 + transport.configure 同步', async () => {
  const session = makeSessionStorage({ sproxy_access_key: '', sproxy_access_key_secret: '' });
  const transport = makeTransport();
  await withLoginGlobals({ session, transport }, async () => {
    const core = makeCore([], [
      okResp({ nonce: WRAP_FIXTURE.nonce, expires_at: '2026-09-07T00:00:00Z' }),
      okResp({
        ak: WRAP_FIXTURE.ak,
        session_skey_id: 'sk-abc123',
        session_expires_at: '2026-09-08T00:00:00Z',
        wrapped_session_secret: WRAP_FIXTURE.envelope,
      }),
    ]);
    await loginLib.loginTOTPByCore({ core }, WRAP_FIXTURE.ak, WRAP_FIXTURE.code, 'web');
  });
  assert.strictEqual(session.getItem('sproxy_access_key'), WRAP_FIXTURE.ak);
  assert.strictEqual(session.getItem('sproxy_access_key_secret'), WRAP_FIXTURE.sessionSKHex);
  assert.strictEqual(session.getItem('sproxy_access_key_id'), 'sk-abc123');
  assert.strictEqual(session.getItem('sproxy_last_ak'), WRAP_FIXTURE.ak, '最近 AK 记忆应写入 sproxy_last_ak');
  // transport.configure 至少一次携带 accessKeyID（登录回填后同步）
  const cfgCall = transport.calls.find((c) => c.accessKeyID === 'sk-abc123');
  assert.ok(cfgCall, '登录后 transport 应收到带 accessKeyID 的 configure');
});

test('注册响应渲染 → base32 文本 / otpauth_uri 链接 / admin 提示 / QR 矩阵生成', async () => {
  // 渲染纯函数先把字段映射成面板 HTML 字符串（不碰 DOM，Node 可直测）
  const html = loginLib.renderRegisterResultHtml({
    ak: 'ak-new-abcdef', owner: 'alice', admin: true,
    otpauth_uri: 'otpauth://totp/alice?secret=JBSWY3DPEHPK3PXP',
    base32_secret: 'JBSWY3DPEHPK3PXP',
  });
  assert.ok(html.includes('JBSWY3DPEHPK3PXP'), '应展示 base32_secret 文本');
  assert.ok(html.includes('otpauth://totp/alice?secret=JBSWY3DPEHPK3PXP'), '应给出 otpauth_uri（链接/复制）');
  assert.ok(html.includes('管理员'), 'admin=true 应有提示');
  assert.ok(html.includes('qr-register'), '结果区应有 QR 挂载点（客户端 JS QR 渲染）');
  // F8：非 otpauth:// scheme 不渲染为可点击链接（纯文本），防任意 href
  const bad = loginLib.renderRegisterResultHtml({ otpauth_uri: 'javascript:alert(1)' });
  assert.ok(!bad.includes('<a href='), '非 otpauth:// 不得输出 <a href=');
  assert.ok(bad.includes('javascript:alert(1)') && bad.includes('<span'), '非 otpauth:// 应输出纯文本 <span>');
});

test('QR 集成：renderQRInto 委托 sproxyQR 渲染挂载点（stub 注入断言 SVG）', async () => {
  // 注入最小 sproxyQR stub（renderToSVG 返回确定性 SVG），断言 el.innerHTML 被写入。
  const prev = globalThis.sproxyQR;
  globalThis.sproxyQR = {
    renderToSVG(uri) { return '<svg xmlns="http://www.w3.org/2000/svg"><rect width="21" height="21"/></svg>'; },
  };
  try {
    const el = { innerHTML: '' };
    await loginLib.renderQRInto(el, 'otpauth://totp/alice?secret=JBSWY3DPEHPK3PXP');
    assert.ok(el.innerHTML.startsWith('<svg'), '挂载点应被写成 SVG：' + el.innerHTML);
    assert.ok(el.innerHTML.includes('xmlns='), 'SVG 应含 xmlns');
  } finally {
    if (prev === undefined) delete globalThis.sproxyQR; else globalThis.sproxyQR = prev;
  }
});

test('QR 集成：renderQRInto 无 sproxyQR 时不抛错且挂载点不动', async () => {
  const prev = globalThis.sproxyQR;
  delete globalThis.sproxyQR;
  try {
    const el = { innerHTML: 'keep' };
    await loginLib.renderQRInto(el, 'otpauth://totp/x?secret=JBSWY3DPEHPK3PXP');
    assert.strictEqual(el.innerHTML, 'keep', '无 sproxyQR 时应静默保持原内容');
  } finally {
    if (prev !== undefined) globalThis.sproxyQR = prev;
  }
});

test('app.js 顶层读取并保存 sproxy_access_key_id（saveAccessKeys 同步写），登录页与之对接', () => {
  const appSrc = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  const loginSrc = fs.readFileSync(path.join(__dirname, 'login.js'), 'utf8');
  assert.ok(appSrc.includes("'sproxy_access_key_id'"), 'app.js 应读取/保存 sproxy_access_key_id 键');
  assert.ok(appSrc.includes("sproxy_access_key_secret'"), '旁证：既有 secret 键仍在');
  // 顶层读取该键（供 transport 注入 entryID/会话回填）
  assert.ok(/accessKeyID\s*=\s*sessionStorage\.getItem\(\s*['"]sproxy_access_key_id['"]\s*\)/.test(appSrc)
    || appSrc.includes("accessKeyID = sessionStorage.getItem('sproxy_access_key_id')"),
    'app.js 顶层应把 sproxy_access_key_id 读入内存');
  assert.ok(appSrc.includes('accessKeyID'), 'app.js 应在 saveAccessKeys / transport configure 中同步 accessKeyID');
});

test('login.js 跨文件隐式全局声明（// global 注释）+ 无重名 formatSize/escHtml', () => {
  const src = fs.readFileSync(path.join(__dirname, 'login.js'), 'utf8');
  const globals = src.match(/\/\/ global[^\n]*/g) || [];
  assert.ok(globals.length >= 1, 'login.js 顶部应有显式 // global 声明: ' + globals.join(';'));
  assert.ok(!/(?:^|\n)\s*(?:function|const|let)\s+formatSize\b/.test(src), '不得定义与 appRender 重名的 formatSize');
  assert.ok(!/(?:^|\n)\s*(?:function|const|let)\s+escHtml\b/.test(src), '不得定义与 appRender 重名的 escHtml');
});
