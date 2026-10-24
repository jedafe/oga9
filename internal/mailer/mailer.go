// Ref: oga9-engineering/sections/01-foundation-tech-stack.md
// Dev: Mailpit at OGA9_SMTP_HOST:PORT (no auth). Prod: real SMTP — set
// OGA9_SMTP_USERNAME/PASSWORD for authenticated delivery.
package mailer

import (
	"fmt"
	"net/smtp"
)

type Mailer struct {
	host string
	port int
	from string
	user string
	pass string
}

func New(host string, port int, from, user, pass string) *Mailer {
	if from == "" {
		from = "noreply@localhost"
	}
	return &Mailer{host: host, port: port, from: from, user: user, pass: pass}
}

func (m *Mailer) Send(to, subject, body string) error {
	var auth smtp.Auth
	if m.user != "" {
		auth = smtp.PlainAuth("", m.user, m.pass, m.host)
	}
	msg := []byte(fmt.Sprintf("To: %s\r\nSubject: %s\r\n\r\n%s\r\n", to, subject, body))
	return smtp.SendMail(fmt.Sprintf("%s:%d", m.host, m.port), auth, m.from, []string{to}, msg)
}
