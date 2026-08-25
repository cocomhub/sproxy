/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * config.js —— sclient 前端库的默认配置与 override 辅助。
 *
 * 默认配置 + localStorage override（应用户偏好/诊断开关，不持久化密钥）。
 * 浏览器暴露全局 sclientConfig，Node 中 module.exports 导出。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.sclientConfig = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  // transport 合法取值：auto | direct
  const TRANSPORT_VALUES = ['auto', 'direct'];

  // 返回一份默认配置对象（每次调用独立拷贝，避免调用方修改共享单例）。
  function defaultConfig() {
    return {
      // 服务端基址，空串表示与当前页面同源。
      baseUrl: '',
      // SproxySig 认证（可选）：accessKey / accessKeySecret 只在请求签名时使用，
      // Secret 永不上线。
      accessKey: '',
      accessKeySecret: '',
      // 传输模式：auto（自动协商）/ direct（强制直连，穿透 SproxySig 校验时用于测试）。
      transport: 'auto',
      // 隧道是否默认启用（preferTunnel 语义）。
      tunnelDefault: true,
      // localStorage 传输覆盖键名。
      overrideKey: 'sproxy_web_transport_override',
      // 分块阈值（字节）：大请求走分块上传；默认 8 MiB，仅作占位校准值。
      chunkThreshold: 8 * 1024 * 1024,
    };
  }

  // 用 patch 覆盖默认配置。只接受已定义键（白名单），未知/非法值跳过。
  // 返回覆盖后的副本（不修改入参）。
  function applyOverride(patch) {
    const cfg = defaultConfig();
    if (!patch || typeof patch !== 'object') return cfg;
    const allowed = Object.keys(defaultConfig());
    for (const key of allowed) {
      const val = patch[key];
      if (val === undefined || val === null) continue;
      if (key === 'transport') {
        if (TRANSPORT_VALUES.indexOf(val) >= 0) cfg.transport = val;
        continue;
      }
      cfg[key] = val;
    }
    return cfg;
  }

  // 从 localStorage 读取传输 override（若存储可用），返回：《当前生效 override
  // 对象, overrideKey》。存储不可用或值为空时返回默认值 + overrideKey。
  // 只在浏览器沙箱有 localStorage 时做 try/catch（隐私窗口/被禁站点）。
  function readLocalOverride() {
    const cfg = defaultConfig();
    const overrideKey = cfg.overrideKey;
    let value = null;
    try {
      // 运行时再解析 localStorage（含 node 单测环境）——避免加载期直接引用未定义全局报错
      value = globalThis.localStorage && globalThis.localStorage.getItem(overrideKey);
    } catch (e) {
      value = null; // localStorage 不可用（隐私模式/禁用/无全局）→ 回退默认
    }
    if (value) {
      try {
        const parsed = JSON.parse(value);
        if (parsed && typeof parsed === 'object') {
          const applied = applyOverride(parsed);
          return { transport: applied.transport, overrideKey, applied };
        }
      } catch (e) {
        // 解析失败忽略，回退默认
      }
    }
    return { transport: cfg.transport, overrideKey, applied: cfg };
  }

  return {
    defaultConfig,
    applyOverride,
    readLocalOverride,
    TRANSPORT_VALUES,
  };
});
