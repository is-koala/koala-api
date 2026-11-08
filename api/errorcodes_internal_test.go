package api

import (
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// extErrorCodes is the operation extension that lists the operation's own
// domain error codes (stil rehberi §8, ADR-0029 §1). Platform, session and
// idempotency codes are universal: they ride on the common responses and
// are not listed per operation.
const extErrorCodes = "x-error-codes"

// documentedStatus is the shape ErrorCode's description uses to give a
// code's HTTP status: "- `COMPANY_NOT_FOUND` (404): …".
var documentedStatus = regexp.MustCompile("(?m)^\\s*-\\s+`([A-Z][A-Z0-9_]*)`\\s+\\((\\d{3})\\)")

// statusFreeCodes are the ErrorCode members that carry no HTTP status, with
// the reason. Everything else must document one, so a new code cannot be
// added without saying what it answers.
func statusFreeCodes() map[string]string {
	return map[string]string{
		// Field codes travel inside ProblemField.code, under the
		// REQUEST_INVALID of Problem.code; they are not a status of their
		// own (stil rehberi §8).
		"FIELD_REQUIRED":       "a ProblemField code, not a Problem code",
		"FIELD_UNKNOWN":        "a ProblemField code, not a Problem code",
		"FIELD_TYPE_INVALID":   "a ProblemField code, not a Problem code",
		"FIELD_FORMAT_INVALID": "a ProblemField code, not a Problem code",
		"FIELD_VALUE_INVALID":  "a ProblemField code, not a Problem code",
		"FIELD_OUT_OF_RANGE":   "a ProblemField code, not a Problem code",
		// Realtime event codes reach the client as the code of an error
		// event on an open socket; there is no HTTP status on a socket
		// (ADR-0018 §5).
		"REALTIME_COMMAND_UNKNOWN": "the code of an error event, not of a response",
		"REALTIME_COMMAND_INVALID": "the code of an error event, not of a response",
		"REALTIME_RATE_LIMITED":    "the code of an error event, not of a response",
		"CONVERSATION_NOT_FOUND":   "the code of an error event until Faz 4 gives it a REST route",
	}
}

// codeStatuses reads the HTTP status of every ErrorCode member out of the
// enum's own description, and fails for a member that documents none and
// is not a reasoned exception.
func codeStatuses(t *testing.T) (statuses map[string]int, enum []string) {
	t.Helper()
	schema := loadFile(t, "components/problem.yaml").Components.Schemas["ErrorCode"]
	if schema == nil || schema.Value == nil {
		t.Fatal("components/problem.yaml has no ErrorCode schema")
	}
	statuses = map[string]int{}
	for _, m := range documentedStatus.FindAllStringSubmatch(schema.Value.Description, -1) {
		status, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("%s: %v", m[1], err)
		}
		statuses[m[1]] = status
	}
	free := statusFreeCodes()
	for _, v := range schema.Value.Enum {
		code, ok := v.(string)
		if !ok {
			t.Fatalf("ErrorCode member %v is not a string", v)
		}
		enum = append(enum, code)
		if _, documented := statuses[code]; documented {
			if reason, exempt := free[code]; exempt {
				t.Errorf("%s documents a status and is also listed as status-free (%s); drop one", code, reason)
			}
			continue
		}
		if _, exempt := free[code]; !exempt {
			t.Errorf("%s has no documented HTTP status in ErrorCode's description and is not a reasoned exception in statusFreeCodes", code)
		}
	}
	// A stale exception hides a code that was removed.
	for code := range free {
		if !slices.Contains(enum, code) {
			t.Errorf("statusFreeCodes names %s, which is not an ErrorCode member", code)
		}
	}
	return statuses, enum
}

// TestOperationStatusesCoverTheirErrorCodes is the rule that keeps a frozen
// contract honest: an operation that can answer with a code must declare
// that code's status, or a client's generated types cannot decode the
// answer. Without it the freeze is a promise the handlers would have to
// break — adding a status later is the incompatible change the style guide
// forbids (§2), only in the other direction.
//
// Scope: the codes an operation itself produces, which it names in
// x-error-codes. Universal codes (the platform's, the session's, the
// idempotency key's) come from the common responses and are not listed, so
// this test does not check them; the reverse direction — a declared status
// no code explains — is not checked either, and both limits are recorded in
// ADR-0029.
func TestOperationStatusesCoverTheirErrorCodes(t *testing.T) {
	t.Parallel()

	statuses, enum := codeStatuses(t)
	doc := loadFile(t, "openapi.yaml")
	checked := 0
	for _, path := range slices.Sorted(maps.Keys(doc.Paths.Map())) {
		item := doc.Paths.Value(path)
		for _, method := range slices.Sorted(maps.Keys(item.Operations())) {
			op := item.Operations()[method]
			where := method + " " + path
			codes, ok := operationCodes(t, where, op)
			if !ok {
				continue
			}
			checked++
			for _, code := range codes {
				if !slices.Contains(enum, code) {
					t.Errorf("%s: %s %s is not an ErrorCode member", where, extErrorCodes, code)
					continue
				}
				status, documented := statuses[code]
				if !documented {
					t.Errorf("%s: %s has no HTTP status, so no operation can answer with it", where, code)
					continue
				}
				if !answers(op, status) {
					t.Errorf("%s: %s is a %d but the operation declares %v; add the response or drop the code",
						where, code, status, declaredStatuses(op))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no operation carried x-error-codes; the walker misses them")
	}
}

// operationCodes returns the operation's x-error-codes. Every operation
// carries the extension, an empty list included: an operation that can
// fail only with universal codes says so, and a forgotten list is not
// mistaken for one.
func operationCodes(t *testing.T, where string, op *openapi3.Operation) ([]string, bool) {
	t.Helper()
	raw, present := op.Extensions[extErrorCodes]
	if !present {
		t.Errorf("%s: no %s; list the operation's own error codes, or [] when it has none", where, extErrorCodes)
		return nil, false
	}
	list, ok := raw.([]any)
	if !ok {
		t.Errorf("%s: %s must be a list, got %T", where, extErrorCodes, raw)
		return nil, false
	}
	var out []string
	for _, v := range list {
		code, ok := v.(string)
		if !ok {
			t.Errorf("%s: %s member %v is not a string", where, extErrorCodes, v)
			return nil, false
		}
		if slices.Contains(out, code) {
			t.Errorf("%s: %s lists %s twice", where, extErrorCodes, code)
			return nil, false
		}
		out = append(out, code)
	}
	return out, true
}

// answers reports whether the operation declares a response for status. A
// server error rides the default response, which every operation has.
func answers(op *openapi3.Operation, status int) bool {
	if op.Responses == nil {
		return false
	}
	if op.Responses.Value(strconv.Itoa(status)) != nil {
		return true
	}
	return status >= 500 && op.Responses.Default() != nil
}

// declaredStatuses lists the operation's response codes for a failure
// message, "default" excluded.
func declaredStatuses(op *openapi3.Operation) []string {
	if op.Responses == nil {
		return nil
	}
	var out []string
	for code := range op.Responses.Map() {
		if !strings.EqualFold(code, "default") {
			out = append(out, code)
		}
	}
	slices.Sort(out)
	return out
}
