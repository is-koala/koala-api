package authz_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/authztest"
)

// TestPermissionsMatchTheModel: every staff permission is one platform
// relation of model.fga and every platform relation is a permission
// (ADR-0026). The catalog is sorted and has 25 codes (Java's
// StaffPermission).
func TestPermissionsMatchTheModel(t *testing.T) {
	t.Parallel()
	model := loadModel(t)
	ps := authz.Permissions()
	if len(ps) != 25 {
		t.Fatalf("%d permissions, want 25", len(ps))
	}
	var codes []string
	var relations []string
	for _, p := range ps {
		codes = append(codes, string(p.Code))
		relations = append(relations, string(p.Code.Relation()))
		got, ok := authz.LookupPermission(string(p.Code))
		if !ok || got != p {
			t.Errorf("LookupPermission(%s) = %+v, %v", p.Code, got, ok)
		}
		switch p.Sensitivity {
		case authz.SensitivityNormal, authz.SensitivityPersonalData, authz.SensitivityFinancial:
		default:
			t.Errorf("%s: sensitivity %q", p.Code, p.Sensitivity)
		}
	}
	if !slices.IsSorted(codes) {
		t.Errorf("permissions are not sorted by code: %v", codes)
	}
	slices.Sort(relations)
	want := model.Relations(authz.TypePlatform)
	slices.Sort(want)
	if !slices.Equal(relations, want) {
		t.Errorf("permission relations = %v, model platform relations = %v", relations, want)
	}
	if _, ok := authz.LookupPermission("payout.release.all"); ok {
		t.Error("LookupPermission accepted an unknown code")
	}
}

// A denied staff permission is a plain refusal, never concealed: the
// platform is no resource (ADR-0026). A marketplace user holds no staff
// permission (D5).
func TestPlatformDenialIsNotConcealed(t *testing.T) {
	t.Parallel()
	model := loadModel(t)
	tuples, err := authztest.NewTuples(model)
	if err != nil {
		t.Fatal(err)
	}
	ev := authz.NewEvaluator(model, tuples)
	for _, s := range []authz.Subject{authz.Staff(uuid.New()), authz.User(uuid.New())} {
		d, err := ev.Check(context.Background(), s, authz.ActionPlatformPayoutRelease, authz.Platform())
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed || d.Conceal {
			t.Errorf("%s: decision %+v, want a denial that is not concealed", s, d)
		}
	}
}
