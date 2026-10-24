// Signed single-use tokens for email verify + password reset (§02).
// Single-use enforced via Valkey marker oga9:auth:used_token:{jti}.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"strconv"
	"strings"
	"time"
)

func SignActionToken(secret, userID, purpose string, ttl time.Duration) (string, string) {
	jti := newJTI()
	exp := time.Now().Add(ttl).Unix()
	payload := fmt.Sprintf("%s|%s|%s|%d", jti, userID, purpose, exp)
	sig := hmac.New(sha256.New, []byte(secret))
	sig.Write([]byte(payload))
	return fmt.Sprintf("%s.%s", payload, hex.EncodeToString(sig.Sum(nil))[:32]), jti
}

func VerifyActionToken(secret, token, purpose string) (jti, userID string, err error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", "", errors.New("malformed token")
	}
	sig := hmac.New(sha256.New, []byte(secret))
	sig.Write([]byte(parts[0]))
	if hex.EncodeToString(sig.Sum(nil))[:32] != parts[1] {
		return "", "", errors.New("bad signature")
	}
	f := strings.Split(parts[0], "|")
	if len(f) != 4 || f[2] != purpose {
		return "", "", errors.New("wrong purpose")
	}
	exp, _ := strconv.ParseInt(f[3], 10, 64)
	if time.Now().Unix() > exp {
		return "", "", errors.New("expired")
	}
	return f[0], f[1], nil
}

func newJTI() string { return uuid.NewString() }
