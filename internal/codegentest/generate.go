// Package codegentest pins what oapi-codegen generates for the OpenAPI 3.1
// constructs tried in the spike (ADR-0019): testdata/openapi31.yaml is
// generated here and codegen_test.go checks the Go shapes. A generator
// upgrade that changes them fails the tests instead of silently changing
// internal/apiv1. The generator version must match internal/apiv1's
// (TestGeneratorVersionMatchesAPIV1).
package codegentest

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.2 -config oapi-codegen.yaml testdata/openapi31.yaml
