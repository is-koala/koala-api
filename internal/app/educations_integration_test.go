//go:build integration

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/users"
)

// academicRef is one university with a faculty and a department under it.
type academicRef struct {
	university, faculty, department uuid.UUID
}

// seedReference writes reference rows as their migration would: the
// application only reads them (M4, ADR-0036).
func (a *authApp) seedReference(t *testing.T) academicRef {
	t.Helper()
	owner := a.dbt.Open(t, db.RoleOwner)
	ref := academicRef{university: uuid.New(), faculty: uuid.New(), department: uuid.New()}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO universities (id, name) VALUES ($1, $2)", []any{ref.university, "Örnek Üniversitesi " + ref.university.String()[:8]}},
		{"INSERT INTO faculties (id, university_id, name) VALUES ($1, $2, 'Mühendislik Fakültesi')", []any{ref.faculty, ref.university}},
		{"INSERT INTO departments (id, faculty_id, degree, name) VALUES ($1, $2, 'BACHELOR', 'Bilgisayar Mühendisliği')", []any{ref.department, ref.faculty}},
	} {
		if _, err := owner.Pool().Exec(t.Context(), stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	return ref
}

// educationBody is an EducationInput.
func educationBody(university uuid.UUID, faculty, department *uuid.UUID, start int, end *int) string {
	var b strings.Builder
	b.WriteString(`{"universityId":"` + university.String() + `","degree":"BACHELOR","startYear":` + strconv.Itoa(start))
	if faculty != nil {
		b.WriteString(`,"facultyId":"` + faculty.String() + `"`)
	}
	if department != nil {
		b.WriteString(`,"departmentId":"` + department.String() + `"`)
	}
	if end != nil {
		b.WriteString(`,"endYear":` + strconv.Itoa(*end))
	}
	b.WriteString("}")
	return b.String()
}

func year(y int) *int { return &y }

type educationAnswer struct {
	ID         uuid.UUID `json:"id"`
	University struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	} `json:"university"`
	Faculty    *struct{ Name string } `json:"faculty"`
	Department *struct{ Name string } `json:"department"`
	EndYear    *int                   `json:"endYear"`
}

type educationList struct {
	Items      []educationAnswer `json:"items"`
	NextCursor *string           `json:"nextCursor"`
	PrevCursor *string           `json:"prevCursor"`
}

// createEducation posts an education record as bearer and returns the
// answer.
func (a *authApp) createEducation(t *testing.T, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	return a.postKeyedAs(t, "/v1/me/educations", "edu-"+uuid.NewString(), body, bearer)
}

// The education operations over the contract (USR-028): create (201 and
// its Location), list, replace and delete; the reference rows bind the
// record (an unknown university, a faculty of another university, a
// department without its faculty); the period rule; an ongoing record
// leads the list.
func TestEducationsOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "egitim@example.com")
	ref, other := a.seedReference(t), a.seedReference(t)

	rec := a.createEducation(t, tok, educationBody(ref.university, &ref.faculty, &ref.department, 2014, year(2018)))
	wantStatus(t, rec, http.StatusCreated, "")
	done := decodeBody[educationAnswer](t, rec)
	if rec.Header().Get("Location") != "/v1/me/educations/"+done.ID.String() || done.University.ID != ref.university ||
		done.Faculty == nil || done.Department == nil || done.Department.Name != "Bilgisayar Mühendisliği" {
		t.Fatalf("created = %s (Location %q)", rec.Body, rec.Header().Get("Location"))
	}
	rec = a.createEducation(t, tok, educationBody(other.university, nil, nil, 2019, nil))
	wantStatus(t, rec, http.StatusCreated, "")
	ongoing := decodeBody[educationAnswer](t, rec)
	if ongoing.Faculty != nil || ongoing.Department != nil || ongoing.EndYear != nil {
		t.Fatalf("ongoing = %s", rec.Body)
	}

	rec = a.call(t, http.MethodGet, "/v1/me/educations", tok, "")
	wantStatus(t, rec, http.StatusOK, "")
	list := decodeBody[educationList](t, rec)
	if len(list.Items) != 2 || list.Items[0].ID != ongoing.ID || list.Items[1].ID != done.ID ||
		list.NextCursor != nil || list.PrevCursor != nil {
		t.Fatalf("USR-028: list = %s", rec.Body)
	}

	// The reference rows and the period.
	for _, tc := range []struct {
		body, code string
		status     int
	}{
		{educationBody(uuid.New(), nil, nil, 2014, nil), string(users.CodeReferenceNotFound), http.StatusNotFound},
		{educationBody(ref.university, &other.faculty, nil, 2014, nil), string(users.CodeReferenceNotFound), http.StatusNotFound},
		{educationBody(ref.university, &ref.faculty, &other.department, 2014, nil), string(users.CodeReferenceNotFound), http.StatusNotFound},
		{educationBody(ref.university, nil, &ref.department, 2014, nil), "REQUEST_INVALID", http.StatusBadRequest},
		{educationBody(ref.university, nil, nil, 2014, year(2013)), string(users.CodeEducationPeriodInvalid), http.StatusBadRequest},
	} {
		wantStatus(t, a.createEducation(t, tok, tc.body), tc.status, tc.code)
		wantStatus(t, a.call(t, http.MethodPut, "/v1/me/educations/"+done.ID.String(), tok, tc.body), tc.status, tc.code)
	}

	rec = a.call(t, http.MethodPut, "/v1/me/educations/"+done.ID.String(), tok, educationBody(other.university, &other.faculty, nil, 2015, year(2019)))
	wantStatus(t, rec, http.StatusOK, "")
	if got := decodeBody[educationAnswer](t, rec); got.ID != done.ID || got.University.ID != other.university || got.Department != nil {
		t.Fatalf("replaced = %s", rec.Body)
	}
	wantStatus(t, a.call(t, http.MethodDelete, "/v1/me/educations/"+done.ID.String(), tok, ""), http.StatusNoContent, "")
	wantStatus(t, a.call(t, http.MethodDelete, "/v1/me/educations/"+done.ID.String(), tok, ""),
		http.StatusNotFound, string(users.CodeEducationNotFound))
	rec = a.call(t, http.MethodGet, "/v1/me/educations", tok, "")
	if list := decodeBody[educationList](t, rec); len(list.Items) != 1 || list.Items[0].ID != ongoing.ID {
		t.Fatalf("after the delete = %s", rec.Body)
	}
}

// Another user's record is the 404 of one that never existed, byte for
// byte, for the replace and the delete, whatever the body (USR-028); the
// owner's record is untouched.
func TestEducationsOfAnotherUserAreMissing(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	owner, intruder := a.addSignedIn(t, "sahip@example.com"), a.addSignedIn(t, "davetsiz@example.com")
	ref := a.seedReference(t)
	rec := a.createEducation(t, owner, educationBody(ref.university, nil, nil, 2010, year(2014)))
	wantStatus(t, rec, http.StatusCreated, "")
	theirs := decodeBody[educationAnswer](t, rec).ID
	missing := uuid.New()

	for _, op := range []struct{ method, body string }{
		{http.MethodPut, educationBody(ref.university, nil, nil, 2011, nil)},
		{http.MethodPut, educationBody(uuid.New(), nil, nil, 2011, nil)},
		{http.MethodDelete, ""},
	} {
		got := a.call(t, op.method, "/v1/me/educations/"+theirs.String(), intruder, op.body)
		none := a.call(t, op.method, "/v1/me/educations/"+missing.String(), intruder, op.body)
		wantStatus(t, got, http.StatusNotFound, string(users.CodeEducationNotFound))
		if got.Code != none.Code || got.Body.String() != none.Body.String() {
			t.Errorf("USR-028 %s: another user's %d %s / a missing one %d %s", op.method, got.Code, got.Body, none.Code, none.Body)
		}
	}
	rec = a.call(t, http.MethodGet, "/v1/me/educations", intruder, "")
	if strings.Contains(rec.Body.String(), theirs.String()) {
		t.Fatalf("the intruder's list shows the owner's record: %s", rec.Body)
	}
	rec = a.call(t, http.MethodGet, "/v1/me/educations", owner, "")
	if list := decodeBody[educationList](t, rec); len(list.Items) != 1 || list.Items[0].EndYear == nil || *list.Items[0].EndYear != 2014 {
		t.Fatalf("the owner's record changed: %s", rec.Body)
	}
}

// A user has at most EducationsPerUser records, and concurrent creates
// cannot pass the cap together: they count under the user's education
// write lock (ADR-0036 Karar 6).
func TestEducationLimitHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "sinir@example.com")
	ref := a.seedReference(t)
	for i := range users.EducationsPerUser - 1 {
		wantStatus(t, a.createEducation(t, tok, educationBody(ref.university, nil, nil, 1990+i, nil)), http.StatusCreated, "")
	}
	const racers = 6
	codes := make([]int, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			codes[i] = a.createEducation(t, tok, educationBody(ref.university, nil, nil, 2020, nil)).Code
		})
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Errorf("a racing create answered %d", c)
		}
	}
	if created != 1 {
		t.Fatalf("%d racing creates passed the cap, want 1 (%v)", created, codes)
	}
	wantStatus(t, a.createEducation(t, tok, educationBody(ref.university, nil, nil, 2021, nil)),
		http.StatusConflict, string(users.CodeEducationLimitReached))
	if n := a.count(t, "SELECT count(*) FROM active_educations e JOIN active_users u ON u.id = e.user_id WHERE u.email = $1",
		"sinir@example.com"); n != users.EducationsPerUser {
		t.Fatalf("%d records, want %d", n, users.EducationsPerUser)
	}
}

// The list pages on its keyset in both directions; a malformed cursor is
// CURSOR_INVALID.
func TestEducationListPages(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "sayfa@example.com")
	ref := a.seedReference(t)
	var want []uuid.UUID // newest end first
	for _, end := range []int{2020, 2018, 2016, 2014, 2012} {
		rec := a.createEducation(t, tok, educationBody(ref.university, nil, nil, end-4, year(end)))
		wantStatus(t, rec, http.StatusCreated, "")
		want = append(want, decodeBody[educationAnswer](t, rec).ID)
	}
	page := func(query string) educationList {
		t.Helper()
		rec := a.call(t, http.MethodGet, "/v1/me/educations"+query, tok, "")
		wantStatus(t, rec, http.StatusOK, "")
		return decodeBody[educationList](t, rec)
	}
	ids := func(l educationList) []uuid.UUID {
		out := make([]uuid.UUID, 0, len(l.Items))
		for _, it := range l.Items {
			out = append(out, it.ID)
		}
		return out
	}
	same := func(what string, got, want []uuid.UUID) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s = %v, want %v", what, got, want)
			}
		}
	}
	first := page("?limit=2")
	same("first page", ids(first), want[:2])
	if first.PrevCursor != nil || first.NextCursor == nil {
		t.Fatalf("first page cursors %v %v", first.PrevCursor, first.NextCursor)
	}
	second := page("?limit=2&cursor=" + *first.NextCursor)
	same("second page", ids(second), want[2:4])
	last := page("?limit=2&cursor=" + *second.NextCursor)
	same("last page", ids(last), want[4:])
	if last.NextCursor != nil || last.PrevCursor == nil {
		t.Fatalf("last page cursors %v %v", last.PrevCursor, last.NextCursor)
	}
	back := page("?limit=2&cursor=" + *last.PrevCursor)
	same("back", ids(back), want[2:4])
	wantStatus(t, a.call(t, http.MethodGet, "/v1/me/educations?cursor=bozuk", tok, ""),
		http.StatusBadRequest, string(authn.CodeCursorInvalid))
}

// An education record is a child of the user: a create for a user who is
// soft-deleted meanwhile finds no active parent and writes nothing
// (softdelete.LockActiveParent, ADR-0020). The service is called directly:
// a deleted user's session no longer authenticates over HTTP.
func TestEducationNeedsAnActiveUser(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	a.addSignedIn(t, "silinen@example.com")
	id := a.userID(t, "silinen@example.com")
	ref := a.seedReference(t)
	cat := dbtest.SoftDeleteCatalog(t)
	svc, err := users.NewService(a.db, a.mods.verifications, a.mods.media, a.mods.address, softdelete.New(cat), a.clock)
	if err != nil {
		t.Fatal(err)
	}
	err = a.db.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		_, err := softdelete.New(cat).Delete(ctx, q, softdelete.DeleteRequest{
			Table: "users", ID: id, Actor: softdelete.Actor{Kind: softdelete.ActorSystem},
			Reason: softdelete.ReasonAccountDeletion, At: a.clock.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateEducation(t.Context(), authn.UserPrincipal{UserID: id},
		users.EducationInput{UniversityID: ref.university, Degree: "BACHELOR", StartYear: 2014},
		func(context.Context, db.Querier, users.Education) error { return nil })
	if err == nil {
		t.Fatal("a deleted user got an education record")
	}
	var n int
	// The base table, deleted rows included: koala_maint reads it.
	if err := a.maint.Pool().QueryRow(t.Context(), "SELECT count(*) FROM educations WHERE user_id = $1", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d records written for a deleted user", n)
	}
}

// A retired reference row cannot be chosen anew, but a record keeps the one
// it names already: a replace that leaves it as it is passes, one that
// picks another retired row is refused (M4, ADR-0036).
func TestEducationRetiredReferences(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "emekli@example.com")
	ref, other := a.seedReference(t), a.seedReference(t)
	rec := a.createEducation(t, tok, educationBody(ref.university, &ref.faculty, &ref.department, 2010, year(2014)))
	wantStatus(t, rec, http.StatusCreated, "")
	kept := decodeBody[educationAnswer](t, rec).ID

	owner := a.dbt.Open(t, db.RoleOwner)
	for _, stmt := range []string{
		"UPDATE universities SET retired_at = now() WHERE id = $1 OR id = $2",
		"UPDATE faculties SET retired_at = now() WHERE university_id = $1 OR university_id = $2",
		"UPDATE departments SET retired_at = now() WHERE faculty_id IN (SELECT id FROM faculties WHERE university_id = $1 OR university_id = $2)",
	} {
		if _, err := owner.Pool().Exec(t.Context(), stmt, ref.university, other.university); err != nil {
			t.Fatal(err)
		}
	}

	// Chosen anew: refused, in a create and in a replace.
	wantStatus(t, a.createEducation(t, tok, educationBody(ref.university, nil, nil, 2015, nil)),
		http.StatusNotFound, string(users.CodeReferenceNotFound))
	wantStatus(t, a.call(t, http.MethodPut, "/v1/me/educations/"+kept.String(), tok,
		educationBody(other.university, nil, nil, 2010, year(2014))), http.StatusNotFound, string(users.CodeReferenceNotFound))
	// Kept: the record's own retired rows stay through a replace.
	rec = a.call(t, http.MethodPut, "/v1/me/educations/"+kept.String(), tok,
		educationBody(ref.university, &ref.faculty, &ref.department, 2011, year(2015)))
	wantStatus(t, rec, http.StatusOK, "")
	if got := decodeBody[educationAnswer](t, rec); got.Department == nil || got.EndYear == nil || *got.EndYear != 2015 {
		t.Fatalf("the kept record = %s", rec.Body)
	}
	// Dropping a retired faculty is a change the record may make.
	wantStatus(t, a.call(t, http.MethodPut, "/v1/me/educations/"+kept.String(), tok,
		educationBody(ref.university, nil, nil, 2011, year(2015))), http.StatusOK, "")

	// Under an active university and faculty: a retired faculty or
	// department of their own cannot be chosen anew, while their active
	// siblings can. GetFacultyOf and GetDepartmentOf apply the rule each
	// on its own.
	live := a.seedReference(t)
	oldFaculty, oldDepartment := uuid.New(), uuid.New()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO faculties (id, university_id, name, retired_at) VALUES ($1, $2, 'Kapanan Fakülte', now())",
			[]any{oldFaculty, live.university}},
		{"INSERT INTO departments (id, faculty_id, degree, name, retired_at) VALUES ($1, $2, 'BACHELOR', 'Kapanan Bölüm', now())",
			[]any{oldDepartment, live.faculty}},
	} {
		if _, err := owner.Pool().Exec(t.Context(), stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	wantStatus(t, a.createEducation(t, tok, educationBody(live.university, &oldFaculty, nil, 2016, nil)),
		http.StatusNotFound, string(users.CodeReferenceNotFound))
	wantStatus(t, a.createEducation(t, tok, educationBody(live.university, &live.faculty, &oldDepartment, 2016, nil)),
		http.StatusNotFound, string(users.CodeReferenceNotFound))
	wantStatus(t, a.createEducation(t, tok, educationBody(live.university, &live.faculty, &live.department, 2016, nil)),
		http.StatusCreated, "")
}

// TestEducationDegreeNamesItsProgram: a record's department is a program of
// the degree the record's degree names (ADR-0038). An associate degree
// takes an associate degree program and is refused a bachelor's program;
// a bachelor's degree the other way round; a master's names its field by
// a bachelor's program. A refusal is REFERENCE_ITEM_NOT_FOUND, in a
// create and in a replace, also for the department the record names
// already.
func TestEducationDegreeNamesItsProgram(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "myo@example.com")
	var (
		thk       = uuid.MustParse("20b200c7-d3d3-4e77-ba67-2b01c96b92a8")
		myo       = uuid.MustParse("01a207cf-0348-7890-bf14-c44956448609") // Ankara Havacılık MYO
		associate = uuid.MustParse("01a207cf-03cc-7a3c-9df7-92d5f5825c58") // Uçak Teknolojisi
	)
	engineering := named(t, a.list(t, "/v1/reference/universities/"+thk.String()+"/faculties"), "Mühendislik Fakültesi")
	bachelor := a.list(t, "/v1/reference/faculties/"+engineering.ID.String()+"/departments")[0]
	body := func(degree string, faculty, department uuid.UUID) string {
		return strings.Replace(educationBody(thk, &faculty, &department, 2018, year(2020)), `"degree":"BACHELOR"`, `"degree":"`+degree+`"`, 1)
	}
	notFound := string(users.CodeReferenceNotFound)

	rec := a.createEducation(t, tok, body("ASSOCIATE", myo, associate))
	wantStatus(t, rec, http.StatusCreated, "")
	created := decodeBody[educationAnswer](t, rec)
	if created.Department == nil || created.Faculty == nil || created.Department.Name != "Uçak Teknolojisi" || created.Faculty.Name != "Ankara Havacılık Meslek Yüksekokulu" {
		t.Fatalf("the associate record = %s", rec.Body)
	}
	wantStatus(t, a.createEducation(t, tok, body("MASTER", engineering.ID, bachelor.ID)), http.StatusCreated, "")
	for _, tc := range []struct {
		degree              string
		faculty, department uuid.UUID
	}{
		{"ASSOCIATE", engineering.ID, bachelor.ID},
		{"BACHELOR", myo, associate},
		{"MASTER", myo, associate},
		{"DOCTORATE", myo, associate},
	} {
		wantStatus(t, a.createEducation(t, tok, body(tc.degree, tc.faculty, tc.department)), http.StatusNotFound, notFound)
		wantStatus(t, a.call(t, http.MethodPut, "/v1/me/educations/"+created.ID.String(), tok,
			body(tc.degree, tc.faculty, tc.department)), http.StatusNotFound, notFound)
	}
	// Without a department the degree binds nothing: a vocational school
	// with a bachelor's degree is the record's own business.
	wantStatus(t, a.call(t, http.MethodPut, "/v1/me/educations/"+created.ID.String(), tok,
		strings.Replace(educationBody(thk, &myo, nil, 2018, year(2020)), `"degree":"BACHELOR"`, `"degree":"ASSOCIATE"`, 1)), http.StatusOK, "")
}
