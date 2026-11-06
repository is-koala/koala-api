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
// again. The value is the relation that decides visibility. A denial of
// the visibility relation itself (company#viewer) needs no entry: it is
// concealed by definition. The policy is checked against the conceal
// column of testdata/matrix.yaml.
//
// Field-level projections (the masked address, the employer view, the CV
// link) do not reject at all; their callers ignore Conceal.
func concealPolicy() map[string]Action {
	return map[string]Action{
		TypeCompany + "#" + string(ActionCompanyCanReadInternal): ActionCompanyViewer,
		TypeCompany + "#" + string(ActionCompanyCanManage):       ActionCompanyViewer,
		TypeCompany + "#" + string(ActionCompanyCanManageJobs):   ActionCompanyViewer,
		TypeJob + "#" + string(ActionJobCanApply):                ActionJobCanView,
		TypeJob + "#" + string(ActionJobCanOpenConversation):     ActionJobCanView,
		TypeJob + "#" + string(ActionJobCanReadExactAddress):     ActionJobCanView,
		TypeUser + "#" + string(ActionUserCanSeePresence):        ActionUserPublicViewer,
		TypeMessage + "#" + string(ActionMessageCanReport):       ActionMessageCanRead,
	}
}

// concealUnless returns the visibility relation for a denial of a on an
// object of objectType, or false when the denial is always concealed.
func concealUnless(objectType string, a Action) (Action, bool) {
	v, ok := concealPolicy()[objectType+"#"+string(a)]
	return v, ok
}

// concealed reports whether a denial on an object of objectType can be
// concealed at all. A denial on the platform object, a missing staff
// permission, never is: the platform is no resource whose existence
// could leak, the admin surface is public in the contract, and the
// staff member gets 403 STAFF_PERMISSION_DENIED (ADR-0026).
func concealed(objectType string) bool { return objectType != TypePlatform }
