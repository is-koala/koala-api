// Package authz is the single authorization decision point of the service
// (ADR-0014, ADR-0024; plan §5). Every "may this subject do that to this
// object" question is answered here and nowhere else.
//
// # Rules
//
// The rules live in model.fga, written in a subset of the OpenFGA DSL
// (schema 1.1): direct type restrictions ([type], [type:*],
// [type#relation]), "X from Y", "or", and a limited "and" / "but not"
// (only at the top of a relation, only over plain operands). Conditions
// ("with condition", CEL) are outside the subset (ADR-0024): a model that
// uses one fails to load. Attribute and status rules (job status, open
// dispute, blocking) are not part of the model: they are named prefilters
// outside ReBAC (plan §5.2-3) or become the presence of a tuple (for
// example job#applicant_pool is written only while the job is OPEN).
//
// model.json is model.fga in OpenFGA's JSON form, produced by the official
// parser (github.com/openfga/language, a test-only dependency). The service
// embeds and interprets model.json; the conformance runner uploads the same
// file to a real OpenFGA server. TestModelJSONMatchesDSL fails when the two
// files drift apart; after editing model.fga run
//
//	go test ./internal/authz -run TestModelJSONMatchesDSL -update
//
// and commit both files; the same run rewrites actions_gen.go, the typed
// constants (TypeJob, ActionJobCanEdit) that catch a misspelt relation at
// compile time.
//
// # Evaluation
//
// Evaluator interprets the model over a TupleReader, the way OpenFGA and
// SpiceDB interpret theirs: direct tuples, computed usersets,
// tuple-to-userset, union, intersection and exclusion, with OpenFGA's
// resolution depth limit and cycle rule (a cycle denies, also under "but
// not"). It visits rewrites in model order and tuples in sorted order, so
// a decision and its Basis are deterministic, and it resolves each
// object#relation once per check. The tuple source is an interface on purpose:
// tests use an in-memory source (authztest), and domain tables get typed
// SQL sources per relation in Faz 2-3. The authz_tuples view is never read
// on the hot path (plan §5.2-1).
//
// # Interfaces
//
// Checker, Scoper, CapabilityReader and RelationSink are the contract of
// plan §5.3. Go interfaces belong to their consumer: a service declares the
// one method it uses, for example
//
//	type checker interface {
//		Check(ctx context.Context, s authz.Subject, a authz.Action, o authz.Object, opts ...authz.CheckOption) (authz.Decision, error)
//	}
//
// and is wired with *Evaluator (or, on migration day, an OpenFGA client).
// The interfaces declared here document the contract and keep alternative
// implementations (ShadowChecker, a future OpenFGA client) honest; services
// should not import them as their dependency type.
//
// # Migration
//
// ShadowChecker is the dark-mode skeleton of plan §5.7: it answers from the
// primary checker, asks the shadow checker too and counts disagreements as
// koala_authz_mismatch_total through MismatchCounter.
package authz
