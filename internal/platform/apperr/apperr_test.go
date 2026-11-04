package apperr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

func TestFromFindsWrappedError(t *testing.T) {
	t.Parallel()

	inner := apperr.New(apperr.KindConflict, "PAYMENT_ATTEMPT_ACTIVE")
	err := fmt.Errorf("reserve attempt: %w", inner)

	got := apperr.From(err)
	if got != inner {
		t.Fatalf("From() = %v, want the wrapped *Error", got)
	}
}

func TestFromTreatsUnknownErrorsAsInternal(t *testing.T) {
	t.Parallel()

	cause := errors.New("boom")
	got := apperr.From(cause)

	if got.Kind != apperr.KindInternal || got.Code != apperr.CodeInternal {
		t.Fatalf("From() = %s %s, want internal INTERNAL", got.Kind, got.Code)
	}
	if !errors.Is(got, cause) {
		t.Fatal("internal error must keep the cause for logging")
	}
}

func TestWrapKeepsCauseForErrorsIs(t *testing.T) {
	t.Parallel()

	cause := errors.New("iyzico timeout")
	err := apperr.Wrap(cause, apperr.KindUpstream, "PROVIDER_UNAVAILABLE")

	if !errors.Is(err, cause) {
		t.Fatal("errors.Is must see the cause")
	}
	if want := "upstream PROVIDER_UNAVAILABLE: iyzico timeout"; err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestWithFieldsDoesNotMutateReceiver(t *testing.T) {
	t.Parallel()

	base := apperr.New(apperr.KindInvalid, "VALIDATION_FAILED")
	a := base.WithFields(apperr.FieldError{Pointer: "/iban", Code: "IBAN_INVALID"})
	b := base.WithFields(apperr.FieldError{Pointer: "/phone", Code: "PHONE_INVALID"})

	if len(base.Fields) != 0 {
		t.Fatalf("receiver mutated: %v", base.Fields)
	}
	if len(a.Fields) != 1 || a.Fields[0].Pointer != "/iban" {
		t.Fatalf("a.Fields = %v", a.Fields)
	}
	if len(b.Fields) != 1 || b.Fields[0].Pointer != "/phone" {
		t.Fatalf("b.Fields = %v", b.Fields)
	}
}

func TestZeroKindIsInternal(t *testing.T) {
	t.Parallel()

	var k apperr.Kind
	if k != apperr.KindInternal {
		t.Fatal("zero Kind must be KindInternal so unclassified errors stay 500")
	}
}
