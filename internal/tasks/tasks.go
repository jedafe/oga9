// Ref: oga9-engineering/sections/08-job-queue-asynq.md, conventions/asynq-tasks-catalog.md
// Registered task types + payloads + handlers.
package tasks

import (
	"context"
	"encoding/json"
	"log"

	"github.com/hibiken/asynq"
	"github.com/jedafe/oga9/internal/mailer"
)

const (
	TypeSendVerification  = "identity:send_verification"
	TypeSendPasswordReset = "identity:send_password_reset"
)

type SendEmailPayload struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func NewEmailTask(taskType string, p SendEmailPayload) (*asynq.Task, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(taskType, b, asynq.Queue("default"), asynq.MaxRetry(5)), nil
}

// Handlers returns the mux registrations for the identity mail tasks.
func Handlers(m *mailer.Mailer) map[string]asynq.HandlerFunc {
	send := func(ctx context.Context, t *asynq.Task) error {
		var p SendEmailPayload
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		if err := m.Send(p.To, p.Subject, p.Body); err != nil {
			log.Printf("mail task %s failed: %v", t.Type(), err)
			return err // asynq retries
		}
		return nil
	}
	return map[string]asynq.HandlerFunc{
		TypeSendVerification:  send,
		TypeSendPasswordReset: send,
	}
}
