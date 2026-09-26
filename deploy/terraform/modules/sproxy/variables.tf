# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

variable "release_name" {
  description = "Helm release 名称"
  type        = string
  default     = "sproxy"
}

variable "chart_repository" {
  description = "Helm chart 仓库（空 = 本地 chart 路径）"
  type        = string
  default     = ""
}

variable "chart_name" {
  description = "chart 名称（repository 空时为本地路径）"
  type        = string
  default     = "./deploy/sproxy-helm"
}

variable "chart_version" {
  description = "chart 版本（本地路径时忽略）"
  type        = string
  default     = ""
}

variable "namespace" {
  description = "部署命名空间"
  type        = string
  default     = "sproxy"
}

variable "image_tag" {
  description = "sproxy 镜像 tag"
  type        = string
  default     = "latest"
}

variable "replica_count" {
  description = "副本数（多副本需共享卷或单写者语义）"
  type        = number
  default     = 1
}

variable "ingress_enabled" {
  description = "是否创建 Ingress"
  type        = bool
  default     = false
}

variable "secret_create" {
  description = "是否创建 Secret（api_keys/metrics_token）"
  type        = bool
  default     = false
  sensitive   = false
}

variable "secret_values" {
  description = "Secret 键值（如 {apiKeys = \"user:key\", metricsToken = \"tok\"}）——sensitive 标注防 tfstate 明文展示"
  type        = map(string)
  default     = {}
  sensitive   = true
}

variable "storage_size" {
  description = "持久卷大小"
  type        = string
  default     = "10Gi"
}
