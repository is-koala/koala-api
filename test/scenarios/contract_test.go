package scenarios_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/api"
)

// extNotImplemented marks an operation whose contract is frozen but whose
// handler has not landed; it answers 501 under its default response
// (ADR-0029 §1).
const extNotImplemented = "x-not-implemented"

// minReasonLength is the shortest reason an x-not-implemented may carry,
// the same bound the contract's other reasoned extensions use
// (x-resource-reason, x-authz-exempt).
const minReasonLength = 20

// boundAhead maps a P0 scenario of a domain that still has frozen
// operations, bound before the domain's last operation lands, to the
// operations whose handlers it tests (ADR-0032 Karar 5). The map is test
// code, not catalog: a scenario is written in API-independent language
// (README.md), so the operations are named here, where the contract is
// read anyway.
func boundAhead() map[string][]string {
	return map[string][]string{
		"USR-001": {"startRegistration"},
		"USR-002": {"startRegistration"},
		"USR-003": {"startRegistration", "verifyRegistration"},
		"USR-004": {"verifyRegistration", "completeRegistration"},
		"USR-005": {"completeRegistration"},
		"USR-006": {"completeRegistration"},
		"USR-007": {"startRegistration"},
		"USR-008": {"startMyVerification", "confirmMyVerification"},
		"USR-009": {"confirmMyVerification"},
		"USR-010": {"startMyVerification", "confirmMyVerification"},
		"USR-011": {"confirmMyVerification", "getMyVerification", "cancelMyVerification"},
		"USR-012": {"confirmMyVerification"},
		"USR-013": {"confirmMyVerification"},
		"USR-015": {"completePasswordReset"},
		"USR-016": {"completePasswordReset"},
		"USR-017": {"requestPasswordReset", "changeMyPassword"},
		"USR-018": {"requestPasswordReset"},
		"USR-019": {"completePasswordReset"},
		"USR-020": {"changeMyPassword"},
		"USR-021": {"completeRegistration", "completePasswordReset", "changeMyPassword"},
		// Media uploads (ADR-0035); the domain's address and reference
		// operations are still frozen.
		"MED-001": {"startMediaUpload", "completeMediaUpload", "getMedia"},
		"MED-002": {"startMediaUpload", "completeMediaUpload"},
		"MED-003": {"getMedia", "deleteMedia"},
		"MED-004": {"getMedia", "deleteMedia"},
		"MED-005": {"completeMediaUpload"},
		"MED-006": {"completeMediaUpload", "deleteMedia"},
		"MED-014": {"startMediaUpload", "completeMediaUpload", "getMedia"},
	}
}

// TestFrozenDomainsDeferTheirScenarios binds the contract to the scenario
// catalog (ADR-0029 §1, ADR-0032 Karar 5). For every domain of
// frozenDomains:
//
//   - while any operation of its tags still carries x-not-implemented,
//     every P0 scenario of the domain is deferred, unless boundAhead names
//     it with operations that all have their handler;
//   - once no operation of its tags carries it, no P0 scenario of the
//     domain may be deferred, and boundAhead holds none of its scenarios.
//
// So the PR that implements a domain's last operation must bind that
// domain's P0 scenarios; a scenario bound ahead of its own operations is
// red, and so is a boundAhead entry that is deferred, names an operation
// that does not exist, is still frozen or belongs to another domain's tags.
// Every marker must also carry a reason.
func TestFrozenDomainsDeferTheirScenarios(t *testing.T) {
	t.Parallel()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// marked counts the operations of each tag that are still frozen, and
	// tagged counts all of them, so a tag that no operation carries (a
	// typo in frozenDomains) is caught instead of passing silently.
	marked, tagged := map[string]int{}, map[string]int{}
	opFrozen := map[string]bool{}   // operationId → still frozen
	opTags := map[string][]string{} // operationId → its tags
	for _, path := range slices.Sorted(maps.Keys(spec.Paths.Map())) {
		for method, op := range spec.Paths.Value(path).Operations() {
			raw, frozen := op.Extensions[extNotImplemented]
			opFrozen[op.OperationID] = frozen
			opTags[op.OperationID] = op.Tags
			if frozen {
				reason, ok := raw.(string)
				if !ok || len(strings.TrimSpace(reason)) < minReasonLength {
					t.Errorf("%s %s: %s needs a reason of at least %d characters, got %v",
						method, path, extNotImplemented, minReasonLength, raw)
				}
			}
			for _, tag := range op.Tags {
				tagged[tag]++
				if frozen {
					marked[tag]++
				}
			}
		}
	}

	ahead := boundAhead()
	seenAhead := map[string]bool{}

	byDomain := map[string]catalog{}
	for _, c := range loadCatalogs(t) {
		byDomain[c.Domain] = c
	}
	for _, domain := range slices.Sorted(maps.Keys(frozenDomains())) {
		c, ok := byDomain[domain]
		if !ok {
			t.Errorf("frozenDomains names domain %q, which has no catalog file", domain)
			continue
		}
		frozen := false
		for _, tag := range frozenDomains()[domain] {
			if tagged[tag] == 0 {
				t.Errorf("domain %q names contract tag %q, which no operation carries", domain, tag)
				continue
			}
			if marked[tag] > 0 {
				frozen = true
			}
		}
		for _, s := range c.Scenarios {
			if s.Priority != "P0" {
				continue
			}
			// A P0 that waits for a later phase is out of this rule: its
			// operations are not part of this domain's freeze.
			if s.Deferred != nil && s.Deferred.Phase > currentPhase {
				continue
			}
			aheadOps, early := ahead[s.ID]
			if early {
				seenAhead[s.ID] = true
				for _, f := range aheadFindings(s.ID, aheadOps, domain, frozen, s.Deferred != nil, opFrozen, opTags) {
					t.Error(f)
				}
				continue
			}
			switch {
			case frozen && s.Deferred == nil:
				t.Errorf("%s: domain %q still has frozen operations, so this P0 must be deferred or named in boundAhead", s.ID, domain)
			case !frozen && s.Deferred != nil:
				t.Errorf("%s: domain %q has no frozen operation left, so this P0 must be bound instead of deferred", s.ID, domain)
			}
		}
	}
	// An entry no P0 of a frozen domain matches is stale (a typo, a
	// scenario moved). A rule that stopped reading the map would not pass
	// either: the bound scenarios above would then be red as undeferred.
	for _, id := range slices.Sorted(maps.Keys(ahead)) {
		if !seenAhead[id] {
			t.Errorf("boundAhead names %s, which is no P0 scenario of a frozen domain", id)
		}
	}
}

// aheadFindings holds one boundAhead entry against the contract and its
// scenario: the scenario is bound, not deferred; its domain still has
// frozen operations (else the entry is stale); and every operation it
// names exists, has its handler and carries one of the domain's tags, so a
// scenario cannot ride on another domain's landed handler.
func aheadFindings(id string, ops []string, domain string, domainFrozen, deferred bool,
	opFrozen map[string]bool, opTags map[string][]string,
) []string {
	var out []string
	if !domainFrozen {
		out = append(out, id+": domain "+domain+" has no frozen operation left; drop the scenario from boundAhead")
	}
	if deferred {
		out = append(out, id+": boundAhead names it, so its operations landed and it must be bound instead of deferred")
	}
	if len(ops) == 0 {
		out = append(out, id+": boundAhead names no operation")
	}
	for _, op := range ops {
		frozen, ok := opFrozen[op]
		switch {
		case !ok:
			out = append(out, id+": boundAhead names "+op+", which is not an operation of the contract")
		case frozen:
			out = append(out, id+": boundAhead names "+op+", which still carries "+extNotImplemented+"; the scenario is bound ahead of its handler")
		}
		if !slices.ContainsFunc(opTags[op], func(tag string) bool { return slices.Contains(frozenDomains()[domain], tag) }) {
			out = append(out, id+": boundAhead names "+op+", whose tags are not among domain "+domain+"'s")
		}
	}
	return out
}

// TestBoundAheadFindings is the rule's own red and green, on the real
// contract's operations.
func TestBoundAheadFindings(t *testing.T) {
	t.Parallel()

	opFrozen := map[string]bool{"startRegistration": false, "loginUser": false, "getMyProfile": true}
	opTags := map[string][]string{"startRegistration": {"users"}, "loginUser": {"auth"}, "getMyProfile": {"users"}}
	tests := []struct {
		name     string
		ops      []string
		frozen   bool // the domain still has frozen operations
		deferred bool
		want     []string // substrings, one per finding
	}{
		{name: "a landed operation of the domain", ops: []string{"startRegistration"}, frozen: true},
		{name: "another domain's operation", ops: []string{"loginUser"}, frozen: true, want: []string{"tags are not among"}},
		{
			name: "USR-022 on loginUser, still deferred", ops: []string{"loginUser"}, frozen: true, deferred: true,
			want: []string{"must be bound instead of deferred", "tags are not among"},
		},
		{name: "a frozen operation", ops: []string{"getMyProfile"}, frozen: true, want: []string{"bound ahead of its handler"}},
		{
			name: "an unknown operation", ops: []string{"startRegistrations"}, frozen: true,
			want: []string{"not an operation of the contract", "tags are not among"},
		},
		{name: "a domain no longer frozen", ops: []string{"startRegistration"}, want: []string{"drop the scenario"}},
		{name: "no operation", frozen: true, want: []string{"names no operation"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := aheadFindings("USR-022", tt.ops, "user", tt.frozen, tt.deferred, opFrozen, opTags)
			if len(got) != len(tt.want) {
				t.Fatalf("findings %v, want %d", got, len(tt.want))
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("finding %q lacks %q", got[i], w)
				}
			}
		})
	}
}
