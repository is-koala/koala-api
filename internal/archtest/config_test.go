package archtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/config"
)

// TestEveryVariableIsDocumented binds config.known to the documentation.
//
// The failure it prevents has already happened once: the object store
// variables were added to known and to ServeStorageErrors, which makes
// four of them required in staging and production, and neither README
// nor the runbook was updated. The next staging deploy would have failed
// at startup with a correct error message and no document to look it up
// in -- and README claims to hold "the full list", so the claim had
// quietly become false (ADR-0030).
//
// A variable has to appear in README's table and in the deployment
// runbook. Appearing anywhere in the file counts: this test checks that
// the name is written down, not how well.
func TestEveryVariableIsDocumented(t *testing.T) {
	t.Parallel()

	readme, err := os.ReadFile(filepath.Join(moduleRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	runbook, err := os.ReadFile(filepath.Join(moduleRoot, "docs", "runbook", "yayin.md"))
	if err != nil {
		t.Fatalf("read the deployment runbook: %v", err)
	}

	vars := config.Variables()
	if len(vars) == 0 {
		t.Fatal("config.Variables() is empty; the test would pass vacuously")
	}
	for _, name := range vars {
		if !strings.Contains(string(readme), name) {
			t.Errorf("%s is read by the service but is not in README's Yapılandırma table", name)
		}
		if !strings.Contains(string(runbook), name) {
			t.Errorf("%s is read by the service but is not in docs/runbook/yayin.md", name)
		}
	}
}

// TestDocumentedVariablesExist is the other direction: a variable the
// documentation still names and the code no longer reads sends a
// deployment after a value that stops startup as an unknown KOALA_*.
func TestDocumentedVariablesExist(t *testing.T) {
	t.Parallel()

	known := map[string]bool{}
	for _, name := range config.Variables() {
		known[name] = true
	}
	for label, docPath := range map[string]string{
		"README.md":             filepath.Join(moduleRoot, "README.md"),
		"docs/runbook/yayin.md": filepath.Join(moduleRoot, "docs", "runbook", "yayin.md"),
	} {
		body, err := os.ReadFile(docPath)
		if err != nil {
			t.Fatalf("read %s: %v", label, err)
		}
		for _, name := range namesIn(string(body)) {
			if !known[name] {
				t.Errorf("%s names %s, which the service does not read; an unknown KOALA_* stops startup", label, name)
			}
		}
	}
}

// namesIn returns every KOALA_* identifier in the text, deduplicated.
func namesIn(body string) []string {
	const prefix = "KOALA_"
	seen := map[string]bool{}
	var out []string
	for i := 0; i+len(prefix) <= len(body); i++ {
		if body[i:i+len(prefix)] != prefix {
			continue
		}
		// Only a name that starts a word, so KOALA_ inside a longer
		// token is not read as one.
		if i > 0 && isNameByte(body[i-1]) {
			continue
		}
		end := i + len(prefix)
		for end < len(body) && isNameByte(body[end]) {
			end++
		}
		name := body[i:end]
		// "KOALA_OBJECT_*" and "KOALA_*" are patterns in prose, not
		// variables.
		if strings.HasSuffix(name, "_") || name == prefix {
			continue
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func isNameByte(b byte) bool {
	return b == '_' || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
