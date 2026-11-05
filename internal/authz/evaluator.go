package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// MaxResolutionDepth is the deepest chain of relation lookups one check
// may follow. It is OpenFGA's default (OPENFGA_RESOLVE_NODE_LIMIT, 25), so
// a model that resolves locally resolves on OpenFGA too.
const MaxResolutionDepth = 25

// ErrResolutionTooComplex is returned when a check needs more than
// MaxResolutionDepth nested lookups. The decision is a denial.
var ErrResolutionTooComplex = errors.New("authz: resolution too complex")

// ErrUnknownRelation is returned for a check on a type or relation the
// model does not define: a programming error, denied and concealed.
var ErrUnknownRelation = errors.New("authz: unknown type or relation")

// ErrInvalidSubject is returned for a subject without a model type
// (SubjectInvalid, SubjectSystem).
var ErrInvalidSubject = errors.New("authz: subject has no model type")

// Evaluator is the local Checker: it interprets the model over a
// TupleReader. It is safe for concurrent use when the reader is.
type Evaluator struct {
	model  *Model
	tuples TupleReader
}

// NewEvaluator returns an evaluator for model reading tuples from r.
func NewEvaluator(model *Model, r TupleReader) *Evaluator {
	return &Evaluator{model: model, tuples: r}
}

var _ Checker = (*Evaluator)(nil)

// Check decides whether s may perform a on o. Errors deny: the returned
// decision is then a concealed denial.
func (e *Evaluator) Check(ctx context.Context, s Subject, a Action, o Object, opts ...CheckOption) (Decision, error) {
	var co checkOptions
	for _, opt := range opts {
		opt(&co)
	}
	// co.atLeastAsFresh is ignored: reads happen in the caller's view of
	// the data (see AtLeastAsFresh).
	denied := Decision{Conceal: true}
	user := TupleUser{Type: s.ModelType(), ID: s.ID.String()}
	if user.Type == "" {
		return denied, fmt.Errorf("check %s on %s: %w", a, o, ErrInvalidSubject)
	}
	if e.model.relation(o.Type, string(a)) == nil || o.ID == "" || o.ID == Wildcard {
		return denied, fmt.Errorf("check %s on %s: %w", a, o, ErrUnknownRelation)
	}
	r := resolver{e: e, user: user, visiting: map[string]bool{}}
	ok, path, err := r.check(ctx, o, string(a), 0)
	if err != nil {
		return denied, fmt.Errorf("check %s on %s: %w", a, o, err)
	}
	if ok {
		return Decision{Allowed: true, Basis: strings.Join(path, " > ")}, nil
	}
	conceal, err := e.conceal(ctx, &r, o, a)
	if err != nil {
		return denied, fmt.Errorf("check %s on %s: %w", a, o, err)
	}
	return Decision{Conceal: conceal}, nil
}

// CheckBatch checks one action on several objects. The first error stops
// the batch.
func (e *Evaluator) CheckBatch(ctx context.Context, s Subject, a Action, objs []Object, opts ...CheckOption) (map[Object]Decision, error) {
	out := make(map[Object]Decision, len(objs))
	for _, o := range objs {
		d, err := e.Check(ctx, s, a, o, opts...)
		if err != nil {
			return nil, err
		}
		out[o] = d
	}
	return out, nil
}

// conceal applies the concealment policy to a denial.
func (e *Evaluator) conceal(ctx context.Context, r *resolver, o Object, a Action) (bool, error) {
	visibility, ok := concealUnless(o.Type, a)
	if !ok {
		return true, nil
	}
	visible, _, err := r.check(ctx, o, string(visibility), 0)
	if err != nil {
		return true, err
	}
	return !visible, nil
}

// resolver evaluates one check: the subject is fixed, visiting detects
// cycles along the current path.
type resolver struct {
	e        *Evaluator
	user     TupleUser
	visiting map[string]bool
}

// check reports whether r.user has relation on o, with the path of
// relations that granted it.
func (r *resolver) check(ctx context.Context, o Object, relation string, depth int) (bool, []string, error) {
	if depth > MaxResolutionDepth {
		return false, nil, ErrResolutionTooComplex
	}
	if err := ctx.Err(); err != nil {
		return false, nil, fmt.Errorf("authz: %w", err)
	}
	rd := r.e.model.relation(o.Type, relation)
	if rd == nil {
		return false, nil, nil
	}
	key := o.String() + "#" + relation
	if r.visiting[key] {
		// A cycle grants nothing, as in OpenFGA.
		return false, nil, nil
	}
	r.visiting[key] = true
	defer delete(r.visiting, key)
	return r.rewrite(ctx, o, rd, rd.rewrite, depth)
}

func (r *resolver) rewrite(ctx context.Context, o Object, rd *relationDef, rw *rewrite, depth int) (bool, []string, error) {
	here := rd.typeName + "#" + rd.name
	switch rw.kind {
	case rewriteThis:
		return r.direct(ctx, o, rd, depth)
	case rewriteComputed:
		ok, path, err := r.check(ctx, o, rw.relation, depth+1)
		return ok, prepend(here, path, ok), err
	case rewriteTupleToUserset:
		parents, err := r.e.tuples.Read(ctx, o, rw.tupleset)
		if err != nil {
			return false, nil, fmt.Errorf("authz: read %s#%s: %w", o, rw.tupleset, err)
		}
		var firstErr error
		for _, p := range parents {
			if !p.IsObject() || !rd.tuplesetAllows(r.e.model, rw.tupleset, p) {
				continue
			}
			ok, path, err := r.check(ctx, Object{Type: p.Type, ID: p.ID}, rw.relation, depth+1)
			if ok {
				return true, prepend(here, path, true), nil
			}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return false, nil, firstErr
	case rewriteUnion:
		var firstErr error
		for _, c := range rw.children {
			ok, path, err := r.rewrite(ctx, o, rd, c, depth)
			if ok {
				return true, path, nil
			}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return false, nil, firstErr
	case rewriteIntersection:
		var basis []string
		for _, c := range rw.children {
			ok, path, err := r.rewrite(ctx, o, rd, c, depth)
			if err != nil || !ok {
				return false, nil, err
			}
			if basis == nil {
				basis = path
			}
		}
		return true, basis, nil
	case rewriteDifference:
		ok, path, err := r.rewrite(ctx, o, rd, rw.children[0], depth)
		if err != nil || !ok {
			return false, nil, err
		}
		excluded, _, err := r.rewrite(ctx, o, rd, rw.children[1], depth)
		if err != nil || excluded {
			return false, nil, err
		}
		return true, path, nil
	}
	return false, nil, fmt.Errorf("%w: unknown rewrite", ErrModel)
}

// direct resolves the direct assignment of rd on o: an exact tuple, a
// wildcard tuple, or a userset tuple whose userset contains the subject.
// Only tuples the relation's type restrictions allow count, as in
// OpenFGA, so a tuple left over from an older model grants nothing.
func (r *resolver) direct(ctx context.Context, o Object, rd *relationDef, depth int) (bool, []string, error) {
	here := rd.typeName + "#" + rd.name
	usersets := false
	for _, ref := range rd.directTypes {
		switch {
		case ref.Relation != "":
			usersets = true
		case ref.Type != r.user.Type:
			// A restriction for another subject type cannot match.
		case ref.Wildcard:
			ok, err := r.e.tuples.Contains(ctx, o, rd.name, TupleUser{Type: ref.Type, ID: Wildcard})
			if err != nil {
				return false, nil, fmt.Errorf("authz: read %s#%s: %w", o, rd.name, err)
			}
			if ok {
				return true, []string{here + " [" + ref.Type + ":" + Wildcard + "]"}, nil
			}
		default:
			ok, err := r.e.tuples.Contains(ctx, o, rd.name, r.user)
			if err != nil {
				return false, nil, fmt.Errorf("authz: read %s#%s: %w", o, rd.name, err)
			}
			if ok {
				return true, []string{here}, nil
			}
		}
	}
	if !usersets {
		return false, nil, nil
	}
	users, err := r.e.tuples.Read(ctx, o, rd.name)
	if err != nil {
		return false, nil, fmt.Errorf("authz: read %s#%s: %w", o, rd.name, err)
	}
	var firstErr error
	for _, u := range users {
		if u.Relation == "" || !rd.allowsUserset(u) {
			continue
		}
		ok, path, err := r.check(ctx, Object{Type: u.Type, ID: u.ID}, u.Relation, depth+1)
		if ok {
			return true, prepend(here, path, true), nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return false, nil, firstErr
}

func (rd *relationDef) allowsUserset(u TupleUser) bool {
	for _, ref := range rd.directTypes {
		if ref.Relation != "" && ref.allows(u) {
			return true
		}
	}
	return false
}

// tuplesetAllows reports whether p is an allowed parent for the tupleset
// relation of rd's type.
func (rd *relationDef) tuplesetAllows(m *Model, tupleset string, p TupleUser) bool {
	ts := m.relation(rd.typeName, tupleset)
	if ts == nil {
		return false
	}
	for _, ref := range ts.directTypes {
		if ref.allows(p) {
			return true
		}
	}
	return false
}

func prepend(here string, path []string, ok bool) []string {
	if !ok {
		return nil
	}
	return append([]string{here}, path...)
}
