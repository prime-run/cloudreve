package util

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	UsernameMinLen = 5
	UsernameMaxLen = 100
)

// NormalizeUsername trims space and lowercases for case-insensitive identity.
func NormalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// ValidateUsername checks identity username rules: length ≥ 5 and ≤ 100.
// There is no email-format or charset restriction — migrated addresses remain valid.
func ValidateUsername(username string) error {
	n := utf8.RuneCountInString(username)
	if n < UsernameMinLen {
		return fmt.Errorf("username must be at least %d characters", UsernameMinLen)
	}
	if n > UsernameMaxLen {
		return fmt.Errorf("username must be at most %d characters", UsernameMaxLen)
	}
	return nil
}
