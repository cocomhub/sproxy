# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# sproxy Terraform module（roadmap 11.10-⑦）：包装 helm_release 部署 sproxy chart。
# 与 Helm 互补：IaC 描述基础设施（此处=Helm release 声明式管理），chart 描述应用。

terraform {
  required_providers {
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.12"
    }
  }
}

resource "helm_release" "sproxy" {
  name       = var.release_name
  repository = var.chart_repository
  chart      = var.chart_name
  version    = var.chart_version
  namespace  = var.namespace
  create_namespace = true

  set {
    name  = "image.tag"
    value = var.image_tag
  }
  set {
    name  = "replicaCount"
    value = var.replica_count
  }
  set {
    name  = "ingress.enabled"
    value = var.ingress_enabled
  }
  set {
    name  = "secret.create"
    value = var.secret_create
  }
  dynamic "set" {
    for_each = var.secret_values
    content {
      name  = "secret.${set.key}"
      value = set.value
    }
  }
  set {
    name  = "storage.size"
    value = var.storage_size
  }
}
