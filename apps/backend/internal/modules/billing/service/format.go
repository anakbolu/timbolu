package service

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rupiah and token figures are rendered here rather than in a screen, because they
// appear in the notices the backend sends: an email cannot call a formatter, and two
// formatters would be two answers to "how much".

// formatIDR renders rupiah the way an Indonesian reader writes it.
func formatIDR(amount int64) string {
	whole := strconv.FormatInt(amount, 10)
	grouped := group(whole, 3, ".")

	return "Rp " + grouped
}

// formatTokens renders a token count with thousands separators.
func formatTokens(tokens int64) string {
	grouped := group(strconv.FormatInt(tokens, 10), 3, ".")

	return grouped
}

// formatDate renders a moment as a date in Jakarta, which is where the customers
// are and therefore which calendar day they read.
func formatDate(at time.Time) string {
	location, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		location = time.UTC
	}
	return at.In(location).Format("2 January 2006")
}

// group inserts a separator every size digits, from the right.
func group(digits string, size int, separator string) string {
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign = "-"
		digits = digits[1:]
	}

	var out strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%size == 0 {
			out.WriteString(separator)
		}
		out.WriteRune(digit)
	}

	if out.Len() == 0 {
		return sign + fmt.Sprintf("%d", 0)
	}
	return sign + out.String()
}
