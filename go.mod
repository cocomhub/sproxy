module github.com/cocomhub/sproxy

go 1.26

require (
	golang.org/x/sys v0.41.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/kr/pretty v0.3.1 // indirect
	gopkg.in/check.v1 v1.0.0-20190902080502-41f04d3bba15 // indirect
)

replace github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws => ./pkg/tunnel/xfer/ext/ws

replace github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic => ./pkg/tunnel/xfer/ext/quic
