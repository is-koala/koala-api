package authz_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
//	    define banned: [user]
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
       "banned": {"this": {}},
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

func folderEvaluator(t *testing.T, tuples ...string) *authz.Evaluator {
	t.Helper()
	m, err := authz.LoadModel([]byte(folderModel))
	if err != nil {
		t.Fatal(err)
	}
	ts := make([]authz.Tuple, 0, len(tuples))
	for _, s := range tuples {
		tp, err := authz.ParseTuple(fixtureRefs(s))
		if err != nil {
			t.Fatal(err)
		}
		ts = append(ts, tp)
	}
	src, err := authztest.NewTuples(m, ts...)
	if err != nil {
		t.Fatal(err)
	}
	return authz.NewEvaluator(m, src)
}

// fixtureRefs maps every "user:<name>" in a tuple string to its UUID.
func fixtureRefs(s string) string {
	parts := strings.Split(s, "@")
	for i, p := range parts {
		ref, rel, hasRel := strings.Cut(p, "#")
		if i == 0 {
			parts[i] = fixtureRef(ref) + "#" + rel
			continue
		}
		parts[i] = fixtureRef(ref)
		if hasRel {
			parts[i] += "#" + rel
		}
	}
	return strings.Join(parts, "@")
}

func alice() authz.Subject { return authz.User(fixtureUserID("alice")) }

func TestEvaluatorRewrites(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		tuples   []string
		relation string
		allowed  bool
		basis    string
	}{
		{"direct", []string{"folder:f#viewer@user:alice"}, "viewer", true, "folder#viewer"},
		{"no tuple", nil, "viewer", false, ""},
		{"other user", []string{"folder:f#viewer@user:bob"}, "viewer", false, ""},
		{"wildcard", []string{"folder:f#viewer@user:*"}, "viewer", true, "folder#viewer [user:*]"},
		{"userset", []string{"folder:f#viewer@group:g#member", "group:g#member@user:alice"}, "viewer", true, "folder#viewer > group#member"},
		{"userset without membership", []string{"folder:f#viewer@group:g#member", "group:g#member@user:bob"}, "viewer", false, ""},
		{"computed", []string{"folder:f#owner@user:alice"}, "viewer", true, "folder#viewer > folder#owner"},
		{"tuple to userset", []string{"folder:f#parent@folder:p", "folder:p#viewer@user:alice"}, "viewer", true, "folder#viewer > folder#viewer"},
		{"two levels up", []string{"folder:f#parent@folder:p", "folder:p#parent@folder:q", "folder:q#owner@user:alice"}, "viewer", true, "folder#viewer > folder#viewer > folder#viewer > folder#owner"},
		{"difference allows", []string{"folder:f#viewer@user:alice"}, "can_view", true, "folder#can_view > folder#viewer"},
		{"difference excludes", []string{"folder:f#viewer@user:alice", "folder:f#banned@user:alice"}, "can_view", false, ""},
		{"difference excludes wildcard viewer", []string{"folder:f#viewer@user:*", "folder:f#banned@user:alice"}, "can_view", false, ""},
		{"intersection both", []string{"folder:f#owner@user:alice"}, "can_audit", true, "folder#can_audit > folder#owner"},
		{"intersection one", []string{"folder:f#viewer@user:alice"}, "can_audit", false, ""},
		{"cycle", []string{"folder:f#parent@folder:p", "folder:p#parent@folder:f"}, "viewer", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := folderEvaluator(t, tt.tuples...)
			d, err := ev.Check(context.Background(), alice(), authz.Action(tt.relation), authz.Object{Type: "folder", ID: "f"})
			if err != nil {
				t.Fatal(err)
			}
			if d.Allowed != tt.allowed || d.Basis != tt.basis {
				t.Fatalf("got allowed=%v basis=%q, want %v %q", d.Allowed, d.Basis, tt.allowed, tt.basis)
			}
			if d.Allowed == d.Conceal {
				t.Fatalf("Conceal %v for allowed %v; the folder model has no conceal policy, so every denial conceals", d.Conceal, d.Allowed)
			}
		})
	}
}

func TestEvaluatorDepthLimit(t *testing.T) {
	t.Parallel()
	chain := func(n int) []string {
		var ts []string
		for i := range n {
			ts = append(ts, fmt.Sprintf("folder:f%d#parent@folder:f%d", i, i+1))
		}
		return append(ts, fmt.Sprintf("folder:f%d#owner@user:alice", n))
	}
	top := authz.Object{Type: "folder", ID: "f0"}

	d, err := folderEvaluator(t, chain(10)...).Check(context.Background(), alice(), "viewer", top)
	if err != nil || !d.Allowed {
		t.Fatalf("10 levels: %+v, %v; want allowed", d, err)
	}
	d, err = folderEvaluator(t, chain(authz.MaxResolutionDepth+5)...).Check(context.Background(), alice(), "viewer", top)
	if !errors.Is(err, authz.ErrResolutionTooComplex) || d.Allowed || !d.Conceal {
		t.Fatalf("too deep: %+v, %v; want a concealed denial with ErrResolutionTooComplex", d, err)
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
		{"empty id", alice(), "viewer", authz.Object{Type: "folder"}, authz.ErrUnknownRelation},
		{"wildcard object", alice(), "viewer", authz.Object{Type: "folder", ID: "*"}, authz.ErrUnknownRelation},
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

// staleReader returns a tuple the model's restrictions no longer allow,
// as left behind by an older model.
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
		"folder:*#viewer@user:alice",     // wildcard object
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
	for _, s := range []string{"doc:1#viewer@user:2", "doc:1#viewer@user:*", "doc:1#viewer@group:g#member"} {
		tp, err := authz.ParseTuple(s)
		if err != nil || tp.String() != s {
			t.Errorf("ParseTuple(%q) = %v, %v", s, tp, err)
		}
	}
	for _, s := range []string{"", "doc:1#viewer", "doc#viewer@user:2", "doc:1@user:2", "doc:1#@user:2", "doc:1#viewer@user", "doc:1#viewer@user:*#member", "doc:1#viewer@group:g#"} {
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
