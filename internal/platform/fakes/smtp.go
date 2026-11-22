package fakes

import (
	"bufio"
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// SMTPMessage is a message the SMTP fake accepted.
type SMTPMessage struct {
	From string
	To   []string
	// Data is the message as sent after DATA, dot-unstuffed, without the
	// terminating line: headers, a blank line and the encoded body.
	Data string
}

// SMTP is a fake SMTP relay (RFC 5321) on a loopback port, for the mail
// client of internal/integrations/smtp (ADR-0032). It speaks the plain
// part of the protocol only: EHLO advertises neither STARTTLS nor AUTH,
// so a client that requires TLS fails against it, which is how a test
// proves that a mandatory TLS policy is not skipped silently. Every
// accepted message is recorded; an unknown command gets 502.
type SMTP struct {
	tb       testing.TB
	ln       net.Listener
	wg       sync.WaitGroup
	mu       sync.Mutex
	msgs     []SMTPMessage
	rejected map[string]bool
}

// NewSMTP starts the fake; it stops at the end of the test.
func NewSMTP(tb testing.TB) *SMTP {
	tb.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	s := &SMTP{tb: tb, ln: ln}
	s.wg.Go(s.accept)
	tb.Cleanup(func() {
		_ = s.ln.Close()
		s.wg.Wait()
	})
	return s
}

// Host and Port are the address to give the client.
func (s *SMTP) Host() string { return "127.0.0.1" }

// Port is the fake's TCP port.
func (s *SMTP) Port() int {
	addr, ok := s.ln.Addr().(*net.TCPAddr)
	if !ok {
		s.tb.Fatalf("fakes: SMTP listener address %v is not TCP", s.ln.Addr())
	}
	return addr.Port
}

// RejectRecipient makes the relay refuse addr for good: RCPT TO gets 550.
func (s *SMTP) RejectRecipient(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejected == nil {
		s.rejected = map[string]bool{}
	}
	s.rejected[addr] = true
}

func (s *SMTP) refuses(addr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejected[addr]
}

// Messages returns the accepted messages in arrival order.
func (s *SMTP) Messages() []SMTPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.msgs)
}

func (s *SMTP) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // closed at cleanup
		}
		s.wg.Go(func() { s.serve(conn) })
	}
}

// smtpTimeout bounds one session, so a stuck client cannot hold the
// test's cleanup.
const smtpTimeout = 10 * time.Second

func (s *SMTP) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(smtpTimeout))
	r := bufio.NewReader(conn)
	reply := func(lines ...string) bool {
		_, err := conn.Write([]byte(strings.Join(lines, "\r\n") + "\r\n"))
		return err == nil
	}
	if !reply("220 fake.smtp.test ESMTP") {
		return
	}
	var cur SMTPMessage
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		var ok bool
		switch strings.ToUpper(verb) {
		case "EHLO":
			ok = reply("250-fake.smtp.test", "250 8BITMIME")
		case "HELO", "NOOP":
			ok = reply("250 OK")
		case "RSET":
			cur = SMTPMessage{}
			ok = reply("250 OK")
		case "MAIL":
			cur = SMTPMessage{From: address(arg)}
			ok = reply("250 OK")
		case "RCPT":
			if to := address(arg); s.refuses(to) {
				ok = reply("550 5.1.1 Mailbox unavailable")
			} else {
				cur.To = append(cur.To, to)
				ok = reply("250 OK")
			}
		case "DATA":
			if !reply("354 End data with <CR><LF>.<CR><LF>") {
				return
			}
			data, err := readData(r)
			if err != nil {
				return
			}
			cur.Data = data
			s.mu.Lock()
			s.msgs = append(s.msgs, cur)
			s.mu.Unlock()
			cur = SMTPMessage{}
			ok = reply("250 OK queued as " + strconv.Itoa(len(s.Messages())))
		case "QUIT":
			_ = reply("221 Bye")
			return
		default:
			ok = reply("502 Command not implemented")
		}
		if !ok {
			return
		}
	}
}

// address takes the address out of "FROM:<a@b> SIZE=1" or "TO:<a@b>".
func address(arg string) string {
	_, rest, _ := strings.Cut(arg, ":")
	rest = strings.TrimSpace(rest)
	if i := strings.IndexByte(rest, '>'); strings.HasPrefix(rest, "<") && i > 0 {
		return rest[1:i]
	}
	return rest
}

var errDataUnterminated = errors.New("fakes: DATA ended without its terminating line")

// readData reads a DATA section up to the lone "." line and undoes the
// dot-stuffing (RFC 5321 §4.5.2).
func readData(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", errors.Join(errDataUnterminated, err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			return b.String(), nil
		}
		b.WriteString(strings.TrimPrefix(trimmed, "."))
		b.WriteString("\n")
	}
}
