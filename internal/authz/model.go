package authz

import (
	"bytes"
	_ "embed" // model.json
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// modelJSON is model.fga in OpenFGA JSON form (see doc.go).
//
//go:embed model.json
var modelJSON []byte

// SchemaVersion is the only OpenFGA schema version the evaluator accepts.
const SchemaVersion = "1.1"

// ErrModel marks a model outside the supported subset or an invalid one.
var ErrModel = errors.New("authz: invalid model")

// Model is a loaded authorization model: type definitions with their
// relations, ready for the evaluator. It is immutable after loading.
type Model struct {
	types map[string]*typeDef
}

type typeDef struct {
	name      string
	relations map[string]*relationDef
}

type relationDef struct {
	typeName string
	name     string
	rewrite  *rewrite
	// directTypes are the type restrictions of the relation's direct
	// assignment ([user, company#member, user:*]). Empty when the
	// relation has no direct assignment.
	directTypes []typeRef
}

// typeRef is one entry of a type restriction.
type typeRef struct {
	Type     string
	Relation string // userset restriction "company#member"
	Wildcard bool   // "user:*"
}

type rewriteKind uint8

const (
	rewriteThis rewriteKind = iota + 1
	rewriteComputed
	rewriteTupleToUserset
	rewriteUnion
	rewriteIntersection
	rewriteDifference
)

// rewrite is one node of a relation's userset rewrite.
type rewrite struct {
	kind rewriteKind
	// relation: the computed relation (rewriteComputed) or the relation
	// looked up on the parent object (rewriteTupleToUserset).
	relation string
	// tupleset: the relation on this object that names the parent
	// (rewriteTupleToUserset).
	tupleset string
	// children: the operands of union and intersection; for difference
	// exactly two, base and subtract.
	children []*rewrite
}

// DefaultModel loads the embedded model.json.
func DefaultModel() (*Model, error) { return LoadModel(modelJSON) }

// LoadModel loads an authorization model in OpenFGA JSON form and checks
// it the way OpenFGA does when the model is written (names follow
// OpenFGA's rules, "self" and "this" are reserved, types and relations
// exist, type restrictions are valid, tuple-to-userset parents are
// directly assignable objects), plus the subset rules of ADR-0024:
// schema 1.1, no modules, no conditions, "and" and "but not" only at the
// top of a relation over plain operands. Unknown JSON fields are
// rejected, so a construct the evaluator does not know cannot slip in.
func LoadModel(data []byte) (*Model, error) {
	var w wireModel
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", ErrModel, err)
	}
	if w.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: schema version %q, want %q", ErrModel, w.SchemaVersion, SchemaVersion)
	}
	if len(w.Conditions) > 0 {
		return nil, fmt.Errorf("%w: conditions are outside the subset (ADR-0024)", ErrModel)
	}
	m := &Model{types: make(map[string]*typeDef, len(w.TypeDefinitions))}
	for i := range w.TypeDefinitions {
		td, err := buildType(&w.TypeDefinitions[i])
		if err != nil {
			return nil, err
		}
		if _, dup := m.types[td.name]; dup {
			return nil, fmt.Errorf("%w: type %q defined twice", ErrModel, td.name)
		}
		m.types[td.name] = td
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// Types returns the type names in sorted order.
func (m *Model) Types() []string {
	out := make([]string, 0, len(m.types))
	for name := range m.types {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Relations returns the relation names of a type in sorted order, or nil
// when the type does not exist.
func (m *Model) Relations(typeName string) []string {
	td := m.types[typeName]
	if td == nil {
		return nil
	}
	out := make([]string, 0, len(td.relations))
	for name := range td.relations {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// UsesIntersectionOrExclusion reports whether the relation's rewrite uses
// "and" or "but not". The subset test keeps an explicit list of such
// relations so a new one is a reviewed decision (ADR-0024).
func (m *Model) UsesIntersectionOrExclusion(typeName, relation string) bool {
	rd := m.relation(typeName, relation)
	if rd == nil {
		return false
	}
	k := rd.rewrite.kind
	return k == rewriteIntersection || k == rewriteDifference
}

func (m *Model) relation(typeName, relation string) *relationDef {
	td := m.types[typeName]
	if td == nil {
		return nil
	}
	return td.relations[relation]
}

// ValidateTuple checks a tuple against the model the way OpenFGA checks a
// write: the object type and relation exist and the user matches one of
// the relation's type restrictions.
func (m *Model) ValidateTuple(t Tuple) error {
	rd := m.relation(t.Object.Type, t.Relation)
	if rd == nil {
		return fmt.Errorf("%w: tuple %s: relation %s#%s is not defined", ErrModel, t, t.Object.Type, t.Relation)
	}
	if t.Object.ID == Wildcard {
		return fmt.Errorf("%w: tuple %s: the object cannot be a wildcard", ErrModel, t)
	}
	for _, ref := range rd.directTypes {
		if ref.allows(t.User) {
			return nil
		}
	}
	return fmt.Errorf("%w: tuple %s: user type is not allowed by %s#%s", ErrModel, t, t.Object.Type, t.Relation)
}

func (r typeRef) allows(u TupleUser) bool {
	if r.Type != u.Type {
		return false
	}
	switch {
	case r.Wildcard:
		return u.IsWildcard()
	case r.Relation != "":
		return u.Relation == r.Relation && u.ID != Wildcard
	default:
		return u.IsObject()
	}
}

func buildType(w *wireType) (*typeDef, error) {
	if !ValidTypeName(w.Type) || reserved(w.Type) {
		return nil, fmt.Errorf("%w: invalid or reserved type name %q", ErrModel, w.Type)
	}
	td := &typeDef{name: w.Type, relations: make(map[string]*relationDef, len(w.Relations))}
	var meta map[string]wireRelationMetadata
	if w.Metadata != nil {
		if w.Metadata.Module != "" || len(w.Metadata.SourceInfo) > 0 {
			return nil, fmt.Errorf("%w: type %q: modular models are outside the subset", ErrModel, w.Type)
		}
		meta = w.Metadata.Relations
	}
	for name := range meta {
		if _, ok := w.Relations[name]; !ok {
			return nil, fmt.Errorf("%w: %s#%s: metadata for an undefined relation", ErrModel, w.Type, name)
		}
	}
	for name, us := range w.Relations {
		if !ValidRelationName(name) || reserved(name) {
			return nil, fmt.Errorf("%w: %s: invalid or reserved relation name %q", ErrModel, w.Type, name)
		}
		rw, err := buildRewrite(&us, true)
		if err != nil {
			return nil, fmt.Errorf("%s#%s: %w", w.Type, name, err)
		}
		rd := &relationDef{typeName: w.Type, name: name, rewrite: rw}
		md := meta[name]
		if md.Module != "" || len(md.SourceInfo) > 0 {
			return nil, fmt.Errorf("%w: %s#%s: modular models are outside the subset", ErrModel, w.Type, name)
		}
		for _, ref := range md.DirectlyRelatedUserTypes {
			if ref.Condition != "" {
				return nil, fmt.Errorf("%w: %s#%s: conditions are outside the subset (ADR-0024)", ErrModel, w.Type, name)
			}
			if ref.Wildcard != nil && ref.Relation != "" {
				return nil, fmt.Errorf("%w: %s#%s: a restriction cannot be both wildcard and userset", ErrModel, w.Type, name)
			}
			rd.directTypes = append(rd.directTypes, typeRef{Type: ref.Type, Relation: ref.Relation, Wildcard: ref.Wildcard != nil})
		}
		td.relations[name] = rd
	}
	return td, nil
}

// buildRewrite converts one wire userset. top is true for the root of a
// relation: only there may intersection and difference appear, and their
// operands must be plain (this, computed, tuple-to-userset).
func buildRewrite(w *wireUserset, top bool) (*rewrite, error) {
	set := 0
	for _, b := range []bool{w.This != nil, w.ComputedUserset != nil, w.TupleToUserset != nil, w.Union != nil, w.Intersection != nil, w.Difference != nil} {
		if b {
			set++
		}
	}
	if set != 1 {
		return nil, fmt.Errorf("%w: a userset must set exactly one rewrite, found %d", ErrModel, set)
	}
	switch {
	case w.This != nil:
		return &rewrite{kind: rewriteThis}, nil
	case w.ComputedUserset != nil:
		if w.ComputedUserset.Object != "" || w.ComputedUserset.Relation == "" {
			return nil, fmt.Errorf("%w: computed userset needs a relation and no object", ErrModel)
		}
		return &rewrite{kind: rewriteComputed, relation: w.ComputedUserset.Relation}, nil
	case w.TupleToUserset != nil:
		ttu := w.TupleToUserset
		if ttu.Tupleset.Object != "" || ttu.Tupleset.Relation == "" ||
			ttu.ComputedUserset.Object != "" || ttu.ComputedUserset.Relation == "" {
			return nil, fmt.Errorf("%w: tuple-to-userset needs a tupleset and a computed relation", ErrModel)
		}
		return &rewrite{kind: rewriteTupleToUserset, relation: ttu.ComputedUserset.Relation, tupleset: ttu.Tupleset.Relation}, nil
	case w.Union != nil:
		return buildChildren(rewriteUnion, w.Union.Child, false)
	case w.Intersection != nil:
		if !top {
			return nil, fmt.Errorf("%w: \"and\" is allowed only at the top of a relation", ErrModel)
		}
		return buildChildren(rewriteIntersection, w.Intersection.Child, true)
	default: // difference
		if !top {
			return nil, fmt.Errorf("%w: \"but not\" is allowed only at the top of a relation", ErrModel)
		}
		if w.Difference.Base == nil || w.Difference.Subtract == nil {
			return nil, fmt.Errorf("%w: \"but not\" needs a base and a subtract", ErrModel)
		}
		return buildChildren(rewriteDifference, []wireUserset{*w.Difference.Base, *w.Difference.Subtract}, true)
	}
}

func buildChildren(kind rewriteKind, children []wireUserset, plainOnly bool) (*rewrite, error) {
	if len(children) < 2 {
		return nil, fmt.Errorf("%w: a set operation needs at least two operands", ErrModel)
	}
	rw := &rewrite{kind: kind, children: make([]*rewrite, 0, len(children))}
	for i := range children {
		c, err := buildRewrite(&children[i], false)
		if err != nil {
			return nil, err
		}
		if plainOnly && (c.kind == rewriteUnion || c.kind == rewriteIntersection || c.kind == rewriteDifference) {
			return nil, fmt.Errorf("%w: operands of \"and\" and \"but not\" must be plain relations", ErrModel)
		}
		rw.children = append(rw.children, c)
	}
	return rw, nil
}

// reserved reports whether name is a keyword OpenFGA reserves for type
// and relation names.
func reserved(name string) bool { return name == "self" || name == "this" }

// validate checks references across types and entrypoints, as OpenFGA's
// model write does.
func (m *Model) validate() error {
	for _, td := range m.types {
		for _, rd := range td.relations {
			if err := m.validateRelation(rd); err != nil {
				return fmt.Errorf("%s#%s: %w", rd.typeName, rd.name, err)
			}
		}
	}
	return m.validateEntrypoints()
}

// validateEntrypoints rejects a relation that can never be granted
// because no rewrite path reaches a direct assignment, for example two
// relations defined only as each other (a: b, b: a). This is OpenFGA's
// "no entrypoint" rule, computed as a fixed point: a relation has an
// entrypoint when its rewrite does; a direct assignment has one when one
// of its restrictions does (a type or a wildcard always, a userset when
// that relation has one); a computed userset and a tuple-to-userset when
// the relation they name does (on some parent type); a union when any
// operand does, an intersection when every operand does, an exclusion
// when its base does.
func (m *Model) validateEntrypoints() error {
	has := map[*relationDef]bool{}
	for changed := true; changed; {
		changed = false
		for _, td := range m.types {
			for _, rd := range td.relations {
				if !has[rd] && m.reachesDirect(rd, rd.rewrite, has) {
					has[rd], changed = true, true
				}
			}
		}
	}
	for _, name := range m.Types() {
		for _, rel := range m.Relations(name) {
			if rd := m.relation(name, rel); !has[rd] {
				return fmt.Errorf("%w: %s#%s has no entrypoint: no rewrite reaches a direct assignment", ErrModel, name, rel)
			}
		}
	}
	return nil
}

func (m *Model) reachesDirect(rd *relationDef, rw *rewrite, has map[*relationDef]bool) bool {
	switch rw.kind {
	case rewriteThis:
		for _, ref := range rd.directTypes {
			if ref.Relation == "" || has[m.relation(ref.Type, ref.Relation)] {
				return true
			}
		}
		return false
	case rewriteComputed:
		return has[m.relation(rd.typeName, rw.relation)]
	case rewriteTupleToUserset:
		for _, ref := range m.relation(rd.typeName, rw.tupleset).directTypes {
			if target := m.relation(ref.Type, rw.relation); target != nil && has[target] {
				return true
			}
		}
		return false
	case rewriteUnion:
		for _, c := range rw.children {
			if m.reachesDirect(rd, c, has) {
				return true
			}
		}
		return false
	case rewriteIntersection:
		for _, c := range rw.children {
			if !m.reachesDirect(rd, c, has) {
				return false
			}
		}
		return true
	case rewriteDifference:
		return m.reachesDirect(rd, rw.children[0], has)
	}
	return false
}

func (m *Model) validateRelation(rd *relationDef) error {
	hasThis := rd.rewrite.contains(rewriteThis)
	switch {
	case hasThis && len(rd.directTypes) == 0:
		return fmt.Errorf("%w: direct assignment without type restrictions", ErrModel)
	case !hasThis && len(rd.directTypes) > 0:
		return fmt.Errorf("%w: type restrictions without direct assignment", ErrModel)
	}
	for _, ref := range rd.directTypes {
		target := m.types[ref.Type]
		if target == nil {
			return fmt.Errorf("%w: restriction names undefined type %q", ErrModel, ref.Type)
		}
		if ref.Relation != "" && target.relations[ref.Relation] == nil {
			return fmt.Errorf("%w: restriction names undefined relation %s#%s", ErrModel, ref.Type, ref.Relation)
		}
	}
	return m.validateRewrite(rd, rd.rewrite)
}

func (m *Model) validateRewrite(rd *relationDef, rw *rewrite) error {
	switch rw.kind {
	case rewriteThis:
		return nil
	case rewriteComputed:
		if m.relation(rd.typeName, rw.relation) == nil {
			return fmt.Errorf("%w: undefined relation %s#%s", ErrModel, rd.typeName, rw.relation)
		}
		return nil
	case rewriteTupleToUserset:
		ts := m.relation(rd.typeName, rw.tupleset)
		if ts == nil {
			return fmt.Errorf("%w: undefined tupleset relation %s#%s", ErrModel, rd.typeName, rw.tupleset)
		}
		if ts.rewrite.kind != rewriteThis {
			return fmt.Errorf("%w: tupleset %s#%s must be a plain direct relation", ErrModel, rd.typeName, rw.tupleset)
		}
		found := false
		for _, ref := range ts.directTypes {
			if ref.Wildcard || ref.Relation != "" {
				return fmt.Errorf("%w: tupleset %s#%s may only name object types", ErrModel, rd.typeName, rw.tupleset)
			}
			if m.relation(ref.Type, rw.relation) != nil {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%w: no parent type of %s#%s defines %q", ErrModel, rd.typeName, rw.tupleset, rw.relation)
		}
		return nil
	case rewriteUnion, rewriteIntersection, rewriteDifference:
		for _, c := range rw.children {
			if err := m.validateRewrite(rd, c); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("%w: unknown rewrite", ErrModel)
}

func (rw *rewrite) contains(k rewriteKind) bool {
	if rw.kind == k {
		return true
	}
	for _, c := range rw.children {
		if c.contains(k) {
			return true
		}
	}
	return false
}

// Wire form: OpenFGA's AuthorizationModel with the protobuf JSON names of
// openfga/api (snake_case except computedUserset and tupleToUserset), as
// openfga/language writes it and OpenFGA's HTTP API reads it. Only the
// fields of the subset are known.
type wireModel struct {
	ID              string                     `json:"id,omitempty"`
	SchemaVersion   string                     `json:"schema_version"`
	TypeDefinitions []wireType                 `json:"type_definitions"`
	Conditions      map[string]json.RawMessage `json:"conditions,omitempty"`
}

type wireType struct {
	Type      string                 `json:"type"`
	Relations map[string]wireUserset `json:"relations,omitempty"`
	Metadata  *wireTypeMetadata      `json:"metadata,omitempty"`
}

type wireTypeMetadata struct {
	Relations  map[string]wireRelationMetadata `json:"relations,omitempty"`
	Module     string                          `json:"module,omitempty"`
	SourceInfo json.RawMessage                 `json:"source_info,omitempty"`
}

type wireRelationMetadata struct {
	DirectlyRelatedUserTypes []wireRelationReference `json:"directly_related_user_types,omitempty"`
	Module                   string                  `json:"module,omitempty"`
	SourceInfo               json.RawMessage         `json:"source_info,omitempty"`
}

type wireRelationReference struct {
	Type      string    `json:"type"`
	Relation  string    `json:"relation,omitempty"`
	Wildcard  *struct{} `json:"wildcard,omitempty"`
	Condition string    `json:"condition,omitempty"`
}

type wireUserset struct {
	This            *struct{}           `json:"this,omitempty"`
	ComputedUserset *wireObjectRel      `json:"computedUserset,omitempty"`
	TupleToUserset  *wireTupleToUserset `json:"tupleToUserset,omitempty"`
	Union           *wireUsersets       `json:"union,omitempty"`
	Intersection    *wireUsersets       `json:"intersection,omitempty"`
	Difference      *wireDifference     `json:"difference,omitempty"`
}

type wireObjectRel struct {
	Object   string `json:"object,omitempty"`
	Relation string `json:"relation,omitempty"`
}

type wireTupleToUserset struct {
	Tupleset        wireObjectRel `json:"tupleset"`
	ComputedUserset wireObjectRel `json:"computedUserset"`
}

type wireUsersets struct {
	Child []wireUserset `json:"child"`
}

type wireDifference struct {
	Base     *wireUserset `json:"base"`
	Subtract *wireUserset `json:"subtract"`
}
