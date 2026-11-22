// Package sms is the text message plumbing of the service (plan §3.1):
// the message a sender delivers and the error that tells a job to stop
// retrying. It knows no message of any module; delivery is a River job
// (ADR-0010, ADR-0033), never a call inside a transaction. The provider is
// internal/integrations/iletimerkezi.
//
// A message's recipient is personal data: a sender keeps it out of its
// errors and logs.
package sms

import "errors"

// ErrUndeliverable marks a send that cannot succeed on a retry: the
// provider refused the recipient or the message for good. A sender wraps
// it; a job that sees it stops retrying.
var ErrUndeliverable = errors.New("sms: undeliverable")

// Message is one text message: the recipient in E.164 and the text.
type Message struct {
	To   string
	Text string
}
