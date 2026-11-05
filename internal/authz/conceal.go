package authz

// concealPolicy is the concealment policy of a denial (ADR-0014,
// ADR-0024), keyed by "type#relation".
//
// By default a denial is concealed: the caller answers with the
// resource's own 404, as if the object did not exist. That holds even
// for an object the subject may see elsewhere, because the endpoint
// behind the action (editing a job, deciding an application) belongs to a
// resource the subject must not learn about.
//
// The relations listed here guard an action on an object the subject can
// otherwise see; denying them reveals nothing, so the denial is a plain
// refusal (403 or a domain error) unless the subject cannot see the
// object either (it is deleted or private), in which case it is concealed
// again. The value is the relation that decides visibility. The policy is
// checked against the conceal column of testdata/matrix.yaml.
//
// Field-level projections (the masked address, the employer view, the CV
// link) do not reject at all; their callers ignore Conceal.
func concealPolicy() map[string]Action {
	return map[string]Action{
		"company#viewer":             "viewer",
		"company#can_read_internal":  "viewer",
		"company#can_manage":         "viewer",
		"company#can_manage_jobs":    "viewer",
		"job#can_apply":              "can_view",
		"job#can_open_conversation":  "can_view",
		"job#can_read_exact_address": "can_view",
		"user#can_see_presence":      "public_viewer",
		"message#can_report":         "can_read",
	}
}

// concealUnless returns the visibility relation for a denial of a on an
// object of objectType, or false when the denial is always concealed.
func concealUnless(objectType string, a Action) (Action, bool) {
	v, ok := concealPolicy()[objectType+"#"+string(a)]
	return v, ok
}
