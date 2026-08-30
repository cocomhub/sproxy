/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * index.js —— sclient 领域 API 命名空间组装。
 *
 * createApi(ctx) → { files, cloud, share, config, hub, sync }。
 *
 * ctx 由库入口注入（浏览器经全局 sclientTransport/sclientConfig/sclientLog/
 * sclientCrypto/sclientUtil 组装后逐命名空间分发）：
 *   { coreRequest: sclientTransport.coreRequest,
 *     config:      sclientConfig.readLocalOverride().applied 或注入配置,
 *     log:         sclientLog,
 *     crypto:      sclientCrypto,
 *     util:        sclientUtil }
 *
 * 各 api/* 模块为「闭包式工厂」(ctx) => ({...方法})——本文件调用并合并，
 * 不向外暴露 coreRequest 本身。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(
      require('./files.js'),
      require('./cloud.js'),
      require('./share.js'),
      require('./config.js'),
      require('./hub.js'),
      require('./sync.js')
    );
  } else {
    var fn = factory(
      root.sclientApiFiles,
      root.sclientApiCloud,
      root.sclientApiShare,
      root.sclientApiConfig,
      root.sclientApiHub,
      root.sclientApiSync
    );
    root.sclientApi = fn;
    if (typeof fn._bindBrowser === 'function') fn._bindBrowser();
  }
})(typeof self !== 'undefined' ? self : this, function (createFilesApi, createCloudApi, createShareApi, createConfigApi, createHubApi, createSyncApi) {
  'use strict';

  // 组装 ctx 的默认实现（浏览器路径）：从各全局取传输核心 + 配置 + 日志。
  // 阶段方缺（缺哪个）时对应命名空间创建失败并抛错——由入口 catch 并提示。
  // 注意：IIFE 的第一个参数 root（浏览器全局）不是本工厂函数的参数——直接引用 root
  // 会 Uncaught ReferenceError；需新建局部 g 指向浏览器全局对象（Node 侧走
  // module.exports 分支，不经过这里，故 globalThis 回退只是防御）。
  const g = (typeof self !== 'undefined' ? self : globalThis);
  function apiFromGlobals() {
    const transport = g.sclientTransport;
    const config = g.sclientConfig;
    const log = g.sclientLog;
    const crypto = g.sclientCrypto;
    const util = g.sclientUtil;
    if (!transport) throw new Error('sclient: 缺少 transport 全局（sclientTransport）');
    if (!util) throw new Error('sclient: 缺少 util 全局（sclientUtil）');
    // config 默认：readLocalOverride().applied（含 override 合并）；
    // 若用户主动 configure 过（注入配置）则优先存命名的全局无法取得——
    // 这里以 readLocalOverride().applied 为准（含 chunkThreshold 等默认）。
    const cfg = config ? config.readLocalOverride().applied : undefined;
    const ctx = {
      coreRequest: transport.coreRequest,
      config: cfg || {},
      log: log || undefined,
      crypto: crypto || undefined,
      util: util,
    };
    return createApi(ctx);
  }

  // createApi：注入式组装（测试/库入口用）。
  function createApi(ctx) {
    return {
      files: createFilesApi(ctx),
      cloud: createCloudApi(ctx),
      share: createShareApi(ctx),
      config: createConfigApi(ctx),
      hub: createHubApi(ctx),
      sync: createSyncApi(ctx),
    };
  }

  return {
    createApi,
    apiFromGlobals,
    // UMD 浏览器分支绑定 host：浏览器加载时把 createApi/apiFromGlobals 暴露为
    // self.sclientApi，使 app.js 的 sclientApi.apiFromGlobals() 可用（Node 走上面
    // module.exports 分支，不受影响）。
    _bindBrowser: function () {
      const h = typeof self !== 'undefined' ? self : globalThis;
      h.sclientApi = { createApi, apiFromGlobals };
    },
  };
});
