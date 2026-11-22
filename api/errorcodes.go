package api

import (
	"context"
	"errors"
	"regexp"
	"strconv"
)

// ErrorCodeStatuses returns the HTTP status every ErrorCode member of
// components/problem.yaml documents in the enum's description. A member
// without one (a field code, a realtime event code) is absent; that every
// other member has one is api's own test (TestOperationStatusesCoverTheirErrorCodes).
//
// It is the one reader of that prose, so the handler binding of
// internal/archtest and the contract tests here read the same statuses.
func ErrorCodeStatuses(ctx context.Context) (map[string]int, error) {
	doc, err := load(ctx, "components/problem.yaml")
	if err != nil {
		return nil, err
	}
	schema := doc.Components.Schemas["ErrorCode"]
	if schema == nil || schema.Value == nil {
		return nil, errors.New("components/problem.yaml has no ErrorCode schema")
	}
	return DocumentedStatuses(schema.Value.Description), nil
}

// DocumentedStatuses reads the "- `CODE` (404): …" lines of an ErrorCode
// description into code → status.
func DocumentedStatuses(description string) map[string]int {
	line := regexp.MustCompile("(?m)^\\s*-\\s+`([A-Z][A-Z0-9_]*)`\\s+\\((\\d{3})\\)")
	out := map[string]int{}
	for _, m := range line.FindAllStringSubmatch(description, -1) {
		if status, err := strconv.Atoi(m[2]); err == nil {
			out[m[1]] = status
		}
	}
	return out
}
