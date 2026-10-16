package service

import (
	"strings"
	"unicode"

	"github.com/google/uuid"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
)

// mentioned returns the Bolu a message calls by name, in the order the names
// appear.
//
// A name is only a name after an `@`: a group where a Bolu is called "Biru" must
// not turn every sentence about biru into an order for it. The match is
// case-insensitive and whole-word, so "@Biru" reaches Biru while "@Biru Muda"
// reaches Biru Muda — the longest name wins, which is what keeps a roster with
// both of them from answering with the wrong one.
//
// A Bolu called by name is not filtered by the resting switch: the switch says
// "no new work", and a person typing its name is addressing it, which is the same
// rule an explicit agent_id already follows.
func mentioned(agents []chatdomain.AgentRef, text string) []chatdomain.AgentRef {
	body := []rune(strings.ToLower(text))
	if len(body) == 0 || len(agents) == 0 {
		return nil
	}

	names := make([][]rune, len(agents))
	for i, agent := range agents {
		names[i] = []rune(strings.ToLower(strings.TrimSpace(agent.Name)))
	}

	found := make([]chatdomain.AgentRef, 0, len(agents))
	seen := map[uuid.UUID]bool{}

	// The cursor moves by hand because a name that matched is skipped whole: the
	// `@` of a longer name must not be read twice.
	for at := 0; at < len(body); {
		if body[at] != '@' {
			at++
			continue
		}

		best := -1
		for i, name := range names {
			if len(name) == 0 || (best >= 0 && len(name) <= len(names[best])) {
				continue
			}
			if !namesTheBolu(body[at+1:], name) {
				continue
			}
			best = i
		}
		if best < 0 {
			at++
			continue
		}

		if !seen[agents[best].ID] {
			seen[agents[best].ID] = true
			found = append(found, agents[best])
		}
		at += 1 + len(names[best])
	}

	return found
}

// namesTheBolu reports whether the body starts with the name and the name ends
// where the name ends: "@Biru." calls Biru, "@Biru Muda" does not.
func namesTheBolu(body, name []rune) bool {
	if len(body) < len(name) {
		return false
	}
	for i, letter := range name {
		if body[i] != letter {
			return false
		}
	}
	if len(body) == len(name) {
		return true
	}
	return !isNameRune(body[len(name)])
}

// isNameRune reports whether a rune continues a name, which is what makes the
// match whole-word.
func isNameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
