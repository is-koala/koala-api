package audit

import (
	"slices"
	"strings"
	"testing"
)

// lists returns the three key lists of the mask by name.
func lists() map[string][]string {
	return map[string][]string{
		"allowedKeys":       allowedKeys(),
		"allowedKeyClasses": allowedKeyClasses(),
		"piiKeyParts":       piiKeyParts(),
	}
}

// The lists are compared against a normalized key (lower case, without
// "_" and "-"), so an entry that is not normalized can never match: it
// would read like a rule and be none. They are grouped by what the keys
// are, not sorted, so only duplicates are checked.
func TestKeyListsAreNormalizedAndFreeOfDuplicates(t *testing.T) {
	t.Parallel()
	for name, list := range lists() {
		for _, e := range list {
			if e != strings.ToLower(e) || strings.ContainsAny(e, "_-") || e == "" {
				t.Errorf("%s: %q is not a normalized key (lower case, no %q, no %q)", name, e, "_", "-")
			}
		}
		sorted := slices.Sorted(slices.Values(list))
		if deduped := slices.Compact(slices.Clone(sorted)); len(deduped) != len(sorted) {
			t.Errorf("%s holds a duplicate: %v", name, list)
		}
	}
}

// Two allowed keys read like personal ones and are kept anyway, by name
// and on purpose: a flag that says the account must change its password
// and a counter that invalidates tokens. Every other allowed key holds no
// personal part, so adding one that does is a decision this test makes
// visible - and it is the mechanism that keeps "passwordFieldName" and
// "secretTokenVersion" out (ADR-0028).
func TestOnlyTwoAllowedKeysHoldAPersonalPart(t *testing.T) {
	t.Parallel()
	var found []string
	for _, k := range allowedKeys() {
		if slices.ContainsFunc(piiKeyParts(), func(p string) bool { return strings.Contains(k, p) }) {
			found = append(found, k)
		}
	}
	slices.Sort(found)
	want := []string{"mustchangepassword", "tokenversion"}
	if !slices.Equal(found, want) {
		t.Fatalf("allowed keys holding a personal part: %v, want %v", found, want)
	}
}

// No class ending may hold a personal part itself: a key matching it
// would then always hold that part too, so the narrowing in allowed()
// would refuse the whole class and the entry would be dead weight.
func TestKeyClassesHoldNoPersonalPart(t *testing.T) {
	t.Parallel()
	for _, c := range allowedKeyClasses() {
		if slices.ContainsFunc(piiKeyParts(), func(p string) bool { return strings.Contains(c, p) }) {
			t.Errorf("class %q holds a personal part, so no key matching it could ever be kept", c)
		}
	}
}

// allowed() is a total decision with one order: exact key, then class
// narrowed by the personal parts, then masked. The table is the rule,
// case by case.
func TestAllowedDecisionTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  string
		want bool
	}{
		{"roles", true},               // exact
		{"ROLES", true},               // case does not matter
		{"token_version", true},       // exact, separators dropped
		{"mustChangePassword", true},  // exact, kept although it reads personal
		{"status", true},              // exact: the ending is no class, the key is listed
		{"statusName", true},          // exact
		{"errorCode", true},           // exact
		{"deviceId", true},            // class "id"
		{"sessionIds", true},          // class "ids"
		{"entry-count", true},         // class "count"
		{"appVersion", true},          // class "version"
		{"isEnabled", true},           // class "enabled"
		{"nationalId", false},         // class, but a personal part
		{"taxId", false},              // same
		{"passportId", false},         // same
		{"drivingLicenseId", false},   // same
		{"salaryTotal", false},        // class "total", but a personal part
		{"emailHostname", false},      // not the exact key "hostname" (and unlisted)
		{"passwordFieldName", false},  // not the exact key "fieldname"
		{"secretTokenVersion", false}, // class "version", but personal parts
		{"memberTokenVersion", false}, // same
		{"targetRoleName", false},     // not the exact key "rolename"
		{"doorCode", false},           // "code" is no class any more
		{"note", false},               // free text
		{"", false},                   // the root of a body that is not an object
		// KVKK m.6: the endings these end in are no longer classes, and
		// their identifier forms are caught by the personal parts.
		{"disabilityStatus", false},
		{"bloodType", false},
		{"religionType", false},
		{"diseaseCode", false},
		{"smokerFlag", false},
		{"unionMembershipStatus", false},
		{"maritalStatus", false},
		{"militaryStatus", false},
		{"healthRecordCount", false},
		{"disabilityId", false},
		{"askerlikDurumu", false},
		{"kanGrubu", false},
		{"engelliDurumu", false},
		{"saglikRaporuId", false},
		// The keys a class ending must keep working for; none of them
		// holds a personal part, and the ones that read closest to one
		// ("financialTotal", "orderingId") are why "inanc" and "din" are
		// not personal parts.
		{"financialTotal", true},
		{"orderingId", true},
		{"bindingId", true},
		{"estateCount", true},
		{"paidTotal", true},
		// Separators: a key written apart is the key it names.
		{"national id", false},
		{"adres id", false},
		{"user.email", false},
		{"target id", true},
	}
	for _, tt := range tests {
		if got := allowed(tt.key); got != tt.want {
			t.Errorf("allowed(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}
