module github.com/cocomhub/sproxy

go 1.26

require (
	golang.org/x/crypto v0.54.0
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/kr/pretty v0.3.1 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	gopkg.in/check.v1 v1.0.0-20190902080502-41f04d3bba15 // indirect
)

replace github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws => ./pkg/tunnel/xfer/ext/ws

replace github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic => ./pkg/tunnel/xfer/ext/quic
