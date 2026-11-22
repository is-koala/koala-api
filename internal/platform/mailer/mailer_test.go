package mailer_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/is-koala/koala-api/internal/platform/mailer"
)

func templates(t *testing.T) *mailer.Templates {
	t.Helper()
	fsys := fstest.MapFS{
		"welcome.txt": {Data: []byte(`{{define "welcome.subject"}}Hello
  {{.Name}}{{end}}
{{define "welcome.text"}}
Open {{.Link}}
{{end}}`)},
		"welcome.html": {Data: []byte(`{{define "welcome.html"}}<p>Hi {{.Name}}</p><a href="{{.Link}}">open</a>{{end}}`)},
	}
	tpl, err := mailer.ParseTemplates(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return tpl
}

func TestRender(t *testing.T) {
	t.Parallel()

	tpl := templates(t)
	data := struct{ Name, Link string }{"<b>Ayşe</b>", "https://iskoala.com/x?token=a&b=c"}
	m, err := tpl.Render("welcome", "ayse@example.com", data)
	if err != nil {
		t.Fatal(err)
	}
	if m.To != "ayse@example.com" {
		t.Errorf("To = %q", m.To)
	}
	// The subject is one header line however the template wraps it.
	if m.Subject != "Hello <b>Ayşe</b>" {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.Text != "Open https://iskoala.com/x?token=a&b=c\n" {
		t.Errorf("Text = %q", m.Text)
	}
	// html/template escapes what it inserts: no markup from the data.
	if strings.Contains(m.HTML, "<b>") || !strings.Contains(m.HTML, "&lt;b&gt;Ayşe&lt;/b&gt;") {
		t.Errorf("HTML = %q, want the name escaped", m.HTML)
	}
	if !strings.Contains(m.HTML, `href="https://iskoala.com/x?token=a&amp;b=c"`) {
		t.Errorf("HTML = %q, want the link in href", m.HTML)
	}
}

func TestRenderFailsClosed(t *testing.T) {
	t.Parallel()

	tpl := templates(t)
	if _, err := tpl.Render("welcome", "a@example.com", map[string]string{"Name": "x"}); err == nil {
		t.Error("Render with a missing key succeeded; want an error, not an empty link")
	}
	if _, err := tpl.Render("unknown", "a@example.com", nil); err == nil {
		t.Error("Render of an unknown message succeeded")
	}
	if err := tpl.Check("welcome"); err != nil {
		t.Errorf("Check(welcome) = %v", err)
	}
	if err := tpl.Check("welcome", "goodbye"); err == nil || !strings.Contains(err.Error(), "goodbye") {
		t.Errorf("Check(goodbye) = %v, want an error naming it", err)
	}
}
