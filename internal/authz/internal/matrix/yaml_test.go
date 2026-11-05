package matrix

import (
	"reflect"
	"testing"
)

func TestParseYAML(t *testing.T) {
	t.Parallel()
	src := `# header comment
version: 0
engines:          # trailing comment
  - [go, openfga]
  - go_only
basis:
  worker: "atamanın işçisi (job_assignment#worker)"
fixtures:
  subjects:
    user:u_worker: {type: user, note: "u_worker'ın notu; a#b"}
  objects:
    job:x: {type: job, attrs: {"status": "OPEN", "deleted": true, "p": null, "l": ["a", "b"], "e": {}}}
  tuples:
    - "job:x#viewer@user:*"
cases:
  - id: AZM-001
    engine: [go, openfga]
    conceal: false
    source: []
  - id: AZM-002
    note: "with \"escaped\" quotes"
`
	got, err := parseYAML(src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"version": "0",
		"engines": []any{[]any{"go", "openfga"}, "go_only"},
		"basis":   map[string]any{"worker": "atamanın işçisi (job_assignment#worker)"},
		"fixtures": map[string]any{
			"subjects": map[string]any{"user:u_worker": map[string]any{"type": "user", "note": "u_worker'ın notu; a#b"}},
			"objects": map[string]any{"job:x": map[string]any{"type": "job", "attrs": map[string]any{
				"status": "OPEN", "deleted": true, "p": nil, "l": []any{"a", "b"}, "e": map[string]any{},
			}}},
			"tuples": []any{"job:x#viewer@user:*"},
		},
		"cases": []any{
			map[string]any{"id": "AZM-001", "engine": []any{"go", "openfga"}, "conceal": false, "source": []any{}},
			map[string]any{"id": "AZM-002", "note": `with "escaped" quotes`},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestParseYAMLRejects(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"tab":                "a:\n\t- b\n",
		"single quote":       "a: 'b'\n",
		"unterminated quote": "a: \"b\n",
		"duplicate key":      "a: 1\na: 2\n",
		"bad indentation":    "a:\n    b: 1\n  c: 2\n",
		"block scalar":       "a: |\n  text\n",
		"unterminated flow":  "a: [b, c\n",
		"trailing garbage":   "a: \"b\" c\n",
		"not a key":          "a: 1\njust text\n",
		"empty":              "# only a comment\n",
		"indented start":     "  a: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if v, err := parseYAML(src); err == nil {
				t.Fatalf("accepted: %#v", v)
			}
		})
	}
}
