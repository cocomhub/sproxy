module github.com/cocomhub/sproxy/web/e2e

go 1.26.4

require (
	github.com/cocomhub/sproxy v0.0.0-00010101000000-000000000000
	github.com/mxschmitt/playwright-go v0.6100.0
)

replace github.com/cocomhub/sproxy => ../../

require (
	github.com/deckarep/golang-set/v2 v2.8.0 // indirect
	github.com/go-jose/go-jose/v3 v3.0.5 // indirect
	github.com/go-stack/stack v1.8.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
