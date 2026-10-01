module github.com/cocomhub/sproxy

go 1.27

require (
	github.com/andybalholm/brotli v1.2.0
	github.com/cocomhub/sproxy/pkg/baidupcs v0.22.0
	github.com/cocomhub/sproxy/pkg/tunnel/mesh v0.22.0
	github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc v0.0.0-20260916140748-05a90dbce3b0
	github.com/google/addlicense v1.2.0
	github.com/klauspost/compress v1.20.0
	github.com/pkg/sftp v1.13.11
	golang.org/x/crypto v0.57.0
	golang.org/x/net v0.59.0
	golang.org/x/perf v0.0.0-20260908200009-22c9c6c9d4da
	golang.org/x/sync v0.23.0
	golang.org/x/sys v0.48.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/bitly/go-simplejson v0.5.0 // indirect
	github.com/bmatcuk/doublestar/v4 v4.0.2 // indirect
	github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws v0.0.0-00010101000000-000000000000 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/safehtml v0.0.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/kardianos/osext v0.0.0-20190222173326-2bc1f35cddc0 // indirect
	github.com/kr/fs v0.1.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.9 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/oleiade/lane v1.0.1 // indirect
	github.com/olekukonko/tablewriter v0.0.4 // indirect
	github.com/pion/datachannel v1.6.2 // indirect
	github.com/pion/dtls/v3 v3.1.8 // indirect
	github.com/pion/ice/v4 v4.4.2 // indirect
	github.com/pion/interceptor v0.1.48 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.2.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.17 // indirect
	github.com/pion/rtp v1.10.5 // indirect
	github.com/pion/sctp v1.11.1 // indirect
	github.com/pion/sdp/v3 v3.0.20 // indirect
	github.com/pion/srtp/v3 v3.0.15 // indirect
	github.com/pion/stun/v4 v4.0.0 // indirect
	github.com/pion/transport/v4 v4.1.1 // indirect
	github.com/pion/turn/v5 v5.1.1 // indirect
	github.com/pion/webrtc/v4 v4.2.20 // indirect
	github.com/qjfoidnh/Baidu-Login v1.4.1 // indirect
	github.com/qjfoidnh/BaiduPCS-Go v0.0.0-20260909034501-1b9131817aaf // indirect
	github.com/qjfoidnh/baidu-tools v1.2.0 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/rs/dnscache v0.0.0-20230804202142-fc85eb664529 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	google.golang.org/protobuf v1.33.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

replace github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws => ./pkg/tunnel/xfer/ext/ws

replace github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic => ./pkg/tunnel/xfer/ext/quic

replace github.com/cocomhub/sproxy/pkg/volume/ext/s3 => ./pkg/volume/ext/s3
