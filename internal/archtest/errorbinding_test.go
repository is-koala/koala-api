package archtest

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/pending"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/staff"
)

// The handler binding of x-error-codes (ADR-0029 Karar 1 "Gereklilik",
// ADR-0032 Karar 1). For every operation that has a handler, the error
// sites the handler can reach are traced statically, and three things are
// held against the contract:
//
//  1. every code a reachable site produces is in the operation's
//     x-error-codes, unless it is universal (universalCodes);
//  2. every reachable site's apperr.Kind maps (httpx.StatusOf) to the
//     status ErrorCode's description documents for its code;
//  3. every code in x-error-codes is produced by a reachable site, unless
//     a reasoned exemption says why the trace cannot see it.
//
// The trace is an AST walk, not a type-checked call graph
// (golang.org/x/tools is not an approved dependency, ADR-0003). It starts
// at the method whose second parameter is apiv1.<Op>RequestObject and
// follows calls it can resolve without types:
//
//   - f(…), a function of the same package;
//   - pkg.F(…), a function of another package of the module;
//   - r.M(…) and r.f.g.M(…), a method on the receiver or reached through
//     the receiver's struct fields whose declared types are structs of the
//     module (by name, so two types with a method of one name do not mix).
//
// It records apperr.New(kind, code) and apperr.Wrap(cause, kind, code)
// whose kind is an apperr.Kind constant and whose code is a constant of
// the module, or a parameter of a function literal bound to a local name
// that every call passes a constant (the `end := func(…, code)` shape of
// the refresh paths).
//
// What it does not see, by design and named in ADR-0032: calls through an
// interface (a field of interface type, a function value, a parameter);
// methods of local variables (q := authndb.New(tx); q.M()); embedded
// fields; codes that travel in a variable of another function. A site it
// reaches but cannot read (kind or code not a constant) is red, not
// skipped. An interface call into code that returns a listed code shows up
// as rule 3, so the blind spot fails closed on the listed side; an unlisted
// code behind an interface is the one way past.

// apiv1Path is the import path of the generated server.
const apiv1Path = modulePath + "/internal/apiv1"

// handlerless are the packages whose methods with an apiv1 request object
// are not handlers: the 501 stubs of frozen operations and the forwarders
// of internal/app. A handler written there is found nowhere, which is red.
func handlerless() []string {
	return []string{modulePath + "/internal/pending", modulePath + "/internal/app"}
}

// universalCodes are the codes no operation lists (ADR-0029 Karar 1, sınır
// 1): they come from the common responses.
func universalCodes() map[string]string {
	out := map[string]string{}
	for _, c := range httpx.PlatformCodes() {
		out[string(c)] = "platform: httpx answers it for any operation (validation, limits, routing)"
	}
	for _, c := range []apperr.Code{
		authn.CodeTokenMissing, authn.CodeTokenInvalid, authn.CodeTokenExpired, authn.CodeSessionInactive,
		authn.CodeSessionDeviceMismatch, authn.CodeStaffSessionEnded, authn.CodeStaffPasswordChangeRequired,
	} {
		out[string(c)] = "session: the Unauthorized/Forbidden response of every authenticated operation"
	}
	out[string(staff.CodePermissionDenied)] = "the staff permission gate of every admin operation"
	out[string(pending.CodeNotImplemented)] = "the default response of a frozen operation"
	return out
}

// nonStrictOperations are operations that have no strict handler, with the
// reason. They are traced nowhere; an entry whose operation gets a strict
// handler is stale and red.
func nonStrictOperations() map[string]string {
	return map[string]string{
		"issueRealtimeTicket": "mounted by internal/realtime.Handler outside the strict server (oapi-codegen exclude-tags: realtime); its codes are pinned by the realtime tests (ADR-0018)",
		"connectRealtime":     "a WebSocket route of internal/realtime outside the strict server; most of its codes are socket events without a status (ADR-0018 §5)",
	}
}

// unseenCodes are listed codes a handler returns through a path the trace
// cannot follow, per operation, with the reason. An entry the trace does
// see is stale and red.
func unseenCodes() map[string]map[string]string {
	return map[string]map[string]string{}
}

// kindStatuses maps every apperr.Kind constant, by name, to its status.
// TestKindStatusesCoverEveryKind holds the keys to the constants.
func kindStatuses() map[string]int {
	kinds := map[string]apperr.Kind{
		"KindInternal": apperr.KindInternal, "KindInvalid": apperr.KindInvalid,
		"KindUnauthenticated": apperr.KindUnauthenticated, "KindForbidden": apperr.KindForbidden,
		"KindNotFound": apperr.KindNotFound, "KindConflict": apperr.KindConflict,
		"KindRateLimited": apperr.KindRateLimited, "KindUpstream": apperr.KindUpstream,
		"KindPayloadTooLarge": apperr.KindPayloadTooLarge, "KindUnavailable": apperr.KindUnavailable,
		"KindUnsupportedMediaType": apperr.KindUnsupportedMediaType, "KindNotImplemented": apperr.KindNotImplemented,
	}
	out := make(map[string]int, len(kinds))
	for name, k := range kinds {
		out[name] = httpx.StatusOf(k)
	}
	return out
}

// errorSite is one apperr.New or apperr.Wrap a trace reached.
type errorSite struct {
	kind, code, pos string
}

// fnRef is a function or method declaration with its file and package.
type fnRef struct {
	pkg  *codePkg
	file *ast.File
	decl *ast.FuncDecl
}

// fieldType is a struct field's declared type and the file it is written in.
type fieldType struct {
	expr ast.Expr
	file *ast.File
}

// codePkg is one package of the module as the trace sees it.
type codePkg struct {
	funcs   map[string]fnRef
	methods map[string]map[string]fnRef     // receiver type → method
	fields  map[string]map[string]fieldType // struct type → field
	consts  map[string]string               // string constants
}

// codeModule is the module's packages by import path.
type codeModule struct {
	fset *token.FileSet
	pkgs map[string]*codePkg
}

// newCodeModule indexes files, grouped by import path.
func newCodeModule(fset *token.FileSet, files map[string][]*ast.File) *codeModule {
	m := &codeModule{fset: fset, pkgs: map[string]*codePkg{}}
	for path, fs := range files {
		p := &codePkg{
			funcs: map[string]fnRef{}, methods: map[string]map[string]fnRef{},
			fields: map[string]map[string]fieldType{}, consts: map[string]string{},
		}
		m.pkgs[path] = p
		for _, f := range fs {
			p.index(f)
		}
	}
	return m
}

func (p *codePkg) index(f *ast.File) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			ref := fnRef{pkg: p, file: f, decl: d}
			if d.Recv == nil {
				p.funcs[d.Name.Name] = ref
				continue
			}
			if _, recv := receiver(d); recv != "" {
				if p.methods[recv] == nil {
					p.methods[recv] = map[string]fnRef{}
				}
				p.methods[recv][d.Name.Name] = ref
			}
		case *ast.GenDecl:
			p.indexGen(f, d)
		}
	}
}

func (p *codePkg) indexGen(f *ast.File, d *ast.GenDecl) {
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.ValueSpec:
			if d.Tok != token.CONST {
				continue
			}
			for i, id := range s.Names {
				if i >= len(s.Values) {
					continue
				}
				if lit, ok := s.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						p.consts[id.Name] = v
					}
				}
			}
		case *ast.TypeSpec:
			st, ok := s.Type.(*ast.StructType)
			if !ok {
				continue
			}
			fields := map[string]fieldType{}
			for _, fld := range st.Fields.List {
				for _, n := range fld.Names {
					fields[n.Name] = fieldType{expr: fld.Type, file: f}
				}
			}
			p.fields[s.Name.Name] = fields
		}
	}
}

// receiver returns a method's receiver name and type name.
func receiver(d *ast.FuncDecl) (name, typ string) {
	if d.Recv == nil || len(d.Recv.List) != 1 {
		return "", ""
	}
	r := d.Recv.List[0]
	t := r.Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	if !ok {
		return "", ""
	}
	if len(r.Names) == 1 {
		name = r.Names[0].Name
	}
	return name, id.Name
}

// imported returns the module package f imports under name, or nil.
func (m *codeModule) imported(f *ast.File, name string) *codePkg {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		local := path[strings.LastIndexByte(path, '/')+1:]
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == name {
			return m.pkgs[path]
		}
	}
	return nil
}

// handlers returns the handler methods of the module by operation method
// name (StartRegistration): the methods whose second parameter is
// apiv1.<Name>RequestObject, outside the handlerless packages.
func (m *codeModule) handlers() map[string][]fnRef {
	out := map[string][]fnRef{}
	for path, p := range m.pkgs {
		if slices.Contains(handlerless(), path) {
			continue
		}
		for _, methods := range p.methods {
			for name, ref := range methods {
				if requestObject(ref) == name+"RequestObject" {
					out[name] = append(out[name], ref)
				}
			}
		}
	}
	return out
}

// requestObject returns the apiv1 type name of a method's second
// parameter, or "".
func requestObject(ref fnRef) string {
	var types []ast.Expr
	for _, fld := range ref.decl.Type.Params.List {
		n := max(1, len(fld.Names))
		for range n {
			types = append(types, fld.Type)
		}
	}
	if len(types) < 2 {
		return ""
	}
	sel, ok := types[1].(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != importName(ref.file, apiv1Path) {
		return ""
	}
	return sel.Sel.Name
}

// errorTrace is the walk from one handler.
type errorTrace struct {
	m          *codeModule
	seen       map[*ast.FuncDecl]bool
	sites      []errorSite
	unresolved []string
}

// trace returns the error sites reachable from entry and the sites it
// reached but could not read.
func (m *codeModule) trace(entry fnRef) (sites []errorSite, unresolved []string) {
	tr := &errorTrace{m: m, seen: map[*ast.FuncDecl]bool{}}
	tr.visit(entry)
	return tr.sites, tr.unresolved
}

func (tr *errorTrace) visit(fn fnRef) {
	if tr.seen[fn.decl] || fn.decl.Body == nil {
		return
	}
	tr.seen[fn.decl] = true
	recvName, recvType := receiver(fn.decl)
	bound := closureNames(fn.decl.Body)
	var stack []ast.Node
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		if call, ok := n.(*ast.CallExpr); ok {
			tr.call(fn, recvName, recvType, call, stack, bound)
		}
		return true
	})
}

func (tr *errorTrace) call(fn fnRef, recvName, recvType string, call *ast.CallExpr, stack []ast.Node, bound map[*ast.FuncLit]string) {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if g, ok := fn.pkg.funcs[fun.Name]; ok {
			tr.visit(g)
		}
	case *ast.SelectorExpr:
		if id, ok := fun.X.(*ast.Ident); ok && id.Name != recvName {
			if alias := importName(fn.file, apperrPath); alias != "" && id.Name == alias {
				tr.site(fn, fun.Sel.Name, call, stack, bound)
				return
			}
			if p := tr.m.imported(fn.file, id.Name); p != nil {
				if g, ok := p.funcs[fun.Sel.Name]; ok {
					tr.visit(g)
				}
				return
			}
		}
		if p, typ, ok := tr.typeOf(fn, recvName, recvType, fun.X); ok {
			if g, ok := p.methods[typ][fun.Sel.Name]; ok {
				tr.visit(g)
			}
		}
	}
}

// typeOf resolves the receiver and a chain of its struct fields to a
// named type of the module.
func (tr *errorTrace) typeOf(fn fnRef, recvName, recvType string, e ast.Expr) (*codePkg, string, bool) {
	switch x := e.(type) {
	case *ast.Ident:
		if recvName != "" && x.Name == recvName {
			return fn.pkg, recvType, true
		}
	case *ast.ParenExpr:
		return tr.typeOf(fn, recvName, recvType, x.X)
	case *ast.SelectorExpr:
		p, typ, ok := tr.typeOf(fn, recvName, recvType, x.X)
		if !ok {
			return nil, "", false
		}
		ft, ok := p.fields[typ][x.Sel.Name]
		if !ok {
			return nil, "", false
		}
		return tr.m.named(p, ft)
	}
	return nil, "", false
}

// named resolves a field's declared type to a struct type or a type with
// methods of the module; an interface, a function type or a type of
// another module is not followed.
func (m *codeModule) named(p *codePkg, ft fieldType) (*codePkg, string, bool) {
	t := ft.expr
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch x := t.(type) {
	case *ast.Ident:
		if p.fields[x.Name] != nil || p.methods[x.Name] != nil {
			return p, x.Name, true
		}
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return nil, "", false
		}
		q := m.imported(ft.file, id.Name)
		if q != nil && (q.fields[x.Sel.Name] != nil || q.methods[x.Sel.Name] != nil) {
			return q, x.Sel.Name, true
		}
	}
	return nil, "", false
}

// site records an apperr.New or apperr.Wrap call.
func (tr *errorTrace) site(fn fnRef, name string, call *ast.CallExpr, stack []ast.Node, bound map[*ast.FuncLit]string) {
	kindAt, codeAt := 0, 1
	switch name {
	case "New":
	case "Wrap":
		kindAt, codeAt = 1, 2
	default:
		return
	}
	pos := tr.m.fset.Position(call.Pos()).String()
	if len(call.Args) <= codeAt {
		tr.unresolved = append(tr.unresolved, pos+": apperr."+name+" with too few arguments")
		return
	}
	kind := kindName(fn.file, call.Args[kindAt])
	codes := tr.codeValues(fn, call.Args[codeAt], stack, bound)
	if kind == "" || codes == nil {
		tr.unresolved = append(tr.unresolved,
			pos+": apperr."+name+" whose kind or code is not a constant; write both as constants at the call")
		return
	}
	for _, c := range codes {
		tr.sites = append(tr.sites, errorSite{kind: kind, code: c, pos: pos})
	}
}

// kindName is the apperr.Kind constant e names, or "".
func kindName(f *ast.File, e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != importName(f, apperrPath) || !strings.HasPrefix(sel.Sel.Name, "Kind") {
		return ""
	}
	return sel.Sel.Name
}

// constValue is the string constant e names in fn's package or an
// imported one.
func (tr *errorTrace) constValue(fn fnRef, e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.Ident:
		v, ok := fn.pkg.consts[x.Name]
		return v, ok
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		if p := tr.m.imported(fn.file, id.Name); p != nil {
			v, ok := p.consts[x.Sel.Name]
			return v, ok
		}
	}
	return "", false
}

// codeValues are the codes e can be: one constant, or, for a parameter of
// the enclosing function literal bound to a local name, the constant each
// call of that name passes. nil when any of them is not a constant.
func (tr *errorTrace) codeValues(fn fnRef, e ast.Expr, stack []ast.Node, bound map[*ast.FuncLit]string) []string {
	if v, ok := tr.constValue(fn, e); ok {
		return []string{v}
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return nil
	}
	for i := len(stack) - 1; i >= 0; i-- {
		lit, ok := stack[i].(*ast.FuncLit)
		if !ok {
			continue
		}
		at := paramIndex(lit.Type, id.Name)
		if at < 0 {
			continue
		}
		name := bound[lit]
		if name == "" {
			return nil
		}
		var out []string
		complete := true
		ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if callee, ok := call.Fun.(*ast.Ident); !ok || callee.Name != name {
				return true
			}
			if at >= len(call.Args) {
				complete = false
				return true
			}
			v, ok := tr.constValue(fn, call.Args[at])
			if !ok {
				complete = false
				return true
			}
			out = append(out, v)
			return true
		})
		if !complete || len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// paramIndex is the position of the parameter named name, or -1.
func paramIndex(ft *ast.FuncType, name string) int {
	i := 0
	for _, fld := range ft.Params.List {
		if len(fld.Names) == 0 {
			i++
			continue
		}
		for _, n := range fld.Names {
			if n.Name == name {
				return i
			}
			i++
		}
	}
	return -1
}

// closureNames maps each function literal assigned to a local name
// (`end := func(…) {…}` or `var end = func…`) to that name.
func closureNames(body *ast.BlockStmt) map[*ast.FuncLit]string {
	out := map[*ast.FuncLit]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range x.Rhs {
				lit, ok := rhs.(*ast.FuncLit)
				if !ok || i >= len(x.Lhs) {
					continue
				}
				if id, ok := x.Lhs[i].(*ast.Ident); ok {
					out[lit] = id.Name
				}
			}
		case *ast.ValueSpec:
			for i, v := range x.Values {
				if lit, ok := v.(*ast.FuncLit); ok && i < len(x.Names) {
					out[lit] = x.Names[i].Name
				}
			}
		}
		return true
	})
	return out
}

// bindingFindings holds an operation's reachable sites against its
// x-error-codes (the three rules at the top of this file). documented is
// code → status from ErrorCode's description, kinds is kind name → status.
func bindingFindings(op string, listed []string, sites []errorSite, documented, kinds map[string]int,
	universal map[string]string, unseen map[string]string,
) []string {
	var out []string
	reached := map[string]bool{}
	for _, s := range sites {
		reached[s.code] = true
		status, ok := kinds[s.kind]
		if !ok {
			out = append(out, s.pos+": "+op+": "+s.kind+" is not a kind kindStatuses knows")
			continue
		}
		doc, ok := documented[s.code]
		switch {
		case !ok:
			out = append(out, s.pos+": "+op+": "+s.code+" documents no HTTP status in ErrorCode, so no response can carry it")
		case doc != status:
			out = append(out, s.pos+": "+op+": "+s.code+" is returned with apperr."+s.kind+" ("+strconv.Itoa(status)+
				") but ErrorCode documents it as "+strconv.Itoa(doc))
		}
		if _, ok := universal[s.code]; !ok && !slices.Contains(listed, s.code) {
			out = append(out, s.pos+": "+op+" can return "+s.code+", which its x-error-codes does not list")
		}
	}
	for _, c := range listed {
		if _, ok := unseen[c]; ok || reached[c] {
			continue
		}
		out = append(out, op+": x-error-codes lists "+c+", which no error site the handler reaches returns")
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// loadCodeModule indexes every non-test, non-generated file of the module.
func loadCodeModule(t *testing.T) *codeModule {
	t.Helper()
	var fset *token.FileSet
	files := map[string][]*ast.File{}
	goFiles(t, func(fs *token.FileSet, path string, f *ast.File) {
		fset = fs
		rel, err := filepath.Rel(moduleRoot, filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		files[modulePath+"/"+filepath.ToSlash(rel)] = append(files[modulePath+"/"+filepath.ToSlash(rel)], f)
	})
	if fset == nil {
		t.Fatal("the walk found no Go files")
	}
	return newCodeModule(fset, files)
}

// operationCodes reads an operation's x-error-codes.
func operationCodes(t *testing.T, where string, ext map[string]any) []string {
	t.Helper()
	raw, ok := ext["x-error-codes"].([]any)
	if !ok {
		t.Errorf("%s: no x-error-codes list", where)
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Errorf("%s: x-error-codes member %v is not a string", where, v)
			continue
		}
		out = append(out, s)
	}
	return out
}

// TestHandlersReturnTheirDeclaredErrorCodes is the binding on the real
// contract and code. It proves it saw real cases: the number of traced
// operations has a floor, and named sites (one of them through the
// closure resolution) must be among the traced ones, so a trace that
// finds nothing is red, not green.
func TestHandlersReturnTheirDeclaredErrorCodes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	spec, err := api.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	documented, err := api.ErrorCodeStatuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	strictType := reflect.TypeOf((*apiv1.StrictServerInterface)(nil)).Elem()
	strict := map[string]bool{}
	for i := range strictType.NumMethod() {
		strict[strictType.Method(i).Name] = true
	}
	m := loadCodeModule(t)
	handlers := m.handlers()
	universal, kinds, unseen := universalCodes(), kindStatuses(), unseenCodes()
	for code := range universal {
		if _, ok := documented[code]; !ok && !strings.HasPrefix(code, "FIELD_") {
			t.Errorf("universal code %s documents no status in ErrorCode", code)
		}
	}

	traced := map[string][]errorSite{}
	ops := map[string]bool{}
	for _, path := range slices.Sorted(maps.Keys(spec.Paths.Map())) {
		for method, op := range spec.Paths.Value(path).Operations() {
			ops[op.OperationID] = true
			if _, frozen := op.Extensions["x-not-implemented"]; frozen {
				continue
			}
			where := method + " " + path + " (" + op.OperationID + ")"
			name := strings.ToUpper(op.OperationID[:1]) + op.OperationID[1:]
			if !strict[name] {
				if _, ok := nonStrictOperations()[op.OperationID]; !ok {
					t.Errorf("%s has no strict handler method and is not in nonStrictOperations", where)
				}
				continue
			}
			found := handlers[name]
			if len(found) != 1 {
				t.Errorf("%s: found %d handler methods taking apiv1.%sRequestObject outside %v, want 1",
					where, len(found), name, handlerless())
				continue
			}
			sites, unresolved := m.trace(found[0])
			for _, u := range unresolved {
				t.Errorf("%s: %s", where, u)
			}
			for _, f := range bindingFindings(op.OperationID, operationCodes(t, where, op.Extensions), sites,
				documented, kinds, universal, unseen[op.OperationID]) {
				t.Error(f)
			}
			traced[op.OperationID] = sites
		}
	}

	// Stale exemptions.
	for op := range nonStrictOperations() {
		if !ops[op] || strict[strings.ToUpper(op[:1])+op[1:]] {
			t.Errorf("nonStrictOperations names %s, which is not an operation without a strict handler", op)
		}
	}
	for op, codes := range unseen {
		for code := range codes {
			for _, s := range traced[op] {
				if s.code == code {
					t.Errorf("unseenCodes exempts %s of %s, which the trace now sees; drop the entry", code, op)
				}
			}
		}
	}

	// Proof that the trace sees real sites.
	if len(traced) < 20 {
		t.Errorf("traced %d operations; the walk no longer reaches the handlers", len(traced))
	}
	for op, want := range map[string]errorSite{
		"loginUser":             {kind: "KindForbidden", code: "AUTH_ACCOUNT_SUSPENDED"},
		"refreshUserSession":    {kind: "KindUnauthenticated", code: "AUTH_REFRESH_TOKEN_REUSED"}, // through `end(…, code)`
		"refreshStaffSession":   {kind: "KindUnauthenticated", code: "AUTH_REFRESH_TOKEN_EXPIRED"},
		"getMySession":          {kind: "KindNotFound", code: "AUTH_SESSION_NOT_FOUND"},
		"completeRegistration":  {kind: "KindConflict", code: "VERIFICATION_TARGET_IN_USE"},
		"completePasswordReset": {kind: "KindInvalid", code: "PASSWORD_RESET_TOKEN_INVALID"},
		"changeMyPassword":      {kind: "KindForbidden", code: "PASSWORD_MISMATCH"},
	} {
		if !slices.ContainsFunc(traced[op], func(s errorSite) bool { return s.kind == want.kind && s.code == want.code }) {
			t.Errorf("the trace of %s does not reach %s %s; it no longer sees the handler's sites", op, want.kind, want.code)
		}
	}
}

// TestKindStatusesCoverEveryKind keeps kindStatuses complete: a Kind added
// to apperr without a line here would make every site of it a finding of
// "unknown kind", but a stale name would pass unnoticed without this.
func TestKindStatusesCoverEveryKind(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(moduleRoot, "internal", "platform", "apperr", "apperr.go"), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST || len(gd.Specs) == 0 {
			continue
		}
		first, ok := gd.Specs[0].(*ast.ValueSpec)
		if !ok {
			continue
		}
		if id, ok := first.Type.(*ast.Ident); !ok || id.Name != "Kind" {
			continue
		}
		for _, spec := range gd.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok {
				for _, n := range vs.Names {
					declared = append(declared, n.Name)
				}
			}
		}
	}
	slices.Sort(declared)
	if got := slices.Sorted(maps.Keys(kindStatuses())); !slices.Equal(got, declared) {
		t.Errorf("kindStatuses names %v, apperr declares %v", got, declared)
	}
}

// demoModule is a two-package module the detector's own tests trace.
func demoModule(t *testing.T) *codeModule {
	t.Helper()
	const imports = `import (
	"context"

	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/demo/other"
	"github.com/is-koala/koala-api/internal/platform/apperr"
)
`
	sources := map[string]string{
		modulePath + "/internal/demo": "package demo\n\n" + imports + `
const (
	CodeGone   apperr.Code = "DEMO_GONE"
	CodeTaken  apperr.Code = "DEMO_TAKEN"
	CodeReused apperr.Code = "DEMO_REUSED"
	CodeStaff  apperr.Code = "DEMO_STAFF_ONLY"
)

type hasher interface{ Hash() error }

type HTTP struct {
	users  *Service
	staff  *Staff
	hasher hasher
}

type Service struct{ inner *Inner }

type Inner struct{}

type Staff struct{}

func (h *HTTP) GetThing(ctx context.Context, _ apiv1.GetThingRequestObject) (any, error) {
	if err := h.users.Login(); err != nil {
		return nil, err
	}
	if err := other.Check(); err != nil {
		return nil, err
	}
	_ = h.hasher.Hash()
	return nil, h.users.refresh()
}

func (h *HTTP) GetOther(ctx context.Context, _ apiv1.GetOtherRequestObject) (any, error) {
	return nil, fail(CodeGone)
}

func (s *Service) Login() error { return s.inner.deep() }

func (i *Inner) deep() error { return apperr.New(apperr.KindNotFound, CodeGone) }

func (s *Staff) Login() error { return apperr.New(apperr.KindForbidden, CodeStaff) }

func (s *Service) refresh() error {
	var outcome error
	end := func(reason string, code apperr.Code) error {
		outcome = apperr.New(apperr.KindConflict, code)
		return nil
	}
	_ = end("a", CodeTaken)
	_ = end("b", CodeReused)
	return outcome
}

func fail(code apperr.Code) error { return apperr.New(apperr.KindInvalid, code) }

func unreached() error { return apperr.New(apperr.KindInvalid, CodeStaff) }
`,
		modulePath + "/internal/demo/other": `package other

import "github.com/is-koala/koala-api/internal/platform/apperr"

const CodeOther = "DEMO_OTHER"

func Check() error { return apperr.Wrap(nil, apperr.KindInvalid, CodeOther) }
`,
		modulePath + "/internal/pending": "package pending\n\n" + imports + `
type Server struct{}

func (Server) GetThing(context.Context, apiv1.GetThingRequestObject) (any, error) { return nil, other.Check() }
`,
	}
	fset := token.NewFileSet()
	files := map[string][]*ast.File{}
	for path, src := range sources {
		f, err := parser.ParseFile(fset, path+"/x.go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = []*ast.File{f}
	}
	return newCodeModule(fset, files)
}

// TestErrorTraceOnADemoModule is the trace's own red and green: what it
// follows, what it keeps apart, what it does not follow, and what it
// refuses to read.
func TestErrorTraceOnADemoModule(t *testing.T) {
	t.Parallel()

	m := demoModule(t)
	handlers := m.handlers()
	if len(handlers["GetThing"]) != 1 || len(handlers["GetOther"]) != 1 {
		t.Fatalf("handlers = %v; want one GetThing (the pending stub is not a handler) and one GetOther", slices.Sorted(maps.Keys(handlers)))
	}
	sites, unresolved := m.trace(handlers["GetThing"][0])
	if len(unresolved) != 0 {
		t.Errorf("GetThing: unresolved %v", unresolved)
	}
	var got []string
	for _, s := range sites {
		got = append(got, s.kind+" "+s.code)
	}
	slices.Sort(got)
	want := []string{
		"KindConflict DEMO_REUSED", // the closure's two calls
		"KindConflict DEMO_TAKEN",
		"KindInvalid DEMO_OTHER", // another package, untyped constant, Wrap
		"KindNotFound DEMO_GONE", // through users.inner, two fields deep
	}
	if !slices.Equal(got, want) {
		// DEMO_STAFF_ONLY would mean the two Login methods were mixed by
		// name; an interface call is not followed.
		t.Errorf("GetThing reaches %v, want %v", got, want)
	}

	_, unresolved = m.trace(handlers["GetOther"][0])
	if len(unresolved) != 1 || !strings.Contains(unresolved[0], "not a constant") {
		t.Errorf("GetOther: unresolved %v; want the site whose code is a function parameter", unresolved)
	}
}

// TestBindingFindings is the comparison's red and green.
func TestBindingFindings(t *testing.T) {
	t.Parallel()

	documented := map[string]int{"DEMO_GONE": 404, "DEMO_TAKEN": 409, "REQUEST_INVALID": 400}
	kinds := map[string]int{"KindNotFound": 404, "KindConflict": 409, "KindInvalid": 400}
	universal := map[string]string{"REQUEST_INVALID": "platform"}
	gone := errorSite{kind: "KindNotFound", code: "DEMO_GONE", pos: "x.go:1"}
	tests := []struct {
		name   string
		listed []string
		sites  []errorSite
		unseen map[string]string
		want   []string // substrings, one per finding
	}{
		{name: "listed and reached", listed: []string{"DEMO_GONE"}, sites: []errorSite{gone}},
		{
			name: "a universal code needs no listing", listed: []string{},
			sites: []errorSite{{kind: "KindInvalid", code: "REQUEST_INVALID", pos: "x.go:2"}},
		},
		{name: "reached, not listed", listed: []string{}, sites: []errorSite{gone}, want: []string{"does not list"}},
		{
			name: "kind disagrees with the documented status", listed: []string{"DEMO_GONE"},
			sites: []errorSite{{kind: "KindConflict", code: "DEMO_GONE", pos: "x.go:3"}},
			want:  []string{"documents it as 404"},
		},
		{
			name: "a code without a documented status", listed: []string{"DEMO_NEW"},
			sites: []errorSite{{kind: "KindConflict", code: "DEMO_NEW", pos: "x.go:4"}},
			want:  []string{"documents no HTTP status"},
		},
		{
			name: "an unknown kind", listed: []string{"DEMO_GONE"},
			sites: []errorSite{{kind: "KindTeapot", code: "DEMO_GONE", pos: "x.go:5"}},
			want:  []string{"not a kind"},
		},
		{name: "listed, not reached", listed: []string{"DEMO_GONE", "DEMO_TAKEN"}, sites: []errorSite{gone}, want: []string{"lists DEMO_TAKEN"}},
		{
			name: "listed, not reached, exempted", listed: []string{"DEMO_GONE", "DEMO_TAKEN"}, sites: []errorSite{gone},
			unseen: map[string]string{"DEMO_TAKEN": "behind an interface"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := bindingFindings("demoOp", tt.listed, tt.sites, documented, kinds, universal, tt.unseen)
			if len(got) != len(tt.want) {
				t.Fatalf("findings %v, want %d", got, len(tt.want))
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("finding %q lacks %q", got[i], w)
				}
			}
		})
	}
}
