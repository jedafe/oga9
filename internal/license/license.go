// Ref: oga9-engineering/sections/09-deployment-editions.md + §07 —
// self-hosted license validation. The signed file verifies OFFLINE
// (ed25519 public key, Rule 29: never always-online); when the control
// plane is unreachable the deployment runs inside license.offline_grace_days
// with a warning, per the §09 degradation matrix.
package license

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Status is the deployment-side license posture.
type Status string

const (
	StatusCommunity Status = "community" // no license file — community features only
	StatusValid     Status = "valid"
	StatusGrace     Status = "grace"   // past expiry, inside offline grace window
	StatusExpired   Status = "expired" // past expiry AND past grace — degraded
	StatusInvalid   Status = "invalid" // signature/parse failure — treated as expired
)

var ErrNoLicense = errors.New("no license file configured")

// File is the signed license document the control plane issues.
type File struct {
	DeploymentID string          `json:"deployment_id"`
	Edition      string          `json:"edition"`
	Entitlements json.RawMessage `json:"entitlements"`
	ExpiresAt    time.Time       `json:"expires_at"`
	Signature    string          `json:"signature"` // hex ed25519
}

// Validator checks the signed file locally. pubHex is the hex ed25519
// public key (OGA9_LICENSE_PUBLIC_KEY); a self-hosted install never holds
// the signing private key.
type Validator struct {
	pub       ed25519.PublicKey
	path      string
	graceDays int
}

func New(pubHex, path string, graceDays int) (*Validator, error) {
	v := &Validator{path: path, graceDays: graceDays}
	if pubHex == "" {
		return v, nil // community install — Check reports community, never errors
	}
	b, err := hex.DecodeString(pubHex)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("OGA9_LICENSE_PUBLIC_KEY: bad ed25519 public key")
	}
	v.pub = ed25519.PublicKey(b)
	return v, nil
}

// payload mirrors billing.Service.licensePayload byte-for-byte — the
// signed material is {deployment_id, entitlements, expires_at}; edition
// rides in the file but is not part of the signed material.
func payload(f *File) []byte {
	ents := json.RawMessage(`{}`)
	if len(f.Entitlements) > 0 && string(f.Entitlements) != "null" {
		ents = canonicalJSON(f.Entitlements)
	}
	b, _ := json.Marshal(map[string]any{
		"deployment_id": f.DeploymentID,
		"entitlements":  ents,
		"expires_at":    f.ExpiresAt.UTC().Format(time.RFC3339),
	})
	return b
}

// canonicalJSON re-encodes jsonb exactly as billing.canonicalJSON does —
// UseNumber preserves literal number bytes so verify-side payload == the
// signed payload (a float re-encode would turn e.g. 1e3 into 1000).
func canonicalJSON(raw json.RawMessage) json.RawMessage {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

// Check reads + verifies the license file. Community (no file) is a clean
// status, not an error — §09 degrades predictably.
func (v *Validator) Check() (Status, *File, error) {
	if v.path == "" || v.pub == nil {
		return StatusCommunity, nil, nil
	}
	raw, err := os.ReadFile(v.path)
	if err != nil {
		if os.IsNotExist(err) {
			return StatusCommunity, nil, nil
		}
		return StatusInvalid, nil, err
	}
	var f File
	if json.Unmarshal(raw, &f) != nil {
		return StatusInvalid, nil, errors.New("license file malformed")
	}
	if f.DeploymentID == "" {
		// an empty/blank file means no license installed — community, not
		// invalid (invalid is for files that PARSE but don't verify).
		return StatusCommunity, nil, nil
	}
	sig, err := hex.DecodeString(f.Signature)
	if err != nil || !ed25519.Verify(v.pub, payload(&f), sig) {
		return StatusInvalid, &f, errors.New("license signature invalid")
	}
	now := time.Now()
	if !now.After(f.ExpiresAt) {
		return StatusValid, &f, nil
	}
	if now.Before(f.ExpiresAt.Add(time.Duration(v.graceDays) * 24 * time.Hour)) {
		return StatusGrace, &f, nil // refresh unreachable — warn, keep running
	}
	return StatusExpired, &f, nil
}

// Entitlement reads a capability/limit from the license entitlements —
// the offline mirror of the entitlement service on self-host.
func (f *File) Entitlement(key string) (int64, bool) {
	var ents map[string]any
	if json.Unmarshal(f.Entitlements, &ents) != nil {
		return 0, false
	}
	switch v := ents[key].(type) {
	case float64:
		return int64(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}
