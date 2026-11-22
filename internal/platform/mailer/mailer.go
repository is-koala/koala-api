// Package mailer is the mail plumbing of the service (plan §3.1): the
// message a sender delivers and the templates a module renders its
// messages from. It knows no mail of any module: each module embeds its
// own template files and decides when a mail is sent. Delivery is a
// River job (ADR-0010, ADR-0032), never a call inside a transaction.
//
// A template set holds, for a message named n, the templates n.subject
// and n.text in the module's *.txt files (text/template) and n.html in its
// *.html files (html/template, which escapes what it inserts). A missing
// key in the data is an error, not an empty string.
package mailer

import (
	"bytes"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io/fs"
	"strings"
	texttemplate "text/template"
)

// ErrUndeliverable marks a send that cannot succeed on a retry: the
// recipient is not an address, or the relay refused it for good. A sender
// wraps it; a job that sees it stops retrying.
var ErrUndeliverable = errors.New("mailer: undeliverable")

// Message is one mail: a recipient, a subject and the body as plain text
// with an HTML alternative.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Templates renders the messages of one module.
type Templates struct {
	text *texttemplate.Template
	html *htmltemplate.Template
}

// ParseTemplates reads every *.txt and *.html file at the root of fsys.
func ParseTemplates(fsys fs.FS) (*Templates, error) {
	text, err := texttemplate.New("").Option("missingkey=error").ParseFS(fsys, "*.txt")
	if err != nil {
		return nil, fmt.Errorf("mailer: parse text templates: %w", err)
	}
	html, err := htmltemplate.New("").Option("missingkey=error").ParseFS(fsys, "*.html")
	if err != nil {
		return nil, fmt.Errorf("mailer: parse HTML templates: %w", err)
	}
	return &Templates{text: text, html: html}, nil
}

// Check reports a message whose subject, text or HTML template is missing,
// so a module can refuse to start instead of failing at its first mail.
func (t *Templates) Check(names ...string) error {
	var errs []error
	for _, n := range names {
		if t.text.Lookup(n+".subject") == nil || t.text.Lookup(n+".text") == nil || t.html.Lookup(n+".html") == nil {
			errs = append(errs, fmt.Errorf("mailer: message %q needs the templates %[1]s.subject, %[1]s.text and %[1]s.html", n))
		}
	}
	return errors.Join(errs...)
}

// Render renders the message name for the recipient to with data.
func (t *Templates) Render(name, to string, data any) (Message, error) {
	var subject, text, html bytes.Buffer
	if err := t.text.ExecuteTemplate(&subject, name+".subject", data); err != nil {
		return Message{}, fmt.Errorf("mailer: render %s subject: %w", name, err)
	}
	if err := t.text.ExecuteTemplate(&text, name+".text", data); err != nil {
		return Message{}, fmt.Errorf("mailer: render %s text: %w", name, err)
	}
	if err := t.html.ExecuteTemplate(&html, name+".html", data); err != nil {
		return Message{}, fmt.Errorf("mailer: render %s HTML: %w", name, err)
	}
	// A subject is one header line.
	s := strings.Join(strings.Fields(subject.String()), " ")
	return Message{To: to, Subject: s, Text: strings.TrimSpace(text.String()) + "\n", HTML: html.String()}, nil
}
