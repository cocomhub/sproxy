/* SPDX-License-Identifier: Apache-2.0 */
/* global module, self */
/*
 * log.js —— sclient 前端库的范围内 logger。
 *
 * 简易 logger：level debug/info/warn/error 四级，默认写入 console，
 * 实现可用 log.setConsole 替换（测试注入友好）。浏览器暴露全局
 * sclientLog，Node 中 module.exports 导出。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.sclientLog = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  const LEVELS = ['debug', 'info', 'warn', 'error'];
  const LEVELS_IDX = { debug: 0, info: 1, warn: 2, error: 3 };

  // 当前生效级别（默认 info）。
  let currentLevel = 'info';
  // 输出目标（默认 console）。可替换（如测试注入）。
  let output = typeof console !== 'undefined' ? console : null;
  // 附加前缀（可选，如模块名），便于区分来自哪个子模块。
  let prefix = '';

  // 设置全局级别：debug/info/warn/error。返回旧级别。
  function setLevel(level) {
    const old = currentLevel;
    if (LEVELS.indexOf(level) >= 0) currentLevel = level;
    return old;
  }

  // 设置/替换输出实现。返回旧实现。
  // output 需提供 debug/info/warn/error 四个方法。
  function setConsole(impl) {
    const old = output;
    output = impl;
    return old;
  }

  // 设置日志前缀（如 "[sclient] "）。返回旧前缀。
  function setPrefix(p) {
    const old = prefix;
    prefix = p || '';
    return old;
  }

  function emit(level, args) {
    const target = output;
    if (!target || typeof target[level] !== 'function') return;
    // 预留扩展：未来可在此做格式化/时序打印；当前直接透传。
    target[level].apply(target, [prefix + args[0]].concat(Array.prototype.slice.call(args, 1)));
  }

  function log(level, args) {
    if (LEVELS_IDX[currentLevel] > LEVELS_IDX[level]) return; // 级别过滤
    emit(level, args);
  }

  return {
    debug() { log('debug', arguments); },
    info() { log('info', arguments); },
    warn() { log('warn', arguments); },
    error() { log('error', arguments); },
    setLevel,
    setConsole,
    setPrefix,
  };
});
