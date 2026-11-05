package api_test

import (
	"context"
	"testing"

	"github.com/is-koala/koala-api/api"
)

func TestLoadResolvesAndValidatesTheContract(t *testing.T) {
	t.Parallel()

	doc, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !doc.IsOpenAPI31OrLater() {
		t.Fatalf("openapi = %q, want 3.1", doc.OpenAPI)
	}
	ping := doc.Paths.Value("/v1/meta/ping")
	if ping == nil || ping.Post == nil {
		t.Fatal("POST /v1/meta/ping is missing")
	}
	// The body's Money comes from components/money.yaml.
	money := ping.Post.RequestBody.Value.Content.Get("application/json").Schema.Value.Properties["amount"]
	if money == nil || money.Value == nil || money.Value.Properties["currency"] == nil {
		t.Fatal("the $ref to components/money.yaml was not resolved")
	}
}
