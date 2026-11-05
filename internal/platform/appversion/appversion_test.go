package appversion_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/appversion"
)

func TestParseRejectsMalformed(t *testing.T) {
	t.Parallel()

	for _, s := range []string{
		"", "1", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "a.b.c", "1.2.3-", "1.2.3+",
		"1.2.3-01", "1.2.3-a..b", "1.2.3-a_b", " 1.2.3", "1.2.3 ", "v1.2.3", "-1.2.3",
		"1.2.3-" + strings.Repeat("a", 64), "99999999999999999999.0.0", "1234567890.0.0",
		"1.0.0-1234567890",
	} {
		if _, err := appversion.Parse(s); !errors.Is(err, appversion.ErrInvalid) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalid", s, err)
		}
	}
}

// TestCompare pins SemVer 2.0.0 §11 precedence, including the examples of
// the specification: 1.0.0-alpha < 1.0.0-alpha.1 < 1.0.0-alpha.beta <
// 1.0.0-beta < 1.0.0-beta.2 < 1.0.0-beta.11 < 1.0.0-rc.1 < 1.0.0.
func TestCompare(t *testing.T) {
	t.Parallel()

	ordered := []string{
		"0.0.0", "0.9.32", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "1.10.0", "2.0.0",
	}
	for i := range ordered {
		for j := range ordered {
			a, b := appversion.MustParse(ordered[i]), appversion.MustParse(ordered[j])
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := a.Compare(b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
}

func TestParseAcceptsNineDigits(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"999999999.0.0", "1.0.0-999999999", "1.0.0-x1234567890", "1.0.0+0001234567890"} {
		if _, err := appversion.Parse(s); err != nil {
			t.Errorf("Parse(%q) = %v", s, err)
		}
	}
}

func TestBuildMetadataDoesNotOrder(t *testing.T) {
	t.Parallel()

	a, b := appversion.MustParse("1.1.1+3"), appversion.MustParse("1.1.1+40")
	if a.Compare(b) != 0 || a.Less(appversion.MustParse("1.1.1")) {
		t.Fatal("build metadata changed the order")
	}
	if a.String() != "1.1.1+3" {
		t.Fatalf("String() = %q", a.String())
	}
	if (appversion.Version{}).String() != "0.0.0" {
		t.Fatal("zero value is not 0.0.0")
	}
}
