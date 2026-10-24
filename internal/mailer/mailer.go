// Ref: oga9-engineering/sections/01-foundation-tech-stack.md
// Dev: Mailpit at OGA9_SMTP_HOST:PORT. Prod: real SMTP via same config.
package mailer

import (
	"fmt"
	"net/smtp"
)

type Mailer struct {
	host string
	port int
	from string
}

func New(host string, port int, from string) *Mailer {
	if from == "" {
		from = "noreply@localhost"
	}
	return &Mailer{host: host, port: port, from: from}
}

func (m *Mailer) Send(to, subject, body string) error {
	msg := []byte(fmt.Sprintf("To: %s\r\nSubject: %s\r\n\r\n%s\r\n", to, subject, body))
	return smtp.SendMail(fmt.Sprintf("%s:%d", m.host, m.port), nil, m.from, []string{to}, msg)
}
