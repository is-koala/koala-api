package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// modulePath is the module's import path; with moduleRoot it turns a
// directory of the walk into the path another package imports it by.
const modulePath = "github.com/is-koala/koala-api"

// apperrPath is the import path of the error model (ADR-0007).
const apperrPath = modulePath + "/internal/platform/apperr"

// contractTest is the test that checks the code lists against the
// contract's ErrorCode list (api/components/problem.yaml). It reads the
// lists by calling them, so a list no call reaches is outside the
// contract check however complete it is.
const contractTest = "internal/app/api_test.go"

// codeListExemptions are the packages that declare an error code without
// a list function, with the reason. An entry is checked: a package that
// no longer declares a code is a stale exemption and fails.
func codeListExemptions() map[string]string {
	return map[string]string{
		// apperr.CodeInternal is the code of the error model itself: the
		// code of every error that is not an *apperr.Error. It is
		// httpx.WriteError that renders it, and httpx.PlatformCodes()
		// lists it; apperr returns nothing to a client of its own.
		"internal/platform/apperr": "CodeInternal is listed by httpx.PlatformCodes(), the package that renders it",
	}
}

// importName returns the name f uses for the package at path: its alias,
// the last element of the path, or "." for a dot import; "" when f does
// not import it.
func importName(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return path[strings.LastIndexByte(path, '/')+1:]
	}
	return ""
}

// isUpperSnake reports whether s is an UPPER_SNAKE identifier, the shape
// CLAUDE.md requires of a stable error code (STAFF_PERMISSION_DENIED).
func isUpperSnake(s string) bool {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if c == '_' {
			if i == len(s)-1 || s[i+1] == '_' {
				return false
			}
			continue
		}
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// isCodeType reports whether the type expression names apperr.Code:
// apperr.Code under the name the file imports it by, or Code inside the
// apperr package itself.
func isCodeType(expr ast.Expr, apperr, pkg string) bool {
	switch x := expr.(type) {
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && apperr != "" && apperr != "." && id.Name == apperr && x.Sel.Name == "Code"
	case *ast.Ident:
		return x.Name == "Code" && (pkg == "apperr" || apperr == ".")
	}
	return false
}

// codeConst is one error-code constant: its name and where it is.
type codeConst struct {
	name string
	pos  string
}

// codeConstants returns the error-code constants f declares. A constant
// counts when its declared type is apperr.Code, or when it is named
// Code<Something> and its value is an UPPER_SNAKE string literal: the
// codes of internal/platform/httpx are untyped string constants, turned
// into apperr.Code only where PlatformCodes builds the slice, so a rule
// that read the declared type alone would miss that package whole. The
// two signals together leave out the keys, actions and role codes that
// are string constants of other kinds (ActionCreate, ownerRoleCode).
func codeConstants(fset *token.FileSet, f *ast.File) []codeConst {
	apperr := importName(f, apperrPath)
	var out []codeConst
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			typed := isCodeType(vs.Type, apperr, f.Name.Name)
			for i, id := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				if !typed && !(strings.HasPrefix(id.Name, "Code") && isUpperSnake(value)) {
					continue
				}
				out = append(out, codeConst{name: id.Name, pos: fset.Position(id.Pos()).String()})
			}
		}
	}
	return out
}

// codeListFunc returns the package-level function of f that returns
// []apperr.Code and whose name ends in "Codes" (Codes, PlatformCodes):
// the list the contract test reads.
func codeListFunc(f *ast.File) *ast.FuncDecl {
	apperr := importName(f, apperrPath)
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Body == nil || !strings.HasSuffix(fd.Name.Name, "Codes") {
			continue
		}
		if fd.Type.Results == nil || len(fd.Type.Results.List) != 1 {
			continue
		}
		arr, ok := fd.Type.Results.List[0].Type.(*ast.ArrayType)
		if !ok || arr.Len != nil {
			continue
		}
		if isCodeType(arr.Elt, apperr, f.Name.Name) {
			return fd
		}
	}
	return nil
}

// identsIn returns every identifier of n, so a constant named anywhere in
// a list function counts as listed - in the slice literal, through a
// helper call or appended in a loop.
func identsIn(n ast.Node) []string {
	var out []string
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			out = append(out, id.Name)
		}
		return true
	})
	return out
}

// codePackage is what the walk learns about one package.
type codePackage struct {
	consts  []codeConst
	listed  map[string]bool
	list    string // the list function's name, "" when there is none
	listPos string
}

// codePackages groups the error-code constants and the list function of
// every package of the module, by import path.
func codePackages(t *testing.T) map[string]*codePackage {
	t.Helper()
	out := map[string]*codePackage{}
	goFiles(t, func(fset *token.FileSet, path string, f *ast.File) {
		rel, err := filepath.Rel(moduleRoot, filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		importPath := modulePath + "/" + filepath.ToSlash(rel)
		p := out[importPath]
		if p == nil {
			p = &codePackage{listed: map[string]bool{}}
			out[importPath] = p
		}
		p.consts = append(p.consts, codeConstants(fset, f)...)
		if fd := codeListFunc(f); fd != nil {
			p.list, p.listPos = fd.Name.Name, fset.Position(fd.Pos()).String()
			for _, name := range identsIn(fd.Body) {
				p.listed[name] = true
			}
		}
	})
	return out
}

// TestEveryErrorCodeIsInItsPackageList: an apperr.Code constant that no
// code list names is outside the contract (ADR-0007 madde 1, ADR-0028).
// TestPlatformCodesAreInTheContract checks that everything in the lists
// is in api/components/problem.yaml ErrorCode; it cannot see a code that
// was never put in a list, and such a code reaches a client as a code no
// generated client knows.
func TestEveryErrorCodeIsInItsPackageList(t *testing.T) {
	t.Parallel()
	pkgs := codePackages(t)
	seen := 0
	for _, path := range slices.Sorted(maps.Keys(pkgs)) {
		p := pkgs[path]
		if len(p.consts) == 0 {
			continue
		}
		rel := strings.TrimPrefix(path, modulePath+"/")
		if reason, exempt := codeListExemptions()[rel]; exempt {
			t.Logf("%s: no list function, exempt: %s", rel, reason)
			continue
		}
		seen += len(p.consts)
		if p.list == "" {
			t.Errorf("%s declares %d error code(s) and no function returning []apperr.Code; add Codes() and read it in %s",
				rel, len(p.consts), contractTest)
			continue
		}
		for _, c := range p.consts {
			if !p.listed[c.name] {
				t.Errorf("%s: %s is not in %s.%s (%s), so it never reaches the contract check",
					c.pos, c.name, rel, p.list, p.listPos)
			}
		}
	}
	// The repository has five lists today (httpx, softdelete, realtime,
	// authn, staff); a walk that finds almost nothing is a broken walk,
	// not a clean repository.
	if seen < 40 {
		t.Errorf("the walk found %d error code constants; it no longer reaches the packages that declare them", seen)
	}
	for rel := range codeListExemptions() {
		p := pkgs[modulePath+"/"+rel]
		if p == nil || len(p.consts) == 0 {
			t.Errorf("%s is exempt from the code list rule but declares no error code; drop the exemption", rel)
		}
	}
}

// TestCodeListsReachTheContractTest closes the other half: a complete
// list helps nothing while no one reads it. Every package that exposes a
// code list must be called by the contract test, which is the only place
// the codes meet api/components/problem.yaml.
func TestCodeListsReachTheContractTest(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(moduleRoot, contractTest), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	read := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !strings.HasSuffix(sel.Sel.Name, "Codes") {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok {
			for _, imp := range f.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				name := path[strings.LastIndexByte(path, '/')+1:]
				if imp.Name != nil {
					name = imp.Name.Name
				}
				if name == pkg.Name {
					read[path] = true
				}
			}
		}
		return true
	})
	if len(read) == 0 {
		t.Fatalf("%s calls no code list; the walk or the test moved", contractTest)
	}
	pkgs := codePackages(t)
	lists := 0
	for _, path := range slices.Sorted(maps.Keys(pkgs)) {
		p := pkgs[path]
		if p.list == "" {
			continue
		}
		lists++
		if !read[path] {
			t.Errorf("%s.%s is not read by %s, so its codes never meet the contract", path, p.list, contractTest)
		}
	}
	if lists < 5 {
		t.Errorf("the walk found %d code lists; it no longer reaches the packages that expose them", lists)
	}
}

// TestErrorCodeDetector is the detector's own red and green: each case
// says how many code constants the file holds and whether it exposes a
// list, so the rule above cannot pass by finding nothing.
func TestErrorCodeDetector(t *testing.T) {
	t.Parallel()
	const imports = "import \"github.com/is-koala/koala-api/internal/platform/apperr\"\n\n"
	tests := []struct {
		name       string
		src        string
		wantConsts int
		wantList   string
		wantListed []string // constants the list function names
	}{
		{
			name: "typed constants and a complete list",
			src: "package realtime\n\n" + imports + `
const (
	CodeTicketInvalid apperr.Code = "REALTIME_TICKET_INVALID"
	CodeDraining      apperr.Code = "REALTIME_DRAINING"
)

func Codes() []apperr.Code { return []apperr.Code{CodeTicketInvalid, CodeDraining} }
`,
			wantConsts: 2, wantList: "Codes",
			wantListed: []string{"CodeTicketInvalid", "CodeDraining"},
		},
		{
			name: "untyped string constants, the httpx shape",
			src: "package httpx\n\n" + imports + `
const CodeRateLimited = "RATE_LIMITED"

const (
	CodeRouteNotFound    = "ROUTE_NOT_FOUND"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
)

func PlatformCodes() []apperr.Code {
	return []apperr.Code{apperr.CodeInternal, CodeRouteNotFound, CodeRateLimited}
}
`,
			wantConsts: 3, wantList: "PlatformCodes",
			wantListed: []string{"CodeRouteNotFound", "CodeRateLimited"},
		},
		{
			name: "a typed constant whose name does not start with Code",
			src: "package staff\n\n" + imports + `
const permissionDenied apperr.Code = "STAFF_PERMISSION_DENIED"
`,
			wantConsts: 1,
		},
		{
			name: "string constants of other kinds",
			src: `package staff

const (
	ActionCreate      = "staff.member.create"
	ownerRoleCode     = "OWNER"
	ExtPermission     = "x-authz-permission"
	CodeLikeButLower  = "not_a_code"
	maxRolesPerCreate = 10
)
`,
			wantConsts: 0,
		},
		{
			name: "a list of the wrong shape is no list",
			src: "package authn\n\n" + imports + `
const CodeTokenMissing apperr.Code = "AUTH_TOKEN_MISSING"

func Codes() []string { return []string{"AUTH_TOKEN_MISSING"} }

func (s *Service) ServiceCodes() []apperr.Code { return nil }
`,
			wantConsts: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "domain.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			got := codeConstants(fset, f)
			if len(got) != tt.wantConsts {
				t.Errorf("detected %d code constants %v, want %d", len(got), names(got), tt.wantConsts)
			}
			fd := codeListFunc(f)
			switch {
			case tt.wantList == "" && fd != nil:
				t.Errorf("detected the list function %s, want none", fd.Name.Name)
			case tt.wantList != "" && fd == nil:
				t.Fatalf("detected no list function, want %s", tt.wantList)
			case fd == nil:
				return
			case fd.Name.Name != tt.wantList:
				t.Fatalf("detected the list function %s, want %s", fd.Name.Name, tt.wantList)
			}
			listed := identsIn(fd.Body)
			for _, want := range tt.wantListed {
				if !slices.Contains(listed, want) {
					t.Errorf("%s does not name %s; identifiers %v", fd.Name.Name, want, listed)
				}
			}
		})
	}
}

// names returns the constant names of a detector result, for a message.
func names(cs []codeConst) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.name)
	}
	return out
}

// TestUpperSnakeShape pins the value side of the detector: the shape
// CLAUDE.md requires of an error code.
func TestUpperSnakeShape(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"INTERNAL", "STAFF_PERMISSION_DENIED", "AUTH_TOKEN_MISSING", "RATE_LIMITED", "A1_B2"} {
		if !isUpperSnake(s) {
			t.Errorf("isUpperSnake(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "internal", "Staff_Denied", "staff.member.create", "x-authz-permission", "_LEADING", "TRAILING_", "DOUBLE__BAR", "1START"} {
		if isUpperSnake(s) {
			t.Errorf("isUpperSnake(%q) = true, want false", s)
		}
	}
}
