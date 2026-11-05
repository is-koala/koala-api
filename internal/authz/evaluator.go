package authz

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
)

// MaxResolutionDepth is the local resolution limit, taken from OpenFGA's
// default (OPENFGA_RESOLVE_NODE_LIMIT). Every lookup of a further
// object#relation (computed userset, tuple-to-userset, userset tuple) is
// one level below the asked relation; a check may go 24 levels deep and
// the 25th fails with ErrResolutionTooComplex. It is stricter than an
// OpenFGA server, which resolves recursive "from" chains past it
// (ADR-0024 §4; pinned in the conformance runner).
const MaxResolutionDepth = 25

// ErrResolutionTooComplex is returned when a check needs more than
// MaxResolutionDepth-1 nested lookups. The decision is a denial.
var ErrResolutionTooComplex = errors.New("authz: resolution too complex")

// ErrUnknownRelation is returned for a check on a type or relation the
// model does not define: a programming error, denied and concealed.
var ErrUnknownRelation = errors.New("authz: unknown type or relation")

// ErrInvalidSubject is returned for a subject without a model type
// (SubjectInvalid, SubjectSystem).
var ErrInvalidSubject = errors.New("authz: subject has no model type")

// ErrInvalidObject is returned for an object that breaks OpenFGA's
// identifier rules (a wildcard, an empty or malformed id).
var ErrInvalidObject = errors.New("authz: invalid object")

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
	// co.atLeastAsFresh is not used: reads happen in the caller's view of
	// the data (see AtLeastAsFresh).
	denied := Decision{Conceal: true}
	user := TupleUser{Type: s.ModelType(), ID: s.ID.String()}
	if user.Type == "" {
		return denied, fmt.Errorf("check %s on %s: %w", a, o, ErrInvalidSubject)
	}
	if !o.Valid() {
		return denied, fmt.Errorf("check %s on %s: %w", a, o, ErrInvalidObject)
	}
	if e.model.relation(o.Type, string(a)) == nil {
		return denied, fmt.Errorf("check %s on %s: %w", a, o, ErrUnknownRelation)
	}
	r := &resolver{e: e, user: user, visiting: map[string]bool{}, done: map[string]outcome{}}
	out, err := r.check(ctx, o, string(a), 0)
	if err != nil {
		return denied, fmt.Errorf("check %s on %s: %w", a, o, err)
	}
	if out.allowed {
		return Decision{Allowed: true, Basis: out.paths}, nil
	}
	conceal, err := r.conceal(ctx, o, a)
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

// outcome is the result of resolving one rewrite.
type outcome struct {
	allowed bool
	// cycle: not allowed because the resolution met a relation already on
	// its own path. A cycle denies wherever it appears, including under
	// the subtract side of "but not", where an OpenFGA server allows
	// (fail-closed on purpose; ADR-0024 §4).
	cycle bool
	paths Basis
}

// resolver evaluates one check for a fixed subject. visiting holds the
// object#relation keys on the current path (cycle detection); done holds
// finished results without a cycle, so a diamond in the relation graph
// (a role included through two others) is resolved once per check.
type resolver struct {
	e        *Evaluator
	user     TupleUser
	visiting map[string]bool
	done     map[string]outcome
}

// conceal applies the concealment policy to a denial.
func (r *resolver) conceal(ctx context.Context, o Object, a Action) (bool, error) {
	visibility, ok := concealUnless(o.Type, a)
	if !ok {
		return true, nil
	}
	out, err := r.check(ctx, o, string(visibility), 0)
	if err != nil {
		return true, err
	}
	return !out.allowed, nil
}

// check resolves relation on o at the given depth.
func (r *resolver) check(ctx context.Context, o Object, relation string, depth int) (outcome, error) {
	if depth >= MaxResolutionDepth {
		return outcome{}, ErrResolutionTooComplex
	}
	if err := ctx.Err(); err != nil {
		return outcome{}, fmt.Errorf("authz: %w", err)
	}
	rd := r.e.model.relation(o.Type, relation)
	if rd == nil {
		return outcome{}, nil
	}
	key := o.String() + "#" + relation
	if out, ok := r.done[key]; ok {
		return out, nil
	}
	if r.visiting[key] {
		return outcome{cycle: true}, nil
	}
	r.visiting[key] = true
	out, err := r.rewrite(ctx, o, rd, rd.rewrite, depth)
	delete(r.visiting, key)
	if err == nil && !out.cycle {
		r.done[key] = out
	}
	return out, err
}

func (r *resolver) rewrite(ctx context.Context, o Object, rd *relationDef, rw *rewrite, depth int) (outcome, error) {
	here := Step{Object: o, Relation: rd.name}
	switch rw.kind {
	case rewriteThis:
		return r.direct(ctx, o, rd, depth)
	case rewriteComputed:
		out, err := r.check(ctx, o, rw.relation, depth+1)
		return out.through(here), err
	case rewriteTupleToUserset:
		parents, err := r.read(ctx, o, rw.tupleset)
		if err != nil {
			return outcome{}, err
		}
		var acc anyOf
		for _, p := range parents {
			if !p.IsObject() || !rd.tuplesetAllows(r.e.model, rw.tupleset, p) {
				continue
			}
			out, err := r.check(ctx, Object{Type: p.Type, ID: p.ID}, rw.relation, depth+1)
			if acc.add(out.through(here), err) {
				break
			}
		}
		return acc.result()
	case rewriteUnion:
		var acc anyOf
		for _, c := range rw.children {
			out, err := r.rewrite(ctx, o, rd, c, depth)
			if acc.add(out, err) {
				break
			}
		}
		return acc.result()
	case rewriteIntersection:
		var paths Basis
		cycle := false
		for _, c := range rw.children {
			out, err := r.rewrite(ctx, o, rd, c, depth)
			switch {
			case err != nil:
				return outcome{}, err
			case out.cycle:
				cycle = true
			case !out.allowed:
				return outcome{}, nil
			default:
				// Every operand holds: report every path.
				paths = append(paths, out.paths...)
			}
		}
		if cycle {
			return outcome{cycle: true}, nil
		}
		return outcome{allowed: true, paths: paths}, nil
	case rewriteDifference:
		base, err := r.rewrite(ctx, o, rd, rw.children[0], depth)
		if err != nil || !base.allowed {
			return outcome{cycle: base.cycle}, err
		}
		sub, err := r.rewrite(ctx, o, rd, rw.children[1], depth)
		switch {
		case err != nil:
			return outcome{}, err
		case sub.cycle:
			// The exclusion cannot be proved false: deny (fail-closed;
			// OpenFGA v1.22 allows here, ADR-0024 §4).
			return outcome{cycle: true}, nil
		case sub.allowed:
			return outcome{}, nil
		}
		return base, nil
	}
	return outcome{}, fmt.Errorf("%w: unknown rewrite", ErrModel)
}

// direct resolves the direct assignment of rd on o: an exact tuple, a
// wildcard tuple, or a userset tuple whose userset contains the subject,
// in the order of the relation's type restrictions. Only tuples the
// restrictions allow count, as in OpenFGA, so a tuple left over from an
// older model grants nothing.
func (r *resolver) direct(ctx context.Context, o Object, rd *relationDef, depth int) (outcome, error) {
	here := Step{Object: o, Relation: rd.name}
	usersets := false
	for _, ref := range rd.directTypes {
		var probe TupleUser
		switch {
		case ref.Relation != "":
			usersets = true
			continue
		case ref.Type != r.user.Type:
			continue
		case ref.Wildcard:
			probe = TupleUser{Type: ref.Type, ID: Wildcard}
		default:
			probe = r.user
		}
		ok, err := r.e.tuples.Contains(ctx, o, rd.name, probe)
		if err != nil {
			return outcome{}, fmt.Errorf("authz: read %s#%s: %w", o, rd.name, err)
		}
		if ok {
			return outcome{allowed: true, paths: Basis{{Steps: []Step{here}, Grant: probe}}}, nil
		}
	}
	if !usersets {
		return outcome{}, nil
	}
	users, err := r.read(ctx, o, rd.name)
	if err != nil {
		return outcome{}, err
	}
	var acc anyOf
	for _, u := range users {
		if u.Relation == "" || !rd.allowsUserset(u) {
			continue
		}
		out, err := r.check(ctx, Object{Type: u.Type, ID: u.ID}, u.Relation, depth+1)
		if acc.add(out.through(here), err) {
			break
		}
	}
	return acc.result()
}

// read returns the users of o#relation in a stable order (by their
// OpenFGA string), whatever order the source returns them in, so the
// evaluation and its Basis are deterministic.
func (r *resolver) read(ctx context.Context, o Object, relation string) ([]TupleUser, error) {
	users, err := r.e.tuples.Read(ctx, o, relation)
	if err != nil {
		return nil, fmt.Errorf("authz: read %s#%s: %w", o, relation, err)
	}
	users = slices.Clone(users)
	slices.SortFunc(users, func(a, b TupleUser) int { return cmp.Compare(a.String(), b.String()) })
	return users, nil
}

// through prepends a step to every path of an allowed outcome.
func (out outcome) through(step Step) outcome {
	if !out.allowed {
		return out
	}
	paths := make(Basis, len(out.paths))
	for i, p := range out.paths {
		paths[i] = Path{Steps: append([]Step{step}, p.Steps...), Grant: p.Grant}
	}
	out.paths = paths
	return out
}

// anyOf folds the operands of a union (and the alternatives of a
// tuple-to-userset or userset): the first allowed operand wins; otherwise
// an error, then a cycle, decides.
type anyOf struct {
	allowed  outcome
	found    bool
	cycle    bool
	firstErr error
}

// add records one operand and reports whether the fold is decided.
func (a *anyOf) add(out outcome, err error) bool {
	switch {
	case err != nil:
		if a.firstErr == nil {
			a.firstErr = err
		}
	case out.allowed:
		a.allowed, a.found = out, true
		return true
	case out.cycle:
		a.cycle = true
	}
	return false
}

func (a *anyOf) result() (outcome, error) {
	if a.found {
		return a.allowed, nil
	}
	if a.firstErr != nil {
		return outcome{}, a.firstErr
	}
	return outcome{cycle: a.cycle}, nil
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
