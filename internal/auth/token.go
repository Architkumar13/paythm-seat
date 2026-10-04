package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Issuer signs bearer tokens. The subject is the only identity a request may act as.
type Issuer struct {
	secret []byte
	ttl    time.Duration
}

type claims struct {
	UserID string `json:"uid"`
	jwt.RegisteredClaims
}

func NewIssuer(secret string, ttl time.Duration) *Issuer {
	return &Issuer{secret: []byte(secret), ttl: ttl}
}

func (i *Issuer) Sign(userID string) (string, error) {
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	})
	signed, err := token.SignedString(i.secret)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

func (i *Issuer) Parse(raw string) (string, error) {
	parsed, err := jwt.ParseWithClaims(raw, &claims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return i.secret, nil
	})
	if err != nil {
		return "", err
	}
	c, ok := parsed.Claims.(*claims)
	if !ok || !parsed.Valid || c.UserID == "" || c.Subject != c.UserID {
		return "", fmt.Errorf("invalid token")
	}
	return c.UserID, nil
}

// SecretEqual compares two secrets without leaking which one is shorter.
func SecretEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}
