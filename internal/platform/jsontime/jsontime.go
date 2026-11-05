// Package jsontime holds the wire type of an instant (K11, ADR-0005 madde
// 5): RFC 3339 in UTC with "Z". The contract's Timestamp schema is
// generated as this type (api/components/datetime.yaml x-go-type), so a
// handler that sets a time in another zone still writes "Z": the rule is
// enforced where the value is encoded, in one place, not by every caller
// remembering .UTC() (ADR-0021).
package jsontime

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// UTC is an instant that encodes as RFC 3339 in UTC ("…Z"), with as many
// fractional digits as needed, like time.Time's own encoding.
type UTC struct {
	time.Time
}

// From returns t as a UTC wire instant.
func From(t time.Time) UTC { return UTC{Time: t} }

// MarshalJSON writes the instant in UTC.
func (t UTC) MarshalJSON() ([]byte, error) {
	b, err := t.UTC().MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("jsontime: %w", err)
	}
	return b, nil
}

// MarshalText writes the instant in UTC; query and path parameters use it.
func (t UTC) MarshalText() ([]byte, error) {
	b, err := t.UTC().MarshalText()
	if err != nil {
		return nil, fmt.Errorf("jsontime: %w", err)
	}
	return b, nil
}

var errNotUTC = errors.New("jsontime: an instant is written in UTC with Z")

// UnmarshalJSON reads an RFC 3339 instant that ends in "Z". The request
// validator already refuses an offset (Timestamp's pattern); this keeps
// the type honest when it is decoded elsewhere.
func (t *UTC) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return errNotUTC
	}
	return t.UnmarshalText(b[1 : len(b)-1])
}

// UnmarshalText reads an RFC 3339 instant that ends in "Z".
func (t *UTC) UnmarshalText(b []byte) error {
	s := string(b)
	if !strings.HasSuffix(s, "Z") {
		return errNotUTC
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("jsontime: %w", err)
	}
	t.Time = v
	return nil
}

// Bind implements the binder of github.com/oapi-codegen/runtime, which the
// generated code uses for query and path parameters.
func (t *UTC) Bind(src string) error { return t.UnmarshalText([]byte(src)) }
