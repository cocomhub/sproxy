# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

output "release_name" {
  description = "Helm release 名称"
  value       = helm_release.sproxy.name
}

output "namespace" {
  description = "部署命名空间"
  value       = helm_release.sproxy.namespace
}

output "service_endpoint" {
  description = "服务访问地址（port-forward 后）"
  value       = "http://127.0.0.1:18083"
}
