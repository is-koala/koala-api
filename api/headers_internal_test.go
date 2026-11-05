package api

import (
	"regexp"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/appversion"
)

// X-App-Version's pattern in the contract accepts exactly the strings the
// server's parser accepts, so a client that validates against the spec
// never sends a version the gate cannot read (ADR-0021).
func TestAppVersionPatternMatchesParser(t *testing.T) {
	t.Parallel()

	schema := loadFile(t, "components/headers.yaml").Components.Parameters["XAppVersion"].Value.Schema.Value
	re := regexp.MustCompile(schema.Pattern)
	if schema.MaxLength == nil {
		t.Fatal("XAppVersion has no maxLength")
	}
	maxLen := *schema.MaxLength
	for _, s := range []string{
		"0.0.0", "0.9.32", "1.4.0", "999999999.0.0", "1234567890.0.0", "01.2.3", "1.02.3", "1.2", "1.2.3.4",
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-0.3.7", "1.0.0-x.7.z.92", "1.0.0-x-y-z.--", "1.0.0-01",
		"1.0.0-999999999", "1.0.0-1234567890", "1.0.0-x1234567890", "1.0.0-", "1.0.0-a..b", "1.0.0-a_b",
		"1.1.1+3", "1.0.0+20130313144700", "1.0.0-beta+exp.sha.5114f85", "1.0.0+0001234567890", "1.0.0+",
		"1.0.0+a..b", "v1.0.0", " 1.0.0", "1.0.0 ", "-1.0.0", "", "1.0.0-" + strings.Repeat("a", 58),
		"1.0.0-" + strings.Repeat("a", 59), "1.0.0-ı",
	} {
		spec := re.MatchString(s) && uint64(len(s)) <= maxLen
		_, err := appversion.Parse(s)
		if spec != (err == nil) {
			t.Errorf("%q: spec pattern accepts=%v, appversion.Parse accepts=%v", s, spec, err == nil)
		}
	}
}
