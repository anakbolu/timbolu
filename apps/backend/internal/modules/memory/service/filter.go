package service

import (
	"regexp"
	"strings"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
)

// The patterns a memory must not carry. They are labels plus a value rather than
// bare numbers, because a bare number is a price, an order number, or a date as
// often as it is a secret — and a filter that refuses ordinary business facts is
// a filter people turn off.
var (
	// A password is recognised by its label: the value after it is what must not
	// be stored.
	passwordPattern = regexp.MustCompile(`(?i)\b(?:kata\s*sandi|password|passwd|pwd|pin)\b\s*(?::|=|-|>|adalah|ialah)?\s*\S{4,}`)
	// A one-time code is digits near the words that mean one.
	otpPattern = regexp.MustCompile(`(?i)\b(?:otp|kode\s*otp|kode\s*verifikasi|kode\s*rahasia|kode\s*autentikasi|token)\b[^\d\n]{0,20}\d{4,8}\b`)
	// An identity or account number carries its own label.
	identityPattern = regexp.MustCompile(`(?i)\b(?:nik|ktp|npwp|nomor\s*identitas|nomor\s*rekening|rekening|nomor\s*kartu|cvv|cvc)\b[^\d\n]{0,20}\d{3,20}\b`)
	// A long digit run is a card candidate; the Luhn check below is what decides.
	cardCandidate = regexp.MustCompile(`(?:\d[ -]?){12,18}\d`)
)

// Sensitive reports whether text carries something that must not be remembered,
// with a short reason in the language the user reads.
//
// The reason is the phrase the API returns and the chat confirmation names, so it
// is a noun a person understands ("nomor kartu") and never the name of a rule.
//
// The check runs before every write — a person asking for it, the extractor
// proposing it, or a document being ingested — because a filter that only guards
// one of those three doors is not a filter.
func Sensitive(text string) (string, bool) {
	if passwordPattern.MatchString(text) {
		return "kata sandi", true
	}
	if otpPattern.MatchString(text) {
		return "kode OTP", true
	}
	if identityPattern.MatchString(text) {
		return "nomor identitas atau rekening", true
	}

	for _, candidate := range cardCandidate.FindAllString(text, -1) {
		digits := onlyDigits(candidate)
		// Luhn holds for card numbers and for about one in ten arbitrary long
		// numbers, so it is what keeps an invoice number from being refused.
		if len(digits) >= 13 && len(digits) <= 19 && luhn(digits) {
			return "nomor kartu", true
		}
	}

	return "", false
}

func onlyDigits(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range value {
		if r >= '0' && r <= '9' {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// luhn is the checksum every card number satisfies.
func luhn(digits string) bool {
	sum := 0
	double := false

	for i := len(digits) - 1; i >= 0; i-- {
		value := int(digits[i] - '0')
		if double {
			value *= 2
			if value > 9 {
				value -= 9
			}
		}
		sum += value
		double = !double
	}

	return sum%10 == 0
}

// sensitiveSentence is what a Bolu answers when something must not be remembered.
//
// It names the reason and says what to do about it, because a refusal that only
// says "no" leaves a person guessing which part of their sentence was the problem.
func sensitiveSentence(reason string) string {
	return "Aku tidak menyimpannya karena ada " + reason + " di dalamnya. " +
		"Tulis lagi tanpa bagian itu ya, supaya aman untuk kuingat."
}

// rememberMarkers are the ways a person asks for something to be remembered, in
// the order they are checked. The longest marker wins, so "tolong ingat ya"
// strips the whole phrase rather than leaving "ya" behind.
var rememberMarkers = []string{
	"mulai sekarang",
	"tolong ingat ya",
	"tolong ingat",
	"jangan lupa ya",
	"jangan lupa",
	"ingat ya",
	"ingatlah",
	"ingat dong",
	"ingat:",
	"ingat bahwa",
	"inget ya",
	"inget:",
	"catat ya",
	"catat:",
	"note:",
	"notes:",
	"perlu diingat",
}

// RememberIntent reads a message that asks for something to be remembered.
//
// Detection is a pattern rather than a model call because it has to be free and
// instant: it runs on every message, and a message that is not a request must
// not spend a token. What it finds is the content — the words after the marker —
// which is what gets stored and what the confirmation names.
func RememberIntent(text string) (content string, ok bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", false
	}

	lower := strings.ToLower(trimmed)

	for _, marker := range rememberMarkers {
		if !strings.HasPrefix(lower, marker) {
			continue
		}

		rest := trimmed[len(marker):]
		// The marker is usually followed by punctuation or a space, both of
		// which are part of the sentence rather than of the memory.
		rest = strings.TrimLeft(rest, " ,.:;-–—\t\n")
		rest = strings.TrimSpace(rest)

		if rest == "" {
			// "ingat ya" on its own asks for nothing in particular. Answering it
			// with a confirmation of nothing would be worse than letting the
			// ordinary reply ask what to remember.
			return "", false
		}

		return rest, true
	}

	return "", false
}

// ClassifyKind guesses what sort of memory this is. The marker settles it when
// one is present; otherwise the words decide, and the default is a fact.
func ClassifyKind(content string) string {
	lower := strings.ToLower(content)

	switch {
	case containsAny(lower, "mulai sekarang", "selalu", "jangan", "usahakan", "ke depannya", "setiap kali"):
		return domain.KindInstruction
	case containsAny(lower, "suka", "lebih baik", "prefer", "utamakan", "senang", "kesukaan"):
		return domain.KindPreference
	}

	return domain.KindFact
}

// ClassifyLayer decides which memory a direct request belongs to.
//
// Talking about the Bolu in the second person — "kamu", "anda" — is talking
// about that Bolu's own way of working, which belongs to it alone. Anything else
// is a fact about the workspace, and the main memory is what every call gets.
func ClassifyLayer(content string) string {
	padded := " " + strings.ToLower(content) + " "

	if containsAny(padded, " kamu ", " anda ", " kau ", " kalian ") {
		return domain.LayerPrivate
	}

	return domain.LayerMain
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}
