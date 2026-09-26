# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# 最小示例（本地 kind/minikube 可跑）：terraform init && terraform apply

provider "helm" {
  kubernetes {
    config_path = "~/.kube/config"
  }
}

module "sproxy" {
  source     = "../modules/sproxy"
  image_tag  = "v0.21.0"
  namespace  = "sproxy"
}
