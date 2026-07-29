// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"github.com/cocomhub/sproxy/pkg/certmgr"
)

// GenerateSelfSignedCert 生成 ECDSA P-256 自签证书并写入 PEM 编码的证书和密钥文件。
// 如果父目录不存在，会自动创建。
//
// 这是一个包装函数，委托给 certmgr 包实现。
func GenerateSelfSignedCert(certFile, keyFile string) error {
	return certmgr.GenerateSelfSignedCert(certFile, keyFile)
}
