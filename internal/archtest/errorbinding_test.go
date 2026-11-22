package archtest

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
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
// The trace is an AST walk with a small type inference, not a
// type-checked call graph (golang.org/x/tools is not an approved
// dependency, ADR-0003). It starts at the method whose second parameter is
// apiv1.<Op>RequestObject and resolves every call it meets:
//
//   - functions of the module, plain, package-qualified or generic
//     (F[T](…));
//   - methods, on the receiver, a parameter, a local variable, a field
//     chain or a call result, by the declared type of each (a variable's
//     type comes from its declaration or from the result type of the call
//     that assigned it);
//   - an interface method, to every type of the module that has the
//     interface's methods;
//   - function values passed as arguments (a method value, a package
//     function) are visited as if called.
//
// Only packages that can construct an *apperr.Error are traced into: those
// importing apperr, and those importing such a package (capable). A call
// into another package of the module, or outside it, cannot produce a
// module code and is not followed.
//
// A call it cannot resolve is a finding, not a silent skip: a receiver of
// unknown type, an indexed or computed callee, an interface of the module
// that no type implements, a function-typed field. The last two are
// findings only when the signature returns an error; each can be opened
// only by a reasoned exemption (unresolvedCalls), and an exemption the
// trace no longer needs is red. Error sites are apperr.New and apperr.Wrap
// whose kind is an apperr.Kind constant and whose code is a constant, or
// a parameter of a function literal bound to a local name that every call
// passes a constant; a package-level var whose initializer builds one is a
// site wherever it is named. A site it cannot read is a finding too.
//
// What it does not see, by design (ADR-0032 Karar 1): a code that reaches
// a capable package through a package that cannot construct one (a
// platform callback, say); shadowing between two locals of one name and
// different types (the first declaration wins); embedded methods beyond
// one level; codes that travel in a variable of another function.

// apiv1Path is the import path of the generated server.
const apiv1Path = modulePath + "/internal/apiv1"

// minExemptionReason is the shortest reason an exemption may carry, the
// bound the contract's reasoned extensions use.
const minExemptionReason = 20

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
// cannot follow, per operation, with the reason. An entry for an operation
// that does not exist, for a code the operation does not list, or for a
// code the trace does see is red.
func unseenCodes() map[string]map[string]string {
	return map[string]map[string]string{}
}

// unresolvedCalls are the calls the trace cannot resolve and accepts, with
// the reason, keyed by the finding's key: "<package>.<Type>" for an
// interface without implementation or a function-typed field, or
// "<package>.<function>: <callee>" for an unresolved expression. An entry
// no trace needs is stale and red.
func unresolvedCalls() map[string]string {
	return map[string]string{
		"internal/authn.jobQueue": "River's client implements it (outside the module): it inserts a job and returns River's own errors, never an apperr code",
	}
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

// unresolved is a call or site the trace could not resolve. key names it
// for unresolvedCalls; an empty key cannot be exempted (an unreadable
// site).
type unresolved struct {
	key, pos, why string
}

// fnRef is a function or method declaration with its file and package.
type fnRef struct {
	pkg  *codePkg
	file *ast.File
	decl *ast.FuncDecl
}

// typeDecl is a named type of a package.
type typeDecl struct {
	file *ast.File
	spec *ast.TypeSpec
}

// varDecl is a package-level variable: its declared type, its value.
type varDecl struct {
	file  *ast.File
	typ   ast.Expr
	value ast.Expr
}

// codePkg is one package of the module as the trace sees it.
type codePkg struct {
	path    string
	rel     string
	capable bool // it can construct an *apperr.Error (see above)
	imports map[string]bool
	funcs   map[string]fnRef
	methods map[string]map[string]fnRef // receiver type → method
	types   map[string]typeDecl
	vars    map[string]varDecl
	consts  map[string]string // string constants
}

// codeModule is the module's packages by import path.
type codeModule struct {
	fset *token.FileSet
	pkgs map[string]*codePkg
}

// newCodeModule indexes files, grouped by import path, and marks the
// capable packages.
func newCodeModule(fset *token.FileSet, files map[string][]*ast.File) *codeModule {
	m := &codeModule{fset: fset, pkgs: map[string]*codePkg{}}
	for path, pkgFiles := range files {
		p := &codePkg{
			path: path, rel: strings.TrimPrefix(path, modulePath+"/"), imports: map[string]bool{},
			funcs: map[string]fnRef{}, methods: map[string]map[string]fnRef{},
			types: map[string]typeDecl{}, vars: map[string]varDecl{}, consts: map[string]string{},
		}
		m.pkgs[path] = p
		for _, f := range pkgFiles {
			p.index(f)
		}
	}
	for _, p := range m.pkgs {
		p.capable = p.imports[apperrPath]
	}
	for changed := true; changed; {
		changed = false
		for _, p := range m.pkgs {
			if p.capable {
				continue
			}
			for imp := range p.imports {
				if q := m.pkgs[imp]; q != nil && q.capable {
					p.capable, changed = true, true
					break
				}
			}
		}
	}
	return m
}

func (p *codePkg) index(f *ast.File) {
	for _, imp := range f.Imports {
		p.imports[strings.Trim(imp.Path.Value, `"`)] = true
	}
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
			for i, id := range s.Names {
				var value ast.Expr
				if i < len(s.Values) {
					value = s.Values[i]
				}
				if d.Tok == token.VAR {
					p.vars[id.Name] = varDecl{file: f, typ: s.Type, value: value}
					continue
				}
				if lit, ok := value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						p.consts[id.Name] = v
					}
				}
			}
		case *ast.TypeSpec:
			p.types[s.Name.Name] = typeDecl{file: f, spec: s}
		}
	}
}

// receiver returns a method's receiver name and type name.
func receiver(d *ast.FuncDecl) (name, typeName string) {
	if d.Recv == nil || len(d.Recv.List) != 1 {
		return "", ""
	}
	r := d.Recv.List[0]
	t := unparen(r.Type)
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch x := t.(type) {
	case *ast.IndexExpr: // a generic receiver T[P]
		t = x.X
	case *ast.IndexListExpr:
		t = x.X
	}
	id, ok := t.(*ast.Ident)
	if !ok {
		return "", ""
	}
	if len(r.Names) == 1 {
		name = r.Names[0].Name
	}
	typeName = id.Name
	return name, typeName
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// imported returns the module package f imports under name and whether f
// imports any package under it.
func (m *codeModule) imported(f *ast.File, name string) (*codePkg, bool) {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if imp.Name != nil {
			if imp.Name.Name == name {
				return m.pkgs[path], true
			}
			continue
		}
		if slices.Contains(packageNames(path), name) {
			return m.pkgs[path], true
		}
	}
	return nil, false
}

// packageNames are the names a package imported without a name may go by,
// read off its path: the last element, the one before a major version
// suffix (github.com/golang-jwt/jwt/v5 is jwt), and the last element
// without a go- prefix, a -go or .go suffix or its dashes
// (github.com/wneessen/go-mail is mail). Without the type checker the
// package clause of an outside package is not read; a name none of these
// gives is not recognized, and a call through it is reported as
// unresolved rather than passed over.
func packageNames(path string) []string {
	elems := strings.Split(path, "/")
	last := elems[len(elems)-1]
	names := []string{last}
	if len(elems) > 1 && len(last) > 1 && last[0] == 'v' && strings.Trim(last[1:], "0123456789") == "" {
		names = append(names, elems[len(elems)-2])
		last = elems[len(elems)-2]
	}
	trimmed := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(last, "go-"), "-go"), ".go")
	return append(names, trimmed, strings.ReplaceAll(trimmed, "-", ""))
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
	var params []ast.Expr
	for _, fld := range ref.decl.Type.Params.List {
		for range max(1, len(fld.Names)) {
			params = append(params, fld.Type)
		}
	}
	if len(params) < 2 {
		return ""
	}
	sel, ok := params[1].(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != importName(ref.file, apiv1Path) {
		return ""
	}
	return sel.Sel.Name
}

// typ is a type as the trace knows it: a type expression in a package and
// file of the module, or ext for a type outside the module (or builtin),
// whose methods cannot construct a module code.
type typ struct {
	ext  bool
	pkg  *codePkg
	file *ast.File
	expr ast.Expr
}

// builtinTypes are the predeclared types.
func builtinTypes() map[string]bool {
	return map[string]bool{
		"any": true, "bool": true, "byte": true, "comparable": true, "complex64": true, "complex128": true,
		"error": true, "float32": true, "float64": true, "int": true, "int8": true, "int16": true, "int32": true,
		"int64": true, "rune": true, "string": true, "uint": true, "uint8": true, "uint16": true, "uint32": true,
		"uint64": true, "uintptr": true,
	}
}

// builtinFuncs are the predeclared functions.
func builtinFuncs() map[string]bool {
	return map[string]bool{
		"append": true, "cap": true, "clear": true, "close": true, "complex": true, "copy": true, "delete": true,
		"imag": true, "len": true, "make": true, "max": true, "min": true, "new": true, "panic": true, "print": true,
		"println": true, "real": true, "recover": true,
	}
}

// named resolves a type to a named type of the module: its package, its
// name and declaration. ok is false for anything else; ext is true when it
// is a type outside the module or a builtin.
func (m *codeModule) named(t typ) (p *codePkg, name string, decl typeDecl, ext, ok bool) {
	if t.ext {
		return nil, "", typeDecl{}, true, false
	}
	e := unparen(t.expr)
	if star, isStar := e.(*ast.StarExpr); isStar {
		e = unparen(star.X)
	}
	switch x := e.(type) {
	case *ast.IndexExpr:
		e = x.X
	case *ast.IndexListExpr:
		e = x.X
	}
	switch x := e.(type) {
	case *ast.Ident:
		if builtinTypes()[x.Name] {
			return nil, "", typeDecl{}, true, false
		}
		if d, found := t.pkg.types[x.Name]; found {
			return t.pkg, x.Name, d, false, true
		}
	case *ast.SelectorExpr:
		id, isIdent := x.X.(*ast.Ident)
		if !isIdent {
			return nil, "", typeDecl{}, false, false
		}
		q, imports := m.imported(t.file, id.Name)
		if !imports {
			return nil, "", typeDecl{}, false, false
		}
		if q == nil {
			return nil, "", typeDecl{}, true, false
		}
		if d, found := q.types[x.Sel.Name]; found {
			return q, x.Sel.Name, d, false, true
		}
	}
	return nil, "", typeDecl{}, false, false
}

// underlying is the type expression a named type stands for, or t itself.
func (m *codeModule) underlying(t typ) typ {
	for range 8 {
		p, _, d, _, ok := m.named(t)
		if !ok {
			return t
		}
		t = typ{pkg: p, file: d.file, expr: d.spec.Type}
	}
	return t
}

// elem is the element type of a slice, array, map (its values) or channel.
func (m *codeModule) elem(t typ) (typ, bool) {
	if t.ext {
		return typ{ext: true}, true
	}
	u := m.underlying(t)
	if u.ext {
		return typ{ext: true}, true
	}
	switch x := unparen(u.expr).(type) {
	case *ast.ArrayType:
		return typ{pkg: u.pkg, file: u.file, expr: x.Elt}, true
	case *ast.MapType:
		return typ{pkg: u.pkg, file: u.file, expr: x.Value}, true
	case *ast.ChanType:
		return typ{pkg: u.pkg, file: u.file, expr: x.Value}, true
	case *ast.Ident:
		if x.Name == "string" {
			return typ{ext: true}, true
		}
	}
	return typ{}, false
}

// field finds a field (or one embedded a level down) of a struct type.
func (m *codeModule) field(t typ, name string) (typ, bool) {
	u := m.underlying(t)
	st, ok := unparen(u.expr).(*ast.StructType)
	if u.ext || !ok {
		return typ{}, false
	}
	var embedded []typ
	for _, fld := range st.Fields.List {
		ft := typ{pkg: u.pkg, file: u.file, expr: fld.Type}
		if len(fld.Names) == 0 {
			embedded = append(embedded, ft)
			continue
		}
		for _, n := range fld.Names {
			if n.Name == name {
				return ft, true
			}
		}
	}
	for _, e := range embedded {
		if _, _, _, ext, _ := m.named(e); ext {
			continue
		}
		if f, ok := m.field(e, name); ok {
			return f, true
		}
	}
	return typ{}, false
}

// interfaceMethods returns the methods of an interface type, by name, with
// their signatures; embedded interfaces of the module are flattened. ok is
// false when t is not an interface.
func (m *codeModule) interfaceMethods(t typ) (map[string]typ, bool) {
	u := m.underlying(t)
	it, ok := unparen(u.expr).(*ast.InterfaceType)
	if u.ext || !ok {
		return nil, false
	}
	out := map[string]typ{}
	for _, fld := range it.Methods.List {
		if len(fld.Names) == 0 {
			if inner, ok := m.interfaceMethods(typ{pkg: u.pkg, file: u.file, expr: fld.Type}); ok {
				maps.Copy(out, inner)
			}
			continue
		}
		for _, n := range fld.Names {
			out[n.Name] = typ{pkg: u.pkg, file: u.file, expr: fld.Type}
		}
	}
	return out, true
}

// implementations returns the methods named method of every type of the
// module whose methods include all of the interface's.
func (m *codeModule) implementations(methods map[string]typ, method string) []fnRef {
	var out []fnRef
	for _, path := range slices.Sorted(maps.Keys(m.pkgs)) {
		p := m.pkgs[path]
		for _, tn := range slices.Sorted(maps.Keys(p.methods)) {
			set := p.methods[tn]
			all := true
			for name := range methods {
				if _, ok := set[name]; !ok {
					all = false
					break
				}
			}
			if all {
				out = append(out, set[method])
			}
		}
	}
	return out
}

// returnsError reports whether a function type's results include error,
// or whether the signature is unknown.
func returnsError(t typ) bool {
	ft, ok := unparen(t.expr).(*ast.FuncType)
	if !ok {
		return true
	}
	if ft.Results == nil {
		return false
	}
	for _, r := range ft.Results.List {
		if id, ok := r.Type.(*ast.Ident); ok && id.Name == "error" {
			return true
		}
	}
	return false
}

// result is the i-th result type of a function type.
func result(sig typ, i int) (typ, bool) {
	ft, ok := unparen(sig.expr).(*ast.FuncType)
	if sig.ext || !ok || ft.Results == nil {
		return typ{ext: sig.ext}, sig.ext
	}
	n := 0
	for _, r := range ft.Results.List {
		for range max(1, len(r.Names)) {
			if n == i {
				return typ{pkg: sig.pkg, file: sig.file, expr: r.Type}, true
			}
			n++
		}
	}
	return typ{}, false
}

// errorTrace is the walk from one handler.
type errorTrace struct {
	m        *codeModule
	seen     map[*ast.FuncDecl]bool
	seenVars map[ast.Expr]bool
	// followed counts the calls resolved to a declaration of the module,
	// entered or not: a trace that follows none never left its handler.
	followed   int
	sites      []errorSite
	unresolved []unresolved
}

// scope is what the walk knows inside one declaration: its receiver, its
// parameters and locals, and its function literals bound to a name.
type scope struct {
	fn        fnRef
	recvName  string
	recvType  string
	vars      map[string]typ
	params    map[string]bool // function-typed parameters: their callers' arguments are visited
	closures  map[string]bool // names bound to a function literal: the literal is in this body
	bound     map[*ast.FuncLit]string
	shadowing map[string]bool // locals that shadow an import or a package-level name
}

// trace returns the error sites reachable from entry, what it could not
// resolve, and how many calls it resolved to a declaration of the module.
func (m *codeModule) trace(entry fnRef) (sites []errorSite, unres []unresolved, followed int) {
	tr := &errorTrace{m: m, seen: map[*ast.FuncDecl]bool{}, seenVars: map[ast.Expr]bool{}}
	tr.visit(entry)
	return tr.sites, tr.unresolved, tr.followed
}

func (tr *errorTrace) visit(fn fnRef) {
	if tr.seen[fn.decl] || fn.decl.Body == nil || !fn.pkg.capable {
		return
	}
	tr.seen[fn.decl] = true
	sc := tr.newScope(fn)
	tr.walk(sc, fn.decl.Body)
}

// newScope declares the receiver, the parameters, the named results and
// every local of a declaration, in source order.
func (tr *errorTrace) newScope(fn fnRef) *scope {
	sc := &scope{
		fn: fn, vars: map[string]typ{}, params: map[string]bool{}, closures: map[string]bool{},
		bound: closureNames(fn.decl.Body), shadowing: map[string]bool{},
	}
	sc.recvName, sc.recvType = receiver(fn.decl)
	tr.declareFields(sc, fn.decl.Type.Params)
	tr.declareFields(sc, fn.decl.Type.Results)
	for _, name := range sc.bound {
		sc.closures[name] = true
	}
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			tr.declareFields(sc, x.Type.Params)
			tr.declareFields(sc, x.Type.Results)
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				tr.declareAssign(sc, x.Lhs, x.Rhs)
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				switch {
				case x.Type != nil:
					tr.declare(sc, id.Name, typ{pkg: fn.pkg, file: fn.file, expr: x.Type})
				case i < len(x.Values):
					if t, ok := tr.exprType(sc, x.Values[i]); ok {
						tr.declare(sc, id.Name, t)
					}
				}
			}
		case *ast.RangeStmt:
			tr.declareRange(sc, x)
		}
		return true
	})
	return sc
}

func (tr *errorTrace) declare(sc *scope, name string, t typ) {
	if name == "_" || name == "" {
		return
	}
	if _, done := sc.vars[name]; done {
		return // the first declaration wins
	}
	sc.vars[name] = t
	if _, imports := tr.m.imported(sc.fn.file, name); imports {
		sc.shadowing[name] = true
	}
}

func (tr *errorTrace) declareFields(sc *scope, fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, fld := range fl.List {
		t := typ{pkg: sc.fn.pkg, file: sc.fn.file, expr: fld.Type}
		_, isFunc := unparen(tr.m.underlying(t).expr).(*ast.FuncType)
		for _, n := range fld.Names {
			tr.declare(sc, n.Name, t)
			if isFunc {
				sc.params[n.Name] = true
			}
		}
	}
}

func (tr *errorTrace) declareAssign(sc *scope, lhs, rhs []ast.Expr) {
	names := make([]string, len(lhs))
	for i, l := range lhs {
		if id, ok := l.(*ast.Ident); ok {
			names[i] = id.Name
		}
	}
	if len(lhs) == len(rhs) {
		for i, r := range rhs {
			if t, ok := tr.exprType(sc, r); ok {
				tr.declare(sc, names[i], t)
			}
		}
		return
	}
	if len(rhs) != 1 {
		return
	}
	switch r := unparen(rhs[0]).(type) {
	case *ast.CallExpr:
		c := tr.resolveCall(sc, r)
		for i, name := range names {
			if t, ok := result(c.sig, i); ok {
				tr.declare(sc, name, t)
			}
		}
	case *ast.TypeAssertExpr:
		if r.Type != nil {
			tr.declare(sc, names[0], typ{pkg: sc.fn.pkg, file: sc.fn.file, expr: r.Type})
		}
		tr.declare(sc, names[1], typ{ext: true})
	case *ast.IndexExpr:
		if t, ok := tr.exprType(sc, r); ok {
			tr.declare(sc, names[0], t)
		}
		tr.declare(sc, names[1], typ{ext: true})
	case *ast.UnaryExpr: // v, ok := <-ch
		if t, ok := tr.exprType(sc, r); ok {
			tr.declare(sc, names[0], t)
		}
		tr.declare(sc, names[1], typ{ext: true})
	}
}

func (tr *errorTrace) declareRange(sc *scope, r *ast.RangeStmt) {
	if r.Tok != token.DEFINE {
		return
	}
	over, ok := tr.exprType(sc, r.X)
	if !ok {
		return
	}
	key, _ := r.Key.(*ast.Ident)
	value, _ := r.Value.(*ast.Ident)
	u := tr.m.underlying(over)
	if mt, isMap := unparen(u.expr).(*ast.MapType); isMap && !u.ext {
		if key != nil {
			tr.declare(sc, key.Name, typ{pkg: u.pkg, file: u.file, expr: mt.Key})
		}
	} else if key != nil {
		tr.declare(sc, key.Name, typ{ext: true}) // an index, a rune position or an int
	}
	if value != nil {
		if e, ok := tr.m.elem(over); ok {
			tr.declare(sc, value.Name, e)
		}
	}
}

// exprType infers the type of a value expression.
func (tr *errorTrace) exprType(sc *scope, e ast.Expr) (typ, bool) {
	switch x := unparen(e).(type) {
	case *ast.Ident:
		switch {
		case x.Name == "nil" || x.Name == "true" || x.Name == "false" || x.Name == "iota":
			return typ{ext: true}, true
		case x.Name == sc.recvName && sc.recvName != "":
			return typ{pkg: sc.fn.pkg, file: sc.fn.file, expr: ast.NewIdent(sc.recvType)}, true
		}
		if t, ok := sc.vars[x.Name]; ok {
			return t, true
		}
		if v, ok := sc.fn.pkg.vars[x.Name]; ok {
			return tr.varType(sc.fn.pkg, v)
		}
		if _, ok := sc.fn.pkg.consts[x.Name]; ok {
			return typ{ext: true}, true
		}
		if f, ok := sc.fn.pkg.funcs[x.Name]; ok {
			return typ{pkg: f.pkg, file: f.file, expr: f.decl.Type}, true
		}
	case *ast.BasicLit, *ast.BinaryExpr, *ast.FuncLit:
		if lit, ok := x.(*ast.FuncLit); ok {
			return typ{pkg: sc.fn.pkg, file: sc.fn.file, expr: lit.Type}, true
		}
		return typ{ext: true}, true
	case *ast.SelectorExpr:
		return tr.selectorType(sc, x)
	case *ast.CallExpr:
		c := tr.resolveCall(sc, x)
		if c.conversion.expr != nil || c.conversion.ext {
			return c.conversion, true
		}
		if c.builtin != "" {
			return tr.builtinResult(sc, c.builtin, x)
		}
		return result(c.sig, 0)
	case *ast.StarExpr:
		return tr.exprType(sc, x.X)
	case *ast.UnaryExpr:
		if x.Op == token.ARROW {
			if t, ok := tr.exprType(sc, x.X); ok {
				return tr.m.elem(t)
			}
			return typ{}, false
		}
		return tr.exprType(sc, x.X)
	case *ast.CompositeLit:
		if x.Type != nil {
			return typ{pkg: sc.fn.pkg, file: sc.fn.file, expr: x.Type}, true
		}
	case *ast.IndexExpr:
		if t, ok := tr.exprType(sc, x.X); ok {
			return tr.m.elem(t)
		}
	case *ast.SliceExpr:
		return tr.exprType(sc, x.X)
	case *ast.TypeAssertExpr:
		if x.Type != nil {
			return typ{pkg: sc.fn.pkg, file: sc.fn.file, expr: x.Type}, true
		}
	}
	return typ{}, false
}

func (tr *errorTrace) varType(p *codePkg, v varDecl) (typ, bool) {
	if v.typ != nil {
		return typ{pkg: p, file: v.file, expr: v.typ}, true
	}
	if v.value == nil {
		return typ{}, false
	}
	// A package-level initializer: its type is ext unless it is a
	// composite or a call of the module, which the trace types without a
	// scope of locals.
	sc := &scope{fn: fnRef{pkg: p, file: v.file, decl: &ast.FuncDecl{Name: ast.NewIdent("var"), Type: &ast.FuncType{}}}, vars: map[string]typ{}}
	return tr.exprType(sc, v.value)
}

func (tr *errorTrace) selectorType(sc *scope, x *ast.SelectorExpr) (typ, bool) {
	if id, ok := x.X.(*ast.Ident); ok && !sc.shadowing[id.Name] && id.Name != sc.recvName {
		if _, local := sc.vars[id.Name]; !local {
			if q, imports := tr.m.imported(sc.fn.file, id.Name); imports {
				if q == nil {
					return typ{ext: true}, true
				}
				if v, ok := q.vars[x.Sel.Name]; ok {
					return tr.varType(q, v)
				}
				if f, ok := q.funcs[x.Sel.Name]; ok {
					return typ{pkg: f.pkg, file: f.file, expr: f.decl.Type}, true
				}
				return typ{ext: true}, true // a constant or a type of the module
			}
		}
	}
	base, ok := tr.exprType(sc, x.X)
	if !ok {
		return typ{}, false
	}
	if base.ext {
		return typ{ext: true}, true
	}
	if f, ok := tr.m.field(base, x.Sel.Name); ok {
		return f, true
	}
	if p, name, _, ext, ok := tr.m.named(base); ext {
		return typ{ext: true}, true
	} else if ok {
		if meth, found := p.methods[name][x.Sel.Name]; found {
			return typ{pkg: meth.pkg, file: meth.file, expr: meth.decl.Type}, true
		}
		if ms, isIface := tr.m.interfaceMethods(base); isIface {
			if sig, found := ms[x.Sel.Name]; found {
				return sig, true
			}
		}
	}
	return typ{}, false
}

func (tr *errorTrace) builtinResult(sc *scope, name string, call *ast.CallExpr) (typ, bool) {
	switch name {
	case "append":
		if len(call.Args) > 0 {
			return tr.exprType(sc, call.Args[0])
		}
	case "make", "new":
		if len(call.Args) > 0 {
			return typ{pkg: sc.fn.pkg, file: sc.fn.file, expr: call.Args[0]}, true
		}
	default:
		return typ{ext: true}, true
	}
	return typ{}, false
}

// callee is what a call resolves to.
type callee struct {
	fn         *fnRef  // a declaration of the module
	impls      []fnRef // an interface method's implementations
	sig        typ     // the signature, for result types
	conversion typ     // the target of a conversion
	builtin    string  // a predeclared function
	skip       bool    // resolved, and cannot construct a module code
	missing    string  // unresolved: the key unresolvedCalls takes ("" for none)
	why        string  // unresolved: what is missing
	apperr     string  // apperr.New or apperr.Wrap: a site
}

// resolveCall resolves a call's callee.
func (tr *errorTrace) resolveCall(sc *scope, call *ast.CallExpr) callee {
	fun := unparen(call.Fun)
	switch x := fun.(type) {
	case *ast.IndexExpr: // a generic function F[T](…), or an element m[k](…)
		if t, ok := tr.exprType(sc, x.X); ok {
			if u := tr.m.underlying(t); !u.ext {
				if _, isFunc := unparen(u.expr).(*ast.FuncType); !isFunc {
					return tr.indexedCallee(sc, call, t)
				}
			}
		}
		fun = x.X
	case *ast.IndexListExpr:
		fun = x.X
	}
	switch x := fun.(type) {
	case *ast.Ident:
		return tr.resolveName(sc, x.Name)
	case *ast.SelectorExpr:
		return tr.resolveSelector(sc, x)
	case *ast.FuncLit:
		return callee{skip: true, sig: typ{pkg: sc.fn.pkg, file: sc.fn.file, expr: x.Type}} // its body is in this one
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StarExpr:
		return callee{conversion: typ{pkg: sc.fn.pkg, file: sc.fn.file, expr: fun}}
	}
	return callee{why: "a computed callee", missing: tr.key(sc, call.Fun)}
}

// indexedCallee is the callee of m[k](…): an element of a map, slice or
// array of functions, put there somewhere the walk does not follow. One
// that returns an error is unresolved.
func (tr *errorTrace) indexedCallee(sc *scope, call *ast.CallExpr, container typ) callee {
	elem, ok := tr.m.elem(container)
	if !ok {
		return callee{why: "an indexed callee of unknown type", missing: tr.key(sc, call.Fun)}
	}
	if elem.ext {
		return callee{skip: true, sig: elem}
	}
	u := tr.m.underlying(elem)
	if _, isFunc := unparen(u.expr).(*ast.FuncType); isFunc && !returnsError(u) {
		return callee{skip: true, sig: u}
	}
	return callee{sig: u, why: "an indexed callee", missing: tr.key(sc, call.Fun)}
}

func (tr *errorTrace) resolveName(sc *scope, name string) callee {
	if t, local := sc.vars[name]; local {
		switch {
		case sc.closures[name], sc.params[name]:
			// A literal in this body, or a callback whose arguments the
			// callers' walks visit.
			return callee{skip: true, sig: t}
		case !returnsError(tr.m.underlying(t)):
			return callee{skip: true, sig: t}
		}
		return callee{why: "a call of a local function value", missing: ""}
	}
	if builtinFuncs()[name] {
		return callee{builtin: name}
	}
	if builtinTypes()[name] {
		return callee{conversion: typ{ext: true}}
	}
	p := sc.fn.pkg
	if f, ok := p.funcs[name]; ok {
		return callee{fn: &f, sig: typ{pkg: f.pkg, file: f.file, expr: f.decl.Type}}
	}
	if _, ok := p.types[name]; ok {
		return callee{conversion: typ{pkg: p, file: sc.fn.file, expr: ast.NewIdent(name)}}
	}
	if v, ok := p.vars[name]; ok {
		if t, ok := tr.varType(p, v); ok && !returnsError(tr.m.underlying(t)) {
			return callee{skip: true, sig: t}
		}
	}
	return callee{why: "a call of an unknown name " + name}
}

func (tr *errorTrace) resolveSelector(sc *scope, x *ast.SelectorExpr) callee {
	if id, ok := x.X.(*ast.Ident); ok && !sc.shadowing[id.Name] && id.Name != sc.recvName {
		if _, local := sc.vars[id.Name]; !local {
			if q, imports := tr.m.imported(sc.fn.file, id.Name); imports {
				return tr.resolvePackageMember(sc, q, id.Name, x.Sel.Name)
			}
		}
	}
	base, ok := tr.exprType(sc, x.X)
	if !ok {
		return callee{why: "a receiver of unknown type", missing: tr.key(sc, x)}
	}
	if base.ext {
		return callee{skip: true, sig: typ{ext: true}}
	}
	if f, ok := tr.m.field(base, x.Sel.Name); ok {
		// A function-typed field.
		u := tr.m.underlying(f)
		if returnsError(u) {
			p, name, _, _, named := tr.m.named(f)
			key := tr.key(sc, x)
			if named {
				key = p.rel + "." + name
			}
			return callee{sig: u, why: "a function-typed field", missing: key}
		}
		return callee{skip: true, sig: u}
	}
	p, name, _, ext, ok := tr.m.named(base)
	if ext {
		return callee{skip: true, sig: typ{ext: true}}
	}
	if !ok {
		return callee{why: "a receiver whose type is not a named type of the module", missing: tr.key(sc, x)}
	}
	if meth, found := p.methods[name][x.Sel.Name]; found {
		return callee{fn: &meth, sig: typ{pkg: meth.pkg, file: meth.file, expr: meth.decl.Type}, skip: !p.capable}
	}
	if ms, isIface := tr.m.interfaceMethods(base); isIface {
		sig, found := ms[x.Sel.Name]
		if !found {
			sig = typ{ext: true} // a method of an embedded interface outside the module
		}
		impls := tr.m.implementations(ms, x.Sel.Name)
		if len(impls) == 0 && returnsError(sig) {
			return callee{sig: sig, why: "an interface of the module that no type implements", missing: p.rel + "." + name}
		}
		return callee{impls: impls, sig: sig}
	}
	if !p.capable {
		return callee{skip: true}
	}
	return callee{why: "a method " + x.Sel.Name + " not declared on " + name, missing: tr.key(sc, x)}
}

func (tr *errorTrace) resolvePackageMember(sc *scope, q *codePkg, local, name string) callee {
	if local == importName(sc.fn.file, apperrPath) {
		if name == "New" || name == "Wrap" {
			return callee{apperr: name, skip: true, sig: tr.apperrSig(name)}
		}
		return callee{skip: true, sig: tr.apperrSig(name)}
	}
	if q == nil {
		return callee{skip: true, sig: typ{ext: true}}
	}
	if f, ok := q.funcs[name]; ok {
		return callee{fn: &f, sig: typ{pkg: f.pkg, file: f.file, expr: f.decl.Type}, skip: !q.capable}
	}
	if _, ok := q.types[name]; ok {
		return callee{conversion: typ{pkg: q, file: q.anyFile(name), expr: ast.NewIdent(name)}}
	}
	if !q.capable {
		return callee{skip: true}
	}
	return callee{why: "an unknown member " + name + " of " + q.rel}
}

// apperrSig is the signature of an apperr function, for its result type.
func (tr *errorTrace) apperrSig(name string) typ {
	if p := tr.m.pkgs[apperrPath]; p != nil {
		if f, ok := p.funcs[name]; ok {
			return typ{pkg: p, file: f.file, expr: f.decl.Type}
		}
	}
	return typ{ext: true}
}

// anyFile is the file that declares the named type.
func (p *codePkg) anyFile(name string) *ast.File {
	return p.types[name].file
}

// key names an unresolved expression for unresolvedCalls.
func (tr *errorTrace) key(sc *scope, e ast.Expr) string {
	fn := sc.fn.decl.Name.Name
	if sc.recvType != "" {
		fn = sc.recvType + "." + fn
	}
	return sc.fn.pkg.rel + "." + fn + ": " + types.ExprString(e)
}

// walk visits the calls and the package-level variables of a body.
func (tr *errorTrace) walk(sc *scope, body ast.Node) {
	var stack []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch x := n.(type) {
		case *ast.CallExpr:
			tr.call(sc, x, stack)
		case *ast.Ident:
			if _, local := sc.vars[x.Name]; !local {
				if v, ok := sc.fn.pkg.vars[x.Name]; ok && v.value != nil {
					tr.walkVar(sc.fn.pkg, v)
				}
			}
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && !sc.shadowing[id.Name] {
				if _, local := sc.vars[id.Name]; !local {
					if q, imports := tr.m.imported(sc.fn.file, id.Name); imports && q != nil && q.capable {
						if v, ok := q.vars[x.Sel.Name]; ok && v.value != nil {
							tr.walkVar(q, v)
						}
					}
				}
			}
		}
		return true
	})
}

// walkVar treats a package-level variable's initializer as a body: a
// `var errX = apperr.New(…)` is a site wherever errX is named.
func (tr *errorTrace) walkVar(p *codePkg, v varDecl) {
	if tr.seenVars[v.value] || !p.capable {
		return
	}
	tr.seenVars[v.value] = true
	sc := &scope{
		fn:   fnRef{pkg: p, file: v.file, decl: &ast.FuncDecl{Name: ast.NewIdent("var"), Type: &ast.FuncType{}}},
		vars: map[string]typ{}, params: map[string]bool{}, closures: map[string]bool{},
		bound: map[*ast.FuncLit]string{}, shadowing: map[string]bool{},
	}
	tr.walk(sc, v.value)
}

func (tr *errorTrace) call(sc *scope, call *ast.CallExpr, stack []ast.Node) {
	c := tr.resolveCall(sc, call)
	if c.fn != nil || len(c.impls) > 0 {
		tr.followed++
	}
	switch {
	case c.apperr != "":
		tr.site(sc, c.apperr, call, stack)
	case c.fn != nil && !c.skip:
		tr.visit(*c.fn)
	case len(c.impls) > 0:
		for _, impl := range c.impls {
			tr.visit(impl)
		}
	case c.why != "":
		tr.unresolved = append(tr.unresolved, unresolved{
			key: c.missing, pos: tr.m.fset.Position(call.Pos()).String(), why: c.why,
		})
	}
	// Function values passed on are visited as if called: a callback's
	// codes come back through its caller, since the callee's call of its
	// parameter is not followed (resolveName).
	for i, arg := range call.Args {
		tr.passedOn(sc, c, i, arg)
	}
}

// passedOn visits a function value passed as the i-th argument of a call
// to c: a package function, plain or instantiated (F[T]), a method value
// (of a type, or of an interface to its implementations). What cannot be
// followed is red, like a call: a function-typed field or an
// unimplemented interface method, a local function value that is neither
// a literal of this body nor a parameter, a package-level function value,
// an element of a container of functions, and any unresolved selector the
// callee takes as a function returning an error.
func (tr *errorTrace) passedOn(sc *scope, c callee, i int, arg ast.Expr) {
	switch a := unparen(arg).(type) {
	case *ast.Ident:
		if t, local := sc.vars[a.Name]; local {
			if !sc.closures[a.Name] && !sc.params[a.Name] && tr.isErrorFunc(t) {
				tr.unresolvedAt(a, tr.key(sc, a), "a local function value passed on")
			}
			return
		}
		if f, ok := sc.fn.pkg.funcs[a.Name]; ok {
			tr.visit(f)
			return
		}
		if v, ok := sc.fn.pkg.vars[a.Name]; ok {
			if t, typed := tr.varType(sc.fn.pkg, v); (typed && tr.isErrorFunc(t)) || tr.takesErrorFunc(c, i) {
				tr.unresolvedAt(a, tr.key(sc, a), "a package-level function value passed on")
			}
		}
	case *ast.IndexExpr:
		tr.passedIndexed(sc, c, i, a, a.X)
	case *ast.IndexListExpr:
		tr.passedIndexed(sc, c, i, a, a.X)
	case *ast.SelectorExpr:
		v := tr.resolveSelector(sc, a)
		switch {
		case v.fn != nil && !v.skip:
			tr.visit(*v.fn)
		case len(v.impls) > 0:
			for _, impl := range v.impls {
				tr.visit(impl)
			}
		case v.why != "" && (tr.isErrorFunc(v.sig) || tr.takesErrorFunc(c, i)):
			tr.unresolvedAt(a, v.missing, v.why+", passed on")
		}
	}
}

// passedIndexed is passedOn for x[…]: an instantiated generic function is
// followed like the function itself; an element of a container of
// functions that return an error is red, its value put there elsewhere.
func (tr *errorTrace) passedIndexed(sc *scope, c callee, i int, arg, x ast.Expr) {
	if t, ok := tr.exprType(sc, x); ok && !t.ext {
		if _, isFunc := unparen(tr.m.underlying(t).expr).(*ast.FuncType); isFunc {
			tr.passedOn(sc, c, i, x)
			return
		}
	}
	if t, ok := tr.exprType(sc, arg); (ok && tr.isErrorFunc(t)) || (!ok && tr.takesErrorFunc(c, i)) {
		tr.unresolvedAt(arg, tr.key(sc, arg), "an indexed function value passed on")
	}
}

// isErrorFunc reports whether t is a function type that returns an error.
func (tr *errorTrace) isErrorFunc(t typ) bool {
	if t.ext || t.expr == nil {
		return false
	}
	u := tr.m.underlying(t)
	_, isFunc := unparen(u.expr).(*ast.FuncType)
	return isFunc && returnsError(u)
}

// takesErrorFunc reports whether c's i-th parameter is a function that
// returns an error.
func (tr *errorTrace) takesErrorFunc(c callee, i int) bool {
	ft, ok := unparen(c.sig.expr).(*ast.FuncType)
	if c.sig.ext || !ok || ft.Params == nil {
		return false
	}
	var params []ast.Expr
	for _, p := range ft.Params.List {
		for range max(1, len(p.Names)) {
			params = append(params, p.Type)
		}
	}
	if len(params) == 0 {
		return false
	}
	t := params[min(i, len(params)-1)]
	if ell, variadic := t.(*ast.Ellipsis); variadic {
		t = ell.Elt
	} else if i >= len(params) {
		return false
	}
	return tr.isErrorFunc(typ{pkg: c.sig.pkg, file: c.sig.file, expr: t})
}

// unresolvedAt records what the walk could not follow at e.
func (tr *errorTrace) unresolvedAt(e ast.Expr, key, why string) {
	tr.unresolved = append(tr.unresolved, unresolved{key: key, pos: tr.m.fset.Position(e.Pos()).String(), why: why})
}

// site records an apperr.New or apperr.Wrap call.
func (tr *errorTrace) site(sc *scope, name string, call *ast.CallExpr, stack []ast.Node) {
	kindAt, codeAt := 0, 1
	if name == "Wrap" {
		kindAt, codeAt = 1, 2
	}
	pos := tr.m.fset.Position(call.Pos()).String()
	if len(call.Args) <= codeAt {
		tr.unresolved = append(tr.unresolved, unresolved{pos: pos, why: "apperr." + name + " with too few arguments"})
		return
	}
	kind := kindName(sc.fn.file, call.Args[kindAt])
	codes := tr.codeValues(sc, call.Args[codeAt], stack)
	if kind == "" || codes == nil {
		tr.unresolved = append(tr.unresolved, unresolved{
			pos: pos, why: "apperr." + name + " whose kind or code is not a constant; write both as constants at the call",
		})
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

// constValue is the string constant e names in its package or an
// imported one.
func (tr *errorTrace) constValue(sc *scope, e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.Ident:
		if _, local := sc.vars[x.Name]; local {
			return "", false
		}
		v, ok := sc.fn.pkg.consts[x.Name]
		return v, ok
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		if q, _ := tr.m.imported(sc.fn.file, id.Name); q != nil {
			v, ok := q.consts[x.Sel.Name]
			return v, ok
		}
	}
	return "", false
}

// codeValues are the codes e can be: one constant, or, for a parameter of
// the enclosing function literal bound to a local name, the constant each
// call of that name passes. nil when any of them is not a constant.
func (tr *errorTrace) codeValues(sc *scope, e ast.Expr, stack []ast.Node) []string {
	if v, ok := tr.constValue(sc, e); ok {
		return []string{v}
	}
	id, ok := e.(*ast.Ident)
	if !ok || sc.fn.decl.Body == nil {
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
		name := sc.bound[lit]
		if name == "" {
			return nil
		}
		var out []string
		complete := true
		ast.Inspect(sc.fn.decl.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if fun, ok := call.Fun.(*ast.Ident); !ok || fun.Name != name {
				return true
			}
			if at >= len(call.Args) {
				complete = false
				return true
			}
			v, ok := tr.constValue(sc, call.Args[at])
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
	if body == nil {
		return out
	}
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
	universal, unseen map[string]string,
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

// exemptionFindings checks the reasoned exemption lists against the
// contract: every reason has at least minExemptionReason characters, every
// unseen entry names an operation with a handler and a code it lists.
func exemptionFindings(listed map[string][]string, nonStrict, calls map[string]string,
	unseen map[string]map[string]string,
) []string {
	var out []string
	reason := func(what, why string) {
		if len(strings.TrimSpace(why)) < minExemptionReason {
			out = append(out, what+": the reason needs at least "+strconv.Itoa(minExemptionReason)+" characters")
		}
	}
	for op, why := range nonStrict {
		reason("nonStrictOperations "+op, why)
	}
	for key, why := range calls {
		reason("unresolvedCalls "+key, why)
	}
	for op, codes := range unseen {
		codesOf, ok := listed[op]
		if !ok {
			out = append(out, "unseenCodes names "+op+", which is not an operation with a handler")
		}
		for code, why := range codes {
			reason("unseenCodes "+op+" "+code, why)
			if ok && !slices.Contains(codesOf, code) {
				out = append(out, "unseenCodes exempts "+code+" of "+op+", which its x-error-codes does not list")
			}
		}
	}
	slices.Sort(out)
	return out
}

// loadCodeModule indexes every non-test file of the module, the generated
// ones included (a method of a generated type is a method like any other).
func loadCodeModule(t *testing.T) *codeModule {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string][]*ast.File{}
	err := filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name != "." && name != ".." && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "bin" || name == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(moduleRoot, filepath.Dir(path))
		if err != nil {
			return err
		}
		importPath := modulePath + "/" + filepath.ToSlash(rel)
		files[importPath] = append(files[importPath], f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
// operations has a floor, named sites must be among the traced ones (one
// through the closure resolution, one through an interface, the two empty
// lists through a helper), and a trace that never leaves its handler is
// red, so a trace that finds nothing cannot pass.
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
	listedByOp := map[string][]string{}
	ops := map[string]bool{}
	usedExemptions := map[string]bool{}
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
			sites, unres, followed := m.trace(found[0])
			if followed == 0 {
				t.Errorf("%s: the trace never left the handler; it no longer follows its calls", where)
			}
			for _, u := range unres {
				if _, ok := unresolvedCalls()[u.key]; ok && u.key != "" {
					usedExemptions[u.key] = true
					continue
				}
				t.Errorf("%s: %s: %s (exemption key %q)", where, u.pos, u.why, u.key)
			}
			listed := operationCodes(t, where, op.Extensions)
			listedByOp[op.OperationID] = listed
			for _, f := range bindingFindings(op.OperationID, listed, sites, documented, kinds, universal, unseen[op.OperationID]) {
				t.Error(f)
			}
			traced[op.OperationID] = sites
		}
	}

	// Stale and malformed exemptions.
	for _, f := range exemptionFindings(listedByOp, nonStrictOperations(), unresolvedCalls(), unseen) {
		t.Error(f)
	}
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
	for key := range unresolvedCalls() {
		if !usedExemptions[key] {
			t.Errorf("unresolvedCalls exempts %q, which no trace meets any more; drop the entry", key)
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
		"getStaffMe":            {kind: "KindUnauthenticated", code: "STAFF_SESSION_ENDED"}, // through staff's accounts interface
		"completeRegistration":  {kind: "KindConflict", code: "VERIFICATION_TARGET_IN_USE"},
		"completePasswordReset": {kind: "KindInvalid", code: "PASSWORD_RESET_TOKEN_INVALID"},
		"changeMyPassword":      {kind: "KindForbidden", code: "PASSWORD_MISMATCH"},
		// The two operations with an empty list: their only sites are the
		// platform's, reached through a helper of the module.
		"startRegistration":    {kind: "KindInvalid", code: "REQUEST_INVALID"},
		"requestPasswordReset": {kind: "KindInvalid", code: "REQUEST_INVALID"},
	} {
		if !slices.ContainsFunc(traced[op], func(s errorSite) bool { return s.kind == want.kind && s.code == want.code }) {
			t.Errorf("the trace of %s does not reach %s %s; it no longer sees the handler's sites", op, want.kind, want.code)
		}
	}
}

// TestPackageNames pins the names an unnamed import goes by: the real
// repository's versioned and dashed paths among them.
func TestPackageNames(t *testing.T) {
	t.Parallel()

	for path, want := range map[string]string{
		"github.com/golang-jwt/jwt/v5": "jwt",
		"github.com/jackc/pgx/v5":      "pgx",
		"github.com/wneessen/go-mail":  "mail",
		"log/slog":                     "slog",
		"github.com/riverqueue/river":  "river",
	} {
		if got := packageNames(path); !slices.Contains(got, want) {
			t.Errorf("packageNames(%q) = %v, want it to include %q", path, got, want)
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

// demoModule is a small module the trace's own tests walk.
func demoModule(t *testing.T) *codeModule {
	t.Helper()
	const imports = `import (
	"context"
	"log/slog"

	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/demo/other"
	"github.com/is-koala/koala-api/internal/demo/quiet"
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
	CodeVar    apperr.Code = "DEMO_VAR"
	CodeImpl   apperr.Code = "DEMO_IMPL"
	CodePassed  apperr.Code = "DEMO_PASSED"
	CodeIndexed apperr.Code = "DEMO_INDEXED"
	CodeGeneric apperr.Code = "DEMO_GENERIC"
)

var errVar = apperr.New(apperr.KindConflict, CodeVar)

type hasher interface{ Hash() error }

type sender interface{ Send() error }

type passer interface{ Pass() error }

type doer interface{ Do() error }

type Passer struct{}

func (Passer) Pass() error { return apperr.New(apperr.KindConflict, CodePassed) }

type Indexed struct{}

func (Indexed) Get() error { return apperr.New(apperr.KindNotFound, CodeIndexed) }

var hookVar func() error

type Store struct{}

type Impl struct{}

func (Impl) Hash() error { return apperr.New(apperr.KindNotFound, CodeImpl) }

type HTTP struct {
	users  *Service
	staff  *Staff
	hasher hasher
	quiet  quiet.Box
	log    *slog.Logger
	hook   func() error
	passer passer
	svcs   map[string]Indexed
	fns    map[string]func() error
	quits  []func()
}

type Service struct{ inner *Inner }

type Inner struct{}

type Staff struct{}

func newStore() *Store { return &Store{} }

func (*Store) load() error { return errVar }

func pick[T any](v T) T { return v }

func (h *HTTP) GetThing(ctx context.Context, _ apiv1.GetThingRequestObject) (any, error) {
	if err := h.users.Login(); err != nil {
		return nil, err
	}
	if err := other.Check(); err != nil {
		return nil, err
	}
	_ = h.hasher.Hash()
	_ = h.quiet.Open()
	h.log.InfoContext(ctx, "m")
	s := newStore()
	_ = s.load()
	_ = pick[int](1)
	return nil, h.users.refresh()
}

func (h *HTTP) GetOther(ctx context.Context, _ apiv1.GetOtherRequestObject) (any, error) {
	var x sender
	_ = x.Send()
	_ = h.hook()
	return nil, fail(CodeGone)
}

func (h *HTTP) GetLonely(context.Context, apiv1.GetLonelyRequestObject) (any, error) { return nil, nil }

func (h *HTTP) GetBlind(ctx context.Context, _ apiv1.GetBlindRequestObject) (any, error) {
	_ = run(h.passer.Pass)
	_ = h.svcs["k"].Get()
	_ = run(generic[int])
	_ = run(h.hook)
	_ = run(hookVar)
	_ = run(h.fns["k"])
	h.quits[0]()
	_ = viaParam[Passer](Passer{})
	_ = viaIndex[[]Passer](nil)
	_ = h.fns["k"]()
	f := h.passer.Pass
	_ = f()
	_ = run(f)
	return nil, nil
}

func run(f func() error) error { return f() }

func generic[T any]() error { return apperr.New(apperr.KindForbidden, CodeGeneric) }

func viaParam[T doer](v T) error { return v.Do() }

func viaIndex[S ~[]E, E doer](s S) error { return s[0].Do() }

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
		// quiet cannot construct an apperr.Error: the trace does not
		// enter it, whatever it calls.
		modulePath + "/internal/demo/quiet": `package quiet

type Box struct{}

func (Box) Open() error { return mystery() }
`,
		modulePath + "/internal/pending": "package pending\n\n" + imports + `
type Server struct{}

func (Server) GetThing(context.Context, apiv1.GetThingRequestObject) (any, error) { return nil, other.Check() }
`,
		apperrPath: `package apperr

type Kind uint8

const (
	KindInvalid Kind = iota
	KindNotFound
	KindConflict
	KindForbidden
)

type Code string

type Error struct{}

func New(kind Kind, code Code) *Error { return &Error{} }

func Wrap(cause error, kind Kind, code Code) *Error { return &Error{} }
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
// follows, what it keeps apart, what it does not enter, and what it
// refuses to pass over.
func TestErrorTraceOnADemoModule(t *testing.T) {
	t.Parallel()

	m := demoModule(t)
	if !m.pkgs[modulePath+"/internal/demo"].capable || m.pkgs[modulePath+"/internal/demo/quiet"].capable {
		t.Fatal("capable packages: want demo, not quiet")
	}
	handlers := m.handlers()
	for _, h := range []string{"GetThing", "GetOther", "GetLonely"} {
		if len(handlers[h]) != 1 {
			t.Fatalf("handlers %v; want one each of GetThing (the pending stub is not a handler), GetOther and GetLonely",
				slices.Sorted(maps.Keys(handlers)))
		}
	}

	sites, unres, visited := m.trace(handlers["GetThing"][0])
	if len(unres) != 0 {
		t.Errorf("GetThing: unresolved %+v", unres)
	}
	var got []string
	for _, s := range sites {
		got = append(got, s.kind+" "+s.code)
	}
	slices.Sort(got)
	want := []string{
		"KindConflict DEMO_REUSED", // the closure's two calls
		"KindConflict DEMO_TAKEN",
		"KindConflict DEMO_VAR",  // a package-level var, through a local's method
		"KindInvalid DEMO_OTHER", // another package, untyped constant, Wrap
		"KindNotFound DEMO_GONE", // through users.inner, two fields deep
		"KindNotFound DEMO_IMPL", // the hasher interface, to its implementation
	}
	if !slices.Equal(got, want) {
		// DEMO_STAFF_ONLY would mean the two Login methods were mixed by
		// name.
		t.Errorf("GetThing reaches %v, want %v", got, want)
	}
	if visited < 5 {
		t.Errorf("GetThing followed %d calls", visited)
	}

	_, unres, _ = m.trace(handlers["GetOther"][0])
	var keys, whys []string
	for _, u := range unres {
		keys, whys = append(keys, u.key), append(whys, u.why)
	}
	for _, w := range []string{"an interface of the module that no type implements", "a function-typed field", "not a constant"} {
		if !slices.ContainsFunc(whys, func(s string) bool { return strings.Contains(s, w) }) {
			t.Errorf("GetOther: unresolved %v, want one that says %q", whys, w)
		}
	}
	if !slices.Contains(keys, "internal/demo.sender") {
		t.Errorf("GetOther: keys %v, want the interface named for unresolvedCalls", keys)
	}

	if _, _, followed := m.trace(handlers["GetLonely"][0]); followed != 0 {
		t.Errorf("GetLonely followed %d calls, want 0: the handler alone, which the real test makes red", followed)
	}

	// GetBlind: each green path has a code of its own, reached by no other
	// path (visit enters a declaration once): DEMO_PASSED only through the
	// interface method value passed on, DEMO_GENERIC only through the
	// instantiated generic function passed on, DEMO_INDEXED only through
	// the indexed access of a known element type. An indexed callee of
	// functions returning nothing is let past; everything else it meets is
	// red, each for its own reason.
	sites, unres, _ = m.trace(handlers["GetBlind"][0])
	got = nil
	for _, s := range sites {
		got = append(got, s.kind+" "+s.code)
	}
	slices.Sort(got)
	if want := []string{"KindConflict DEMO_PASSED", "KindForbidden DEMO_GENERIC", "KindNotFound DEMO_INDEXED"}; !slices.Equal(got, want) {
		t.Errorf("GetBlind reaches %v, want %v", got, want)
	}
	whys = nil
	for _, u := range unres {
		whys = append(whys, u.why)
	}
	slices.Sort(whys)
	wantWhys := []string{
		"a call of a local function value",                        // f()
		"a function-typed field, passed on",                       // run(h.hook)
		"a local function value passed on",                        // run(f)
		"a package-level function value passed on",                // run(hookVar)
		"a receiver of unknown type",                              // s[0].Do(), an indexed access of unknown element type
		"a receiver whose type is not a named type of the module", // v.Do(), a parameter of a type parameter's type
		"an indexed callee",                                       // h.fns["k"]()
		"an indexed function value passed on",                     // run(h.fns["k"])
	}
	if !slices.Equal(whys, wantWhys) {
		t.Errorf("GetBlind: unresolved %q, want %q", whys, wantWhys)
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

// TestExemptionFindings: the exemption lists are checked, not trusted.
// Today's lists pass; an exemption for a code the operation does not list,
// for an operation without a handler, or with a short reason is red.
func TestExemptionFindings(t *testing.T) {
	t.Parallel()

	listed := map[string][]string{"startRegistration": {}, "verifyRegistration": {"REGISTRATION_TOKEN_INVALID"}}
	if f := exemptionFindings(listed, nonStrictOperations(), unresolvedCalls(), unseenCodes()); len(f) != 0 {
		t.Errorf("today's exemption lists: %v", f)
	}
	got := exemptionFindings(listed,
		map[string]string{"issueRealtimeTicket": "short"},
		map[string]string{"internal/demo.sender": "a reason that is long enough"},
		map[string]map[string]string{
			"verifyRegistration": {"CONSENT_REQUIRED": "a code the operation does not list at all"},
			"noSuchOperation":    {"REGISTRATION_TOKEN_INVALID": "an operation without a handler at all"},
		})
	want := []string{
		"nonStrictOperations issueRealtimeTicket: the reason needs",
		"unseenCodes exempts CONSENT_REQUIRED of verifyRegistration",
		"unseenCodes names noSuchOperation",
	}
	if len(got) != len(want) {
		t.Fatalf("findings %v, want %d", got, len(want))
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("finding %q lacks %q", got[i], w)
		}
	}
}
