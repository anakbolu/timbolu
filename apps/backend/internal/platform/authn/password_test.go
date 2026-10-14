package authn

import (
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("hash is not argon2id: %q", hash)
	}
	if strings.Contains(hash, password) {
		t.Fatal("the hash contains the password")
	}

	ok, err := VerifyPassword(hash, password)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("the correct password must verify")
	}

	wrong, err := VerifyPassword(hash, "wrong password")
	if err != nil {
		t.Fatalf("verify wrong: %v", err)
	}
	if wrong {
		t.Fatal("a wrong password must not verify")
	}
}

// TestHashPasswordIsSalted proves two hashes of the same password differ, so the
// stored value does not reveal that two accounts share a password.
func TestHashPasswordIsSalted(t *testing.T) {
	first, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	second, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if first == second {
		t.Fatal("two hashes of the same password are identical: the salt is not random")
	}
}

func TestHashPasswordRejectsEmpty(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Fatal("an empty password must be rejected")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"not encoded":      "plaintext",
		"wrong algorithm":  "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$a2V5",
		"unknown version":  "$argon2id$v=13$m=65536,t=3,p=2$c2FsdA$a2V5",
		"bad parameters":   "$argon2id$v=19$m=x,t=y,p=z$c2FsdA$a2V5",
		"bad salt base64":  "$argon2id$v=19$m=65536,t=3,p=2$!!!$a2V5",
		"bad key base64":   "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$!!!",
		"too few sections": "$argon2id$v=19$m=65536,t=3,p=2",
	}

	for name, hash := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyPassword(hash, "password"); err == nil {
				t.Fatalf("expected an error for %q", hash)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	valid := []string{"Passw0rd", "longer-Password-value1", "Abcdefgh1"}
	for _, password := range valid {
		if err := ValidatePassword(password); err != nil {
			t.Errorf("%q must be accepted: %v", password, err)
		}
	}

	invalid := map[string]string{
		"too short":      "Ab1",
		"empty":          "",
		"only spaces":    "         ",
		"no uppercase":   "password1",
		"no lowercase":   "PASSWORD1",
		"no digit":       "Passworddd",
		"only digits":    "12345678",
		"only uppercase": "PASSWORD",
	}
	for name, password := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := ValidatePassword(password); err == nil {
				t.Fatalf("%q must be rejected", password)
			}
		})
	}
}

func TestGenerateToken(t *testing.T) {
	raw, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if raw == "" || hash == "" {
		t.Fatal("token and hash must both be present")
	}
	if raw == hash {
		t.Fatal("the stored hash must differ from the raw token")
	}
	if hash != HashToken(raw) {
		t.Fatal("the returned hash must be HashToken(raw)")
	}

	other, _, err := GenerateToken()
	if err != nil {
		t.Fatalf("generate second: %v", err)
	}
	if other == raw {
		t.Fatal("two generated tokens are identical")
	}
}

func TestHashTokenIsStable(t *testing.T) {
	const raw = "a-token"

	// HashToken is a pure function: the same input always produces the same
	// digest, which is what makes a stored hash lookup work.
	first := HashToken(raw)
	if first != HashToken(raw) {
		t.Fatal("HashToken must be deterministic")
	}
	if first == HashToken(raw+"x") {
		t.Fatal("different tokens must hash differently")
	}
	if strings.Contains(HashToken(raw), raw) {
		t.Fatal("the hash must not contain the token")
	}
}
