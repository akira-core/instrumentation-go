module github.com/akira-core/instrumentation-go/otel-gorilla-ws/tests/integration

go 1.25.0

require (
	github.com/akira-core/instrumentation-go/otel-gorilla-ws v0.0.0
	github.com/gorilla/websocket v1.5.3
	github.com/stretchr/testify v1.11.1
	go.opentelemetry.io/otel v1.44.0
	go.opentelemetry.io/otel/sdk v1.44.0
	go.opentelemetry.io/otel/trace v1.44.0
)

require (
	github.com/akira-core/instrumentation-go/otel-flags v0.2.0 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.0 // indirect
	github.com/blang/semver v3.5.1+incompatible // indirect
	github.com/bluele/gcache v0.0.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/diegoholiveira/jsonlogic/v3 v3.10.1 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/nikunjy/rules v1.5.0 // indirect
	github.com/open-feature/go-sdk v1.17.2 // indirect
	github.com/open-feature/go-sdk-contrib/providers/go-feature-flag v1.1.1 // indirect
	github.com/open-feature/go-sdk-contrib/providers/ofrep v0.1.7 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/thomaspoignant/go-feature-flag/modules/core v0.7.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.uber.org/mock v0.6.0 // indirect
	golang.org/x/exp v0.0.0-20240719175910-8a7402abbf56 // indirect
	golang.org/x/sys v0.46.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/akira-core/instrumentation-go/otel-gorilla-ws => ../..
