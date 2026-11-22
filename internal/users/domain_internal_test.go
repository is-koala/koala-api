package users

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	dbsrc "github.com/is-koala/koala-api/db"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

func ptr[T any](v T) *T { return &v }

// USR-024 and USR-029: a verified identity freezes the name and the birth
// date against a change, not against the value on file; the gender stays
// free. Names are trimmed; a text sent blank or null is cleared.
func TestPatchApply(t *testing.T) {
	t.Parallel()
	born := time.Date(1996, 4, 17, 0, 0, 0, 0, time.UTC)
	cur := Profile{FirstName: "Ayşe", LastName: "Kaya", BirthDate: &born, JobTitle: ptr("Depocu")}
	frozen := cur
	frozen.IdentityVerified = true

	tests := []struct {
		name  string
		cur   Profile
		patch Patch
		want  func(Profile) bool
		code  apperr.Code
	}{
		{"names trimmed", cur, Patch{FirstName: ptr("  Zeynep ")}, func(p Profile) bool { return p.FirstName == "Zeynep" }, ""},
		{"text cleared by null", cur, Patch{JobTitle: Field[string]{Set: true}}, func(p Profile) bool { return p.JobTitle == nil }, ""},
		{"text cleared by blank", cur, Patch{JobTitle: Field[string]{Set: true, Value: ptr("  ")}}, func(p Profile) bool { return p.JobTitle == nil }, ""},
		{"text kept when not sent", cur, Patch{}, func(p Profile) bool { return p.JobTitle != nil && *p.JobTitle == "Depocu" }, ""},
		{"frozen name", frozen, Patch{FirstName: ptr("Zeynep")}, nil, CodeFieldImmutable},
		{"frozen birth date", frozen, Patch{BirthDate: Field[time.Time]{Set: true, Value: ptr(born.AddDate(0, 0, 1))}}, nil, CodeFieldImmutable},
		{"frozen birth date cleared", frozen, Patch{BirthDate: Field[time.Time]{Set: true}}, nil, CodeFieldImmutable},
		{"frozen, the value on file", frozen, Patch{FirstName: ptr(" Ayşe"), BirthDate: Field[time.Time]{Set: true, Value: ptr(born)}},
			func(p Profile) bool { return !fieldsChanged(frozen, p) }, ""},
		{"frozen, the gender is free", frozen, Patch{Gender: Field[string]{Set: true, Value: ptr("FEMALE")}},
			func(p Profile) bool { return p.Gender != nil && *p.Gender == "FEMALE" }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.patch.apply(tt.cur)
			if tt.code != "" {
				if apperr.From(err).Code != tt.code {
					t.Fatalf("USR-024: apply = %v, want %s", err, tt.code)
				}
				return
			}
			if err != nil || !tt.want(got) {
				t.Fatalf("USR-024/USR-029: apply = %+v, %v", got, err)
			}
		})
	}
}

// USR-027: the identity number is fully blanked once verified and absent
// before.
func TestMaskedIdentityNumber(t *testing.T) {
	t.Parallel()
	if (Profile{}).MaskedIdentityNumber() != nil {
		t.Error("USR-027: an unverified identity shows a mask")
	}
	if m := (Profile{IdentityVerified: true}).MaskedIdentityNumber(); m == nil || *m != "***********" {
		t.Errorf("USR-027: verified mask = %v", m)
	}
}

// USR-028: an end before the start is refused; an ongoing education and
// one year long are not.
func TestEducationPeriod(t *testing.T) {
	t.Parallel()
	in := EducationInput{StartYear: 2014}
	if err := in.check(); err != nil {
		t.Errorf("ongoing: %v", err)
	}
	in.EndYear = ptr(int32(2014))
	if err := in.check(); err != nil {
		t.Errorf("one year: %v", err)
	}
	in.EndYear = ptr(int32(2013))
	if apperr.From(in.check()).Code != CodeEducationPeriodInvalid {
		t.Error("USR-028: an end before the start passed")
	}
}

// The page is cut on the keyset in both directions, also around a
// position whose record is gone; a malformed cursor is CURSOR_INVALID.
func TestEducationPages(t *testing.T) {
	t.Parallel()
	var all []Education
	for _, end := range []int32{0, 2020, 2018, 2016, 2014} {
		e := Education{ID: uuid.New(), StartYear: 2010}
		if end != 0 {
			e.EndYear = ptr(end)
		}
		all = append(all, e)
	}
	ids := func(p EducationPage) []uuid.UUID {
		out := make([]uuid.UUID, 0, len(p.Items))
		for _, e := range p.Items {
			out = append(out, e.ID)
		}
		return out
	}
	cursor := func(s *string) *educationCursor {
		t.Helper()
		if s == nil {
			t.Fatal("no cursor")
		}
		c, err := decodeEducationCursor(*s)
		if err != nil {
			t.Fatal(err)
		}
		return &c
	}
	first := pageOf(all, 2, nil)
	if got := ids(first); len(got) != 2 || got[0] != all[0].ID || got[1] != all[1].ID || first.PrevCursor != nil {
		t.Fatalf("first = %v", got)
	}
	second := pageOf(all, 2, cursor(first.NextCursor))
	if got := ids(second); len(got) != 2 || got[0] != all[2].ID || got[1] != all[3].ID {
		t.Fatalf("second = %v", got)
	}
	// The record at the position is gone: the next page still starts
	// after it.
	without := slices.Concat(all[:3], all[4:])
	if got := ids(pageOf(without, 2, cursor(second.NextCursor))); len(got) != 1 || got[0] != all[4].ID {
		t.Fatalf("after a deleted position = %v", got)
	}
	back := pageOf(all, 2, cursor(second.PrevCursor))
	if got := ids(back); len(got) != 2 || got[0] != all[0].ID || got[1] != all[1].ID || back.PrevCursor != nil {
		t.Fatalf("back = %v", got)
	}
	for _, bad := range []string{
		"bozuk",
		"ZWR1MXxufDF8Mg", // edu1|n|1|2: a part short
		"dnJmMXxufDF8MnwwMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDA", // another list's prefix
		"ZWR1MXx4fDF8MnwwMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDA", // no direction
		"ZWR1MXxufGF8MnwwMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDA", // an end that is no number
	} {
		if _, err := decodeEducationCursor(bad); apperr.From(err).Code != "CURSOR_INVALID" {
			t.Errorf("cursor %q: %v", bad, err)
		}
	}
}

// The profile answers its links to later modules with null and an empty
// list (ADR-0036 Karar 5). The day one of those tables lands, this is red:
// the PR that brings it wires the profile to it in the same change, so a
// live link is never answered as absent.
func TestProfileLinksWaitForTheirTables(t *testing.T) {
	t.Parallel()
	cat, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for table, link := range map[string]string{
		"media":     "profilePictureId and cvId (PATCH answers 501 for an id, GET answers null)",
		"addresses": "addressId (PATCH answers 501 for an id, GET answers null)",
		"companies": "companies (GET answers an empty list)",
	} {
		if _, ok := cat.Table(table); ok {
			t.Errorf("the %s table landed: wire the profile's %s to it (ADR-0036 Karar 5)", table, link)
		}
	}
	if _, ok := cat.Table("users"); !ok {
		t.Fatal("the catalog has no users table; the check reads the wrong catalog")
	}
}
