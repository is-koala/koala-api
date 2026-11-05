package pii_test

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/pii"
)

// adminPhone is a projection: it fills a response field from a personal
// value and takes the grant as a parameter (internal/archtest
// TestRevealOnlyInProjections). Without a grant for the field and the data
// subject the answer is masked.
func adminPhone(phone pii.Value[string], g *pii.Grant) string {
	if clear, ok := pii.Reveal(g, phone); ok {
		return clear
	}
	return pii.MaskString(phone, 4)
}

// A projection without a grant (nil: Gate.Open denied, or its access log
// row could not be written) answers masked.
func ExampleReveal() {
	subject := uuid.MustParse("0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a10")
	phone := pii.NewValue(pii.FieldPhone, subject, "+905551112233")
	fmt.Println(adminPhone(phone, nil))
	fmt.Println(adminPhone(phone, &pii.Grant{}))
	// Output:
	// **********233
	// **********233
}
