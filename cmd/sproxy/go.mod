module github.com/cocomhub/sproxy/cmd/sproxy

go 1.26

require (
	github.com/cocomhub/buildinfo v0.0.1
	github.com/cocomhub/sproxy v0.0.0
	github.com/cocomhub/sproxy/pkg/tunnel/hub/ext/kad v0.0.0
	github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws v0.0.0-20260821150521-399ff896ec8b
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/spf13/viper v1.21.0
)

require (
	github.com/coder/websocket v1.8.15 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/sagikazarmark/locafero v0.12.0 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/cocomhub/sproxy => ../../

replace github.com/cocomhub/sproxy/pkg/tunnel/hub/ext/kad => ../../pkg/tunnel/hub/ext/kad
