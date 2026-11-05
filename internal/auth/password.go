// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
package auth

import "golang.org/x/crypto/bcrypt"

// bcryptCost 12 — DefaultCost (10) is the 2014 baseline; existing hashes
// still verify (cost is embedded), only new hashes get the bump.
const bcryptCost = 12

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(h), err
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}
