// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

module github.com/cocomhub/sproxy/tool/pikpak-hybrid

go 1.27

require github.com/cocomhub/sproxy/pkg/volume/ext/pikpak v0.0.0

require (
	github.com/abema/go-mp4 v1.7.3 // indirect
	github.com/cocomhub/sproxy v0.0.0 // indirect
	github.com/cocomhub/sproxy/pkg/media/ext/mp4 v0.0.0-00010101000000-000000000000 // indirect
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/cocomhub/sproxy/pkg/volume/ext/pikpak => ../../pkg/volume/ext/pikpak

replace github.com/cocomhub/sproxy/pkg/media/ext/mp4 => ../../pkg/media/ext/mp4

replace github.com/cocomhub/sproxy => ../..
