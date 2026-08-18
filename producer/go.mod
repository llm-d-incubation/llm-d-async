module github.com/llm-d/llm-d-async/producer

go 1.26.0

require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/llm-d/llm-d-async/api v0.9.0
	github.com/redis/go-redis/v9 v9.22.0
	github.com/stretchr/testify v1.12.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/llm-d/llm-d-async/api => ../api
