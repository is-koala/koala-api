// Package apiv1 is the server side of the /v1 HTTP contract, generated from
// api/openapi.yaml and api/components/*.yaml by oapi-codegen (ADR-0006,
// ADR-0019).
// Do not edit the *.gen.go files: change the spec and run `make generate`.
// CI fails when the committed code is stale (`make gen-check`).
//
// The oapi-codegen version is pinned once, in the -command line below; the
// runtime module in go.mod (github.com/oapi-codegen/runtime) is bumped with
// it. `go run pkg@version` keeps the generator's dependencies out of go.mod
// (ADR-0004).
package apiv1

//go:generate -command oapi-codegen go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.2
//go:generate oapi-codegen -config oapi-codegen.yaml ../../api/openapi.yaml
//go:generate oapi-codegen -config oapi-codegen-components.yaml -o problem.gen.go ../../api/components/problem.yaml
//go:generate oapi-codegen -config oapi-codegen-components.yaml -o money.gen.go ../../api/components/money.yaml
//go:generate oapi-codegen -config oapi-codegen-components.yaml -o pagination.gen.go ../../api/components/pagination.yaml
//go:generate oapi-codegen -config oapi-codegen-components.yaml -o datetime.gen.go ../../api/components/datetime.yaml
//go:generate oapi-codegen -config oapi-codegen-components.yaml -o headers.gen.go ../../api/components/headers.yaml
