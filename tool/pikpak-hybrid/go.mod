// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

module github.com/cocomhub/sproxy/tool/pikpak-hybrid

go 1.27

require github.com/cocomhub/sproxy/pkg/volume/ext/pikpak v0.0.0

require github.com/cocomhub/sproxy v0.0.0 // indirect

replace github.com/cocomhub/sproxy/pkg/volume/ext/pikpak => ../../pkg/volume/ext/pikpak

replace github.com/cocomhub/sproxy => ../..
