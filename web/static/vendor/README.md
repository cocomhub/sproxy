# vendor —— 第三方前端库

本目录存放第三方 JavaScript 库（不加项目 SPDX 头，由 Makefile `addlicense` 目标
`-ignore "web/static/vendor/**"` 保护）。

## qrcode-generator

| 项 | 值 |
|---|---|
| 库 | `qrcode-generator`（QR Code Generator for JavaScript） |
| 版本 | **1.4.4** |
| 来源 URL | https://cdnjs.cloudflare.com/ajax/libs/qrcode-generator/1.4.4/qrcode.js |
| 版权 | Copyright (c) 2009 Kazuhiko Arase（d-project.com），**MIT License**（文件首部含完整版权与许可证文本） |
| 用途 | 客户端 TOTP otpauth_uri 二维码生成（由 `web/static/qrcode.js` 包装层消费，`qrcode(0,'L')` 自动版本） |

## 升级 / 审计策略

- **升级**：核对新版本仍是 MIT 许可、纯浏览器/Node 双环境可用（UMD）、无破坏性 API
  变化后，以 1.4.4 相同方式放入本目录（保留上游原版版权头，不改写），并同步更新
  `web/static/qrcode.js` 头注释中的版本号。
- **审计**：任何第三方新增/升级需人工确认上游来源与供应链（对比 cdnjs/npm 指纹或
  release tag），并跑 `make web-test`（含 qrcode.test.js 结构性断言 + login.test.js
  QR 集成 stub 测试）回归。
