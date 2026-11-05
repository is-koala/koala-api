// Package appversion parses and orders client application versions
// (X-App-Version, ADR-0021). A version is semver: MAJOR.MINOR.PATCH with
// an optional pre-release ("-beta.1") and build metadata ("+3"), the form
// api/components/headers.yaml allows. Build metadata never affects the
// order; a pre-release orders before its release (SemVer 2.0.0 §11).
package appversion

import (
	"errors"
	"strconv"
	"strings"
)

// maxLen bounds the header value that is parsed at all; maxDigits bounds
// one number.
const (
	maxLen    = 64
	maxDigits = 9
)

// Version is a parsed client version. The zero value is 0.0.0.
type Version struct {
	core [3]uint64
	pre  []string
	text string
}

// ErrInvalid is returned for a value that is not MAJOR.MINOR.PATCH with an
// optional -pre-release and +build part.
var ErrInvalid = errors.New("app version: want MAJOR.MINOR.PATCH, for example 1.4.0")

// Parse parses s strictly: three numeric parts without leading zeros, then
// an optional pre-release and build metadata of [0-9A-Za-z-] identifiers
// separated by dots. A number has at most 9 digits (Java's AppVersion
// rule), so it never overflows. The contract's X-App-Version pattern
// (api/components/headers.yaml) accepts exactly the same strings.
func Parse(s string) (Version, error) {
	if s == "" || len(s) > maxLen {
		return Version{}, ErrInvalid
	}
	rest, build, hasBuild := strings.Cut(s, "+")
	if hasBuild && !validIdentifiers(build, false) {
		return Version{}, ErrInvalid
	}
	core, pre, hasPre := strings.Cut(rest, "-")
	if hasPre && !validIdentifiers(pre, true) {
		return Version{}, ErrInvalid
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, ErrInvalid
	}
	v := Version{text: s}
	for i, p := range parts {
		n, ok := numeric(p)
		if !ok {
			return Version{}, ErrInvalid
		}
		v.core[i] = n
	}
	if hasPre {
		v.pre = strings.Split(pre, ".")
	}
	return v, nil
}

// MustParse is Parse for constants in code and tests; it panics on an
// invalid value.
func MustParse(s string) Version {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// String returns the text the version was parsed from ("0.0.0" for the
// zero value).
func (v Version) String() string {
	if v.text == "" {
		return "0.0.0"
	}
	return v.text
}

// Less reports whether v orders before w.
func (v Version) Less(w Version) bool { return v.Compare(w) < 0 }

// Compare returns -1, 0 or +1 as v orders before, with or after w.
func (v Version) Compare(w Version) int {
	for i := range v.core {
		if v.core[i] != w.core[i] {
			if v.core[i] < w.core[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(v.pre) == 0 && len(w.pre) == 0:
		return 0
	case len(v.pre) == 0:
		return 1 // a release orders after its pre-releases
	case len(w.pre) == 0:
		return -1
	}
	for i := 0; i < len(v.pre) && i < len(w.pre); i++ {
		if c := compareIdentifier(v.pre[i], w.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(v.pre) < len(w.pre):
		return -1
	case len(v.pre) > len(w.pre):
		return 1
	default:
		return 0
	}
}

// compareIdentifier orders two pre-release identifiers: numeric ones
// numerically and before alphanumeric ones, the rest in ASCII order.
func compareIdentifier(a, b string) int {
	an, aNum := numeric(a)
	bn, bNum := numeric(b)
	switch {
	case aNum && bNum:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	case aNum:
		return -1
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// numeric parses a SemVer numeric identifier: 1 to maxDigits digits, no
// leading zero.
func numeric(s string) (uint64, bool) {
	if s == "" || len(s) > maxDigits || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// validIdentifiers checks dot-separated [0-9A-Za-z-] identifiers. In a
// pre-release a numeric identifier must not have a leading zero.
func validIdentifiers(s string, pre bool) bool {
	for id := range strings.SplitSeq(s, ".") {
		if id == "" {
			return false
		}
		allDigits := true
		for _, c := range []byte(id) {
			switch {
			case c >= '0' && c <= '9':
			case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-':
				allDigits = false
			default:
				return false
			}
		}
		if _, ok := numeric(id); pre && allDigits && !ok {
			return false // leading zero or more than maxDigits digits
		}
	}
	return true
}
