module github.com/imprezahost/impreza-devkit/agent-go

go 1.26.3

toolchain go1.26.6

require (
	filippo.io/edwards25519 v1.2.0
	github.com/BurntSushi/toml v1.6.0
	github.com/imprezahost/impreza-devkit/sdk-go v0.0.0
	github.com/spf13/cobra v1.10.2
	golang.org/x/crypto v0.57.0
	golang.org/x/sys v0.48.0
)

require deps.dev/util/semver v0.0.0-20260727054525-2946ae4a6141

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/net v0.58.0 // indirect
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/imprezahost/impreza-devkit/sdk-go => ../sdk-go
