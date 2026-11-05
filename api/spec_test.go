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
	login := doc.Paths.Value("/v1/auth/login")
	if login == nil || login.Post == nil {
		t.Fatal("POST /v1/auth/login is missing")
	}
	// The response's Timestamp comes from components/datetime.yaml.
	ok := login.Post.Responses.Value("200")
	if ok == nil || ok.Value == nil {
		t.Fatal("POST /v1/auth/login has no 200")
	}
	exp := ok.Value.Content.Get("application/json").Schema.Value.Properties["accessTokenExpiresAt"]
	if exp == nil || exp.Value == nil || exp.Value.Format != "date-time" {
		t.Fatal("the $ref to components/datetime.yaml was not resolved")
	}
}
