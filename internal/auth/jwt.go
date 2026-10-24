package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type Claims struct {
	jwt.RegisteredClaims
}

func IssueToken(secret string, userID, jti uuid.UUID, ttl time.Duration) (string, error) {
	c := Claims{RegisteredClaims: jwt.RegisteredClaims{
		Subject:   userID.String(),
		ID:        jti.String(),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
		Issuer:    "oga9",
	}}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
}

func ParseToken(secret, token string) (*Claims, error) {
	var c Claims
	t, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("oga9"))
	if err != nil || !t.Valid {
		return nil, errors.New("invalid token")
	}
	return &c, nil
}
