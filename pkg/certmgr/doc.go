// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package certmgr 提供统一证书生命周期管理接口。
//
// 支持三种证书模式：
//
//  1. 静态文件证书（FileCert）：使用 CertFile + KeyFile 指定 PEM 证书和密钥文件路径。
//     适用于已有证书的场景。
//
//  2. ACME 自动证书（ACME）：使用 ACME 协议自动从 Let's Encrypt 等 CA 获取证书。
//     需要配置域名列表和邮箱地址。支持 HTTP-01 挑战（需 80 端口）。
//
//  3. 自签证书（SelfSigned）：自动生成 ECDSA P-256 自签证书。
//     适用于开发环境或内网服务。证书默认存储到 certs/ 目录。
//
// 所有模式均支持 mTLS 客户端证书验证（通过 ClientCA 配置）。
//
// 包依赖：
//   - 标准库（crypto/tls, crypto/x509 等）
//   - golang.org/x/crypto/acme/autocert（仅 ACME 模式需要）
package certmgr
