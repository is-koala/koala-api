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

// TestFrozenDomainsDeferTheirScenarios binds the contract to the scenario
// catalog (ADR-0029 §1). For every domain of frozenDomains:
//
//   - while any operation of its tags still carries x-not-implemented,
//     every P0 scenario of the domain is deferred;
//   - once no operation of its tags carries it, no P0 scenario of the
//     domain may be deferred.
//
// So the PR that implements a domain's last operation must bind that
// domain's P0 scenarios, and a scenario bound ahead of its operations is
// red too. Every marker must also carry a reason.
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
	for _, path := range slices.Sorted(maps.Keys(spec.Paths.Map())) {
		for method, op := range spec.Paths.Value(path).Operations() {
			raw, frozen := op.Extensions[extNotImplemented]
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
			switch {
			case frozen && s.Deferred == nil:
				t.Errorf("%s: domain %q still has frozen operations, so this P0 must be deferred", s.ID, domain)
			case !frozen && s.Deferred != nil:
				t.Errorf("%s: domain %q has no frozen operation left, so this P0 must be bound instead of deferred", s.ID, domain)
			}
		}
	}
}
