// Ref: oga9-engineering/sections/10-organization-core.md (invitations)
// One-time codes for channel-bound actions — today: phone-targeted org
// invites, where the bearer link alone must never mint a membership.
// Codes are 6 digits, SHA-256-hashed at rest in Valkey (10-minute TTL,
// same pattern as the staged MFA secret), max 5 attempts, 60s resend
// cooldown. Delivery goes through a pluggable sender seam — the SMS
// gateway wiring lives in main (Rule 3: nothing hardcoded).
package otp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	valkey "github.com/valkey-io/valkey-go"
)

var ErrUnconfigured = errors.New("otp_delivery_unconfigured")
var ErrCooldown = errors.New("otp_resend_cooldown")

const (
	codeTTL     = 600 // seconds — same window as the staged MFA secret
	maxAttempts = 5
	cooldownS   = 60
)

// SendFunc delivers a plaintext code to a destination (E.164 phone today).
type SendFunc func(ctx context.Context, destination, code string) error

type Service struct {
	vk     valkey.Client
	sender SendFunc
}

func NewService(vk valkey.Client) *Service { return &Service{vk: vk} }

// SetSender wires the delivery channel (SMS gateway). Nil → Send returns
// ErrUnconfigured so callers can degrade honestly (§22-style honest 503).
func (s *Service) SetSender(fn SendFunc) { s.sender = fn }

func codeKey(scope, ref string) string  { return "oga9:otp:" + scope + ":" + ref }
func triesKey(scope, ref string) string { return "oga9:otptries:" + scope + ":" + ref }
func cdKey(scope, ref string) string    { return "oga9:otpcd:" + scope + ":" + ref }

// hash binds the code to scope+ref — a code minted for invite A can't be
// replayed against invite B.
func hash(scope, ref, code string) string {
	sum := sha256.Sum256([]byte(scope + "|" + ref + "|" + code))
	return hex.EncodeToString(sum[:])
}

// Send mints a fresh code for (scope, ref), stores its hash, and delivers
// the plaintext via the sender. A resend replaces the prior code — only
// the latest is ever valid. 60s cooldown between sends per ref.
func (s *Service) Send(ctx context.Context, scope, ref, destination string) error {
	if s.sender == nil {
		return ErrUnconfigured
	}
	if _, err := s.vk.Do(ctx, s.vk.B().Get().Key(cdKey(scope, ref)).Build()).AsBytes(); err == nil {
		return ErrCooldown
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return err
	}
	code := fmt.Sprintf("%06d", n.Int64())
	if err := s.vk.Do(ctx, s.vk.B().Set().Key(codeKey(scope, ref)).
		Value(hash(scope, ref, code)).ExSeconds(codeTTL).Build()).Error(); err != nil {
		return err
	}
	s.vk.Do(ctx, s.vk.B().Set().Key(cdKey(scope, ref)).Value("1").ExSeconds(cooldownS).Build())
	s.vk.Do(ctx, s.vk.B().Del().Key(triesKey(scope, ref)).Build()) // fresh code, fresh attempt budget
	return s.sender(ctx, destination, code)
}

// Verify consumes a code — bounded attempts (5), constant-time compare.
// After max attempts the stored code is dropped so a re-request (and a new
// code) is required — an attacker can't keep grinding one code's space.
func (s *Service) Verify(ctx context.Context, scope, ref, code string) bool {
	if code == "" {
		return false
	}
	want, err := s.vk.Do(ctx, s.vk.B().Get().Key(codeKey(scope, ref)).Build()).AsBytes()
	if err != nil {
		return false
	}
	tries, _ := s.vk.Do(ctx, s.vk.B().Incr().Key(triesKey(scope, ref)).Build()).AsInt64()
	if tries == 1 {
		s.vk.Do(ctx, s.vk.B().Expire().Key(triesKey(scope, ref)).Seconds(codeTTL).Build())
	}
	if tries > maxAttempts {
		s.vk.Do(ctx, s.vk.B().Del().Key(codeKey(scope, ref)).Build())
		s.vk.Do(ctx, s.vk.B().Del().Key(triesKey(scope, ref)).Build())
		return false
	}
	if subtle.ConstantTimeCompare([]byte(hash(scope, ref, code)), want) != 1 {
		return false
	}
	s.vk.Do(ctx, s.vk.B().Del().Key(codeKey(scope, ref)).Build())
	s.vk.Do(ctx, s.vk.B().Del().Key(triesKey(scope, ref)).Build())
	return true
}
