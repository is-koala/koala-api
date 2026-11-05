// Package api holds the HTTP contract: openapi.yaml and the shared
// components in components/ (ADR-0005). The files are embedded so the
// server validates requests against the same documents its code is
// generated from (internal/apiv1, ADR-0006).
package api

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

//go:embed openapi.yaml components/*.yaml
var files embed.FS

// Load parses the embedded contract, resolves its references to the
// component files and validates the document. References outside the
// embedded files (other hosts, parent directories) are refused.
func Load(ctx context.Context) (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	loader.Context = ctx
	loader.IsExternalRefsAllowed = true
	loader.ReadFromURIFunc = readEmbedded

	doc, err := loader.LoadFromURI(&url.URL{Path: "openapi.yaml"})
	if err != nil {
		return nil, fmt.Errorf("load openapi.yaml: %w", err)
	}
	if err := doc.Validate(ctx); err != nil {
		return nil, fmt.Errorf("validate openapi.yaml: %w", err)
	}
	return doc, nil
}

func readEmbedded(_ *openapi3.Loader, uri *url.URL) ([]byte, error) {
	if uri.Scheme != "" || uri.Host != "" {
		return nil, fmt.Errorf("reference %s: only the embedded contract files can be referenced", uri)
	}
	name := path.Clean(uri.Path)
	if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
		return nil, errors.New("reference outside api/: only the embedded contract files can be referenced")
	}
	data, err := files.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return data, nil
}
