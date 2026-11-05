package authz_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/authztest"
)

// folderModel exercises every rewrite of the subset:
//
//	type user
//	type group
//	  relations
//	    define member: [user]
//	type folder
//	  relations
//	    define parent: [folder]
//	    define owner: [user]
//	    define viewer: [user, user:*, group#member] or owner or viewer from parent
//	    define banned: [user] or banned from parent
//	    define can_view: viewer but not banned
//	    define can_audit: owner and viewer
const folderModel = `{
  "schema_version": "1.1",
  "type_definitions": [
    {"type": "user"},
    {"type": "group",
     "relations": {"member": {"this": {}}},
     "metadata": {"relations": {"member": {"directly_related_user_types": [{"type": "user"}]}}}},
    {"type": "folder",
     "relations": {
       "parent": {"this": {}},
       "owner": {"this": {}},
       "viewer": {"union": {"child": [
         {"this": {}},
         {"computedUserset": {"relation": "owner"}},
         {"tupleToUserset": {"tupleset": {"relation": "parent"}, "computedUserset": {"relation": "viewer"}}}]}},
       "banned": {"union": {"child": [
         {"this": {}},
         {"tupleToUserset": {"tupleset": {"relation": "parent"}, "computedUserset": {"relation": "banned"}}}]}},
       "can_view": {"difference": {"base": {"computedUserset": {"relation": "viewer"}}, "subtract": {"computedUserset": {"relation": "banned"}}}},
       "can_audit": {"intersection": {"child": [{"computedUserset": {"relation": "owner"}}, {"computedUserset": {"relation": "viewer"}}]}}},
     "metadata": {"relations": {
       "parent": {"directly_related_user_types": [{"type": "folder"}]},
       "owner": {"directly_related_user_types": [{"type": "user"}]},
       "viewer": {"directly_related_user_types": [{"type": "user"}, {"type": "user", "wildcard": {}}, {"type": "group", "relation": "member"}]},
       "banned": {"directly_related_user_types": [{"type": "user"}]},
       "can_view": {},
       "can_audit": {}}}}
  ]
}`

// newEvaluator loads model and tuples written with symbolic user names
// ("user:alice"), which become fixture UUIDs.
func newEvaluator(t *testing.T, model *authz.Model, tuples ...string) (*authz.Evaluator, *countingReader) {
	t.Helper()
	ts := make([]authz.Tuple, 0, len(tuples))
	for _, s := range tuples {
		tp, err := authz.ParseTuple(fixtureRefs(s))
		if err != nil {
			t.Fatal(err)
		}
		ts = append(ts, tp)
	}
	src, err := authztest.NewTuples(model, ts...)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingReader{TupleReader: src}
	return authz.NewEvaluator(model, counted), counted
}

func folderEvaluator(t *testing.T, tuples ...string) *authz.Evaluator {
	t.Helper()
	m, err := authz.LoadModel([]byte(folderModel))
	if err != nil {
		t.Fatal(err)
	}
	ev, _ := newEvaluator(t, m, tuples...)
	return ev
}

// countingReader counts the reads of the source it wraps.
type countingReader struct {
	authz.TupleReader
	reads atomic.Int64
}

func (c *countingReader) Contains(ctx context.Context, o authz.Object, relation string, u authz.TupleUser) (bool, error) {
	c.reads.Add(1)
	return c.TupleReader.Contains(ctx, o, relation, u)
}

func (c *countingReader) Read(ctx context.Context, o authz.Object, relation string) ([]authz.TupleUser, error) {
	c.reads.Add(1)
	return c.TupleReader.Read(ctx, o, relation)
}

// fixtureRefs maps every "user:<name>" and "staff:<name>" in a tuple
// string to its fixture UUID.
func fixtureRefs(s string) string {
	parts := strings.Split(s, "@")
	for i, p := range parts {
		ref, rel, hasRel := strings.Cut(p, "#")
		if i == 0 {
			parts[i] = fixtureRef(ref) + "#" + rel
			continue
		}
		parts[i] = fixtureRef(ref)
		if name, ok := strings.CutPrefix(ref, "staff:"); ok {
			parts[i] = "staff:" + fixtureUserID(name).String()
		}
		if hasRel {
			parts[i] += "#" + rel
		}
	}
	return strings.Join(parts, "@")
}

// readable replaces fixture UUIDs in a basis with their names.
func readable(b authz.Basis, names ...string) string {
	s := b.String()
	for _, n := range names {
		s = strings.ReplaceAll(s, fixtureUserID(n).String(), n)
	}
	return s
}

func alice() authz.Subject { return authz.User(fixtureUserID("alice")) }

func TestEvaluatorRewrites(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		tuples   []string
		relation authz.Action
		allowed  bool
		basis    string
	}{
		{"direct", []string{"folder:f#viewer@user:alice"}, "viewer", true, "folder:f#viewer@user:alice"},
		{"no tuple", nil, "viewer", false, ""},
		{"other user", []string{"folder:f#viewer@user:bob"}, "viewer", false, ""},
		{"wildcard", []string{"folder:f#viewer@user:*"}, "viewer", true, "folder:f#viewer@user:*"},
		{"userset", []string{"folder:f#viewer@group:g#member", "group:g#member@user:alice"}, "viewer", true, "folder:f#viewer > group:g#member@user:alice"},
		{"userset without membership", []string{"folder:f#viewer@group:g#member", "group:g#member@user:bob"}, "viewer", false, ""},
		{"usersets in stable order", []string{"folder:f#viewer@group:z#member", "folder:f#viewer@group:a#member", "group:z#member@user:alice", "group:a#member@user:alice"}, "viewer", true, "folder:f#viewer > group:a#member@user:alice"},
		{"computed", []string{"folder:f#owner@user:alice"}, "viewer", true, "folder:f#viewer > folder:f#owner@user:alice"},
		{"tuple to userset", []string{"folder:f#parent@folder:p", "folder:p#viewer@user:alice"}, "viewer", true, "folder:f#viewer > folder:p#viewer@user:alice"},
		{"two levels up", []string{"folder:f#parent@folder:p", "folder:p#parent@folder:q", "folder:q#owner@user:alice"}, "viewer", true, "folder:f#viewer > folder:p#viewer > folder:q#viewer > folder:q#owner@user:alice"},
		{"difference allows", []string{"folder:f#viewer@user:alice"}, "can_view", true, "folder:f#can_view > folder:f#viewer@user:alice"},
		{"difference excludes", []string{"folder:f#viewer@user:alice", "folder:f#banned@user:alice"}, "can_view", false, ""},
		{"difference excludes inherited", []string{"folder:f#viewer@user:alice", "folder:f#parent@folder:p", "folder:p#banned@user:alice"}, "can_view", false, ""},
		{"difference excludes wildcard viewer", []string{"folder:f#viewer@user:*", "folder:f#banned@user:alice"}, "can_view", false, ""},
		// OpenFGA's CycleDetected: a cycle under the subtract side cannot
		// prove the exclusion false, so the check denies.
		{"cycle under but not denies", []string{"folder:f#viewer@user:alice", "folder:f#parent@folder:p", "folder:p#parent@folder:f"}, "can_view", false, ""},
		{"cycle under the base still grants", []string{"folder:f#viewer@user:alice", "folder:f#parent@folder:p", "folder:p#parent@folder:f"}, "viewer", true, "folder:f#viewer@user:alice"},
		{"intersection reports every path", []string{"folder:f#owner@user:alice"}, "can_audit", true, "folder:f#can_audit > folder:f#owner@user:alice & folder:f#can_audit > folder:f#viewer > folder:f#owner@user:alice"},
		{"intersection one", []string{"folder:f#viewer@user:alice"}, "can_audit", false, ""},
		{"cycle", []string{"folder:f#parent@folder:p", "folder:p#parent@folder:f"}, "viewer", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := folderEvaluator(t, tt.tuples...)
			for range 2 { // the same data gives the same answer
				d, err := ev.Check(context.Background(), alice(), tt.relation, authz.Object{Type: "folder", ID: "f"})
				if err != nil {
					t.Fatal(err)
				}
				if got := readable(d.Basis, "alice"); d.Allowed != tt.allowed || got != tt.basis {
					t.Fatalf("got allowed=%v basis=%q, want %v %q", d.Allowed, got, tt.allowed, tt.basis)
				}
				if d.Allowed == d.Conceal {
					t.Fatalf("Conceal %v for allowed %v; the folder model has no conceal policy, so every denial conceals", d.Conceal, d.Allowed)
				}
			}
		})
	}
}

// TestEvaluatorDepthLimit pins the limit's exact value: 24 nested lookups
// below the asked relation resolve, the 25th fails (OpenFGA's 25).
func TestEvaluatorDepthLimit(t *testing.T) {
	t.Parallel()
	// chain(n): f0 -> f1 -> ... -> fn by parent, alice owns fn. Resolving
	// f0#viewer looks up f1#viewer ... fn#viewer, then fn#owner: n+1
	// nested lookups.
	chain := func(n int) []string {
		var ts []string
		for i := range n {
			ts = append(ts, fmt.Sprintf("folder:f%d#parent@folder:f%d", i, i+1))
		}
		return append(ts, fmt.Sprintf("folder:f%d#owner@user:alice", n))
	}
	top := authz.Object{Type: "folder", ID: "f0"}
	deepest := authz.MaxResolutionDepth - 2 // 24 nested lookups

	d, err := folderEvaluator(t, chain(deepest)...).Check(context.Background(), alice(), "viewer", top)
	if err != nil || !d.Allowed {
		t.Fatalf("%d nested lookups: %+v, %v; want allowed", deepest+1, d, err)
	}
	d, err = folderEvaluator(t, chain(deepest+1)...).Check(context.Background(), alice(), "viewer", top)
	if !errors.Is(err, authz.ErrResolutionTooComplex) || d.Allowed || !d.Conceal {
		t.Fatalf("%d nested lookups: %+v, %v; want a concealed denial with ErrResolutionTooComplex", deepest+2, d, err)
	}
}

// TestStaffRoleInclusion: assignee from includer resolves through the
// role DAG and the basis names the roles.
func TestStaffRoleInclusion(t *testing.T) {
	t.Parallel()
	model := loadModel(t)
	ev, _ := newEvaluator(t, model,
		"staff_role:junior#includer@staff_role:senior", // senior includes junior
		"staff_role:senior#includer@staff_role:lead",   // lead includes senior
		"staff_role:lead#assignee@staff:s1",
		"staff_role:a#includer@staff_role:b", // a cycle with no assignee
		"staff_role:b#includer@staff_role:a",
		"platform:koala#payout_release@staff_role:junior#assignee",
		"platform:koala#payment_refund@staff_role:a#assignee",
	)
	koala := authz.Object{Type: authz.TypePlatform, ID: "koala"}
	staff := func(name string) authz.Subject { return authz.Staff(fixtureUserID(name)) }

	d, err := ev.Check(context.Background(), staff("s1"), authz.ActionPlatformPayoutRelease, koala)
	if err != nil || !d.Allowed {
		t.Fatalf("lead through senior and junior: %+v, %v", d, err)
	}
	want := "platform:koala#payout_release > staff_role:junior#assignee > staff_role:senior#assignee > staff_role:lead#assignee@staff:s1"
	if got := readable(d.Basis, "s1"); got != want {
		t.Errorf("basis %q, want %q", got, want)
	}
	for _, tc := range []struct {
		name string
		s    authz.Subject
		a    authz.Action
	}{
		{"unassigned staff", staff("s2"), authz.ActionPlatformPayoutRelease},
		{"role cycle", staff("s1"), authz.ActionPlatformPaymentRefund},
		{"a user is not staff", authz.User(fixtureUserID("s1")), authz.ActionPlatformPayoutRelease},
	} {
		d, err := ev.Check(context.Background(), tc.s, tc.a, koala)
		if err != nil || d.Allowed {
			t.Errorf("%s: %+v, %v; want denied", tc.name, d, err)
		}
	}
}

// TestEvaluatorDiamondIsResolvedOnce: a role reached through two others
// is resolved once per check, so a layered diamond costs reads linear in
// its size, not exponential.
func TestEvaluatorDiamondIsResolvedOnce(t *testing.T) {
	t.Parallel()
	const levels = 11 // 2^11 paths without memoization; depth 22 < 25
	var tuples []string
	for i := range levels {
		for _, side := range []string{"a", "b"} {
			mid := fmt.Sprintf("%s%d", side, i)
			tuples = append(tuples,
				fmt.Sprintf("staff_role:r%d#includer@staff_role:%s", i, mid),
				fmt.Sprintf("staff_role:%s#includer@staff_role:r%d", mid, i+1))
		}
	}
	tuples = append(tuples, "platform:koala#audit_read@staff_role:r0#assignee")
	ev, reads := newEvaluator(t, loadModel(t), tuples...)
	d, err := ev.Check(context.Background(), authz.Staff(fixtureUserID("nobody")), authz.ActionPlatformAuditRead, authz.Object{Type: authz.TypePlatform, ID: "koala"})
	if err != nil || d.Allowed {
		t.Fatalf("%+v, %v; want denied", d, err)
	}
	if n := reads.reads.Load(); n > 10*levels {
		t.Fatalf("%d reads for %d levels; the diamond is not memoized", n, levels)
	}
}

func TestEvaluatorRefuses(t *testing.T) {
	t.Parallel()
	ev := folderEvaluator(t, "folder:f#viewer@user:*")
	f := authz.Object{Type: "folder", ID: "f"}
	tests := []struct {
		name string
		s    authz.Subject
		a    authz.Action
		o    authz.Object
		want error
	}{
		{"zero subject", authz.Subject{}, "viewer", f, authz.ErrInvalidSubject},
		{"system subject", authz.Subject{Kind: authz.SubjectSystem, ID: uuid.New()}, "viewer", f, authz.ErrInvalidSubject},
		{"unknown relation", alice(), "editor", f, authz.ErrUnknownRelation},
		{"unknown type", alice(), "viewer", authz.Object{Type: "drive", ID: "d"}, authz.ErrUnknownRelation},
		{"empty id", alice(), "viewer", authz.Object{Type: "folder"}, authz.ErrInvalidObject},
		{"wildcard object", alice(), "viewer", authz.Object{Type: "folder", ID: "*"}, authz.ErrInvalidObject},
		{"id with a space", alice(), "viewer", authz.Object{Type: "folder", ID: "a b"}, authz.ErrInvalidObject},
		{"id with a hash", alice(), "viewer", authz.Object{Type: "folder", ID: "a#b"}, authz.ErrInvalidObject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, err := ev.Check(context.Background(), tt.s, tt.a, tt.o)
			if !errors.Is(err, tt.want) || d.Allowed || !d.Conceal {
				t.Fatalf("got %+v, %v; want a concealed denial with %v", d, err, tt.want)
			}
		})
	}
}

// failingReader fails every read.
type failingReader struct{}

var errRead = errors.New("read failed")

func (failingReader) Contains(context.Context, authz.Object, string, authz.TupleUser) (bool, error) {
	return false, errRead
}

func (failingReader) Read(context.Context, authz.Object, string) ([]authz.TupleUser, error) {
	return nil, errRead
}

func TestEvaluatorReaderErrorDenies(t *testing.T) {
	t.Parallel()
	m, err := authz.LoadModel([]byte(folderModel))
	if err != nil {
		t.Fatal(err)
	}
	ev := authz.NewEvaluator(m, failingReader{})
	d, err := ev.Check(context.Background(), alice(), "viewer", authz.Object{Type: "folder", ID: "f"})
	if !errors.Is(err, errRead) || d.Allowed || !d.Conceal {
		t.Fatalf("got %+v, %v; want a concealed denial wrapping the read error", d, err)
	}
}

// staleReader returns tuples the model's restrictions no longer allow, as
// left behind by an older model.
type staleReader struct{}

func (staleReader) Contains(_ context.Context, _ authz.Object, relation string, u authz.TupleUser) (bool, error) {
	return relation == "banned" && u.IsWildcard(), nil
}

func (staleReader) Read(context.Context, authz.Object, string) ([]authz.TupleUser, error) {
	return []authz.TupleUser{{Type: "folder", ID: "x", Relation: "owner"}}, nil
}

func TestEvaluatorIgnoresTuplesOutsideRestrictions(t *testing.T) {
	t.Parallel()
	m, err := authz.LoadModel([]byte(folderModel))
	if err != nil {
		t.Fatal(err)
	}
	ev := authz.NewEvaluator(m, staleReader{})
	for _, rel := range []authz.Action{"viewer", "banned"} {
		d, err := ev.Check(context.Background(), alice(), rel, authz.Object{Type: "folder", ID: "f"})
		if err != nil || d.Allowed {
			t.Errorf("%s: %+v, %v; want denied", rel, d, err)
		}
	}
}

func TestCheckBatchAndOptions(t *testing.T) {
	t.Parallel()
	ev := folderEvaluator(t, "folder:a#viewer@user:alice")
	a, b := authz.Object{Type: "folder", ID: "a"}, authz.Object{Type: "folder", ID: "b"}
	got, err := ev.CheckBatch(context.Background(), alice(), "viewer", []authz.Object{a, b}, authz.AtLeastAsFresh("token"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[a].Allowed || got[b].Allowed {
		t.Fatalf("CheckBatch: %+v", got)
	}
	if _, err := ev.CheckBatch(context.Background(), alice(), "nope", []authz.Object{a}); !errors.Is(err, authz.ErrUnknownRelation) {
		t.Fatalf("CheckBatch with an unknown relation: %v", err)
	}
}

func TestNewTuplesValidatesLikeAnOpenFGAWrite(t *testing.T) {
	t.Parallel()
	m, err := authz.LoadModel([]byte(folderModel))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"folder:f#owner@user:*",          // owner does not allow a wildcard
		"folder:f#owner@group:g#member",  // nor a userset
		"folder:f#can_view@user:alice",   // computed relation, no direct assignment
		"folder:f#nope@user:alice",       // undefined relation
		"drive:d#owner@user:alice",       // undefined type
		"folder:f#parent@user:alice",     // parent must be a folder
		"folder:f#viewer@group:g#viewer", // userset relation not allowed
	} {
		tp, err := authz.ParseTuple(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if _, err := authztest.NewTuples(m, tp); !errors.Is(err, authz.ErrModel) {
			t.Errorf("%s: %v, want ErrModel", s, err)
		}
	}
	tp, err := authz.ParseTuple("folder:f#owner@user:alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authztest.NewTuples(m, tp, tp); err == nil {
		t.Errorf("duplicate tuple accepted")
	}
}

func TestParseTuple(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"doc:1#viewer@user:2", "doc:1#viewer@user:*", "doc:1#viewer@group:g#member", "doc:a.b-c_d#viewer@user:x|y"} {
		tp, err := authz.ParseTuple(s)
		if err != nil || tp.String() != s {
			t.Errorf("ParseTuple(%q) = %v, %v", s, tp, err)
		}
	}
	for _, s := range []string{
		"", "doc:1#viewer", "doc#viewer@user:2", "doc:1@user:2", "doc:1#@user:2", "doc:1#viewer@user",
		"doc:1#viewer@user:*#member", "doc:1#viewer@group:g#", "doc:*#viewer@user:2", "doc:1#vie wer@user:2",
		"do*c:1#viewer@user:2", "doc:1#viewer@user:a b", "doc:" + strings.Repeat("x", 256) + "#viewer@user:2",
	} {
		if _, err := authz.ParseTuple(s); err == nil {
			t.Errorf("ParseTuple(%q) accepted", s)
		}
	}
}

func TestSubjectString(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("0e6f4a42-5b1c-4c39-8f0e-2d6a3b7c9e11")
	if got := authz.User(id).String(); got != "user:"+id.String() {
		t.Errorf("User: %q", got)
	}
	if got := authz.Staff(id).String(); got != "staff:"+id.String() {
		t.Errorf("Staff: %q", got)
	}
	if got := (authz.Subject{Kind: authz.SubjectSystem, ID: id}).String(); got != "" {
		t.Errorf("System: %q", got)
	}
}
