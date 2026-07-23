module github.com/llm-d/llm-d-async/frontend

go 1.26.0

require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/llm-d/llm-d-async/api v0.9.0
	github.com/llm-d/llm-d-async/producer v0.7.4
	github.com/prometheus/client_golang v1.24.1
	github.com/redis/go-redis/v9 v9.21.0
	github.com/stretchr/testify v1.11.1
	sigs.k8s.io/yaml v1.6.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.yaml.in/yaml/v2 v2.4.4 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/llm-d/llm-d-async/api => ../api

replace github.com/llm-d/llm-d-async/producer => ../producer
