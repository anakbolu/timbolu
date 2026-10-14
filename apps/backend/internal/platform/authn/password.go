// Package authn holds the authentication primitives: password hashing with
// argon2id, one-time token generation, and access-token issuing and
// verification. Nothing here knows about HTTP or the database.
package authn

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. The architecture document mandates argon2id; these
// values follow the OWASP recommendation (64 MiB, 3 iterations, 2 lanes).
const (
	argonMemory      = 64 * 1024 // KiB
	argonIterations  = 3
	argonParallelism = 2
	argonSaltLength  = 16
	argonKeyLength   = 32
)

// ErrInvalidHash means the stored hash is not in the expected encoded format.
var ErrInvalidHash = errors.New("authn: password hash is not in the argon2id format")

// HashPassword derives an argon2id hash and encodes it with its parameters, so
// a future parameter change can still verify old hashes.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("authn: password is required")
	}

	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("authn: generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, argonIterations, argonMemory, argonParallelism, argonKeyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches the encoded hash. The
// comparison is constant time.
func VerifyPassword(encoded, password string) (bool, error) {
	params, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	other := argon2.IDKey([]byte(password), salt, params.iterations, params.memory, params.parallelism, uint32(len(key)))

	return subtle.ConstantTimeCompare(key, other) == 1, nil
}

type argonParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	version, err := strconv.Atoi(strings.TrimPrefix(parts[2], "v="))
	if err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	var params argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memory, &params.iterations, &params.parallelism); err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	return params, salt, key, nil
}

// GenerateToken returns a fresh random token and the hash to store. Only the
// hash is persisted, so a database leak does not hand over live tokens.
func GenerateToken() (raw string, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("authn: generate token: %w", err)
	}

	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashToken(raw), nil
}

// HashToken hashes a token for lookup. SHA-256 is enough here because the token
// is high-entropy random, unlike a password.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// MinPasswordLength is the shortest accepted password. Longer is better, but a
// minimum keeps the validation message honest.
const MinPasswordLength = 8

// ValidatePassword enforces the password policy: long enough, mixed case, at
// least one digit, and not only whitespace. The register and reset screens
// mirror these rules as a checklist, so a change here must reach them too.
func ValidatePassword(password string) error {
	if len([]rune(password)) < MinPasswordLength {
		return fmt.Errorf("authn: password must be at least %d characters", MinPasswordLength)
	}
	if strings.TrimSpace(password) == "" {
		return errors.New("authn: password must not be only whitespace")
	}

	var upper, lower, digit bool
	for _, r := range password {
		switch {
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsLower(r):
			lower = true
		case unicode.IsDigit(r):
			digit = true
		}
	}

	if !upper || !lower {
		return errors.New("authn: password must mix uppercase and lowercase letters")
	}
	if !digit {
		return errors.New("authn: password must contain at least one digit")
	}

	return nil
}
