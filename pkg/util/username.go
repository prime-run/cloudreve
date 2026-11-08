package util

import (
	"fmt"
	"regexp"
	"unicode/utf8"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

const (
	UsernameMinLen = 5
	UsernameMaxLen = 100
)

// ValidateUsername checks identity username rules: length ≥ 5, charset [a-zA-Z0-9_-], no email format.
func ValidateUsername(username string) error {
	n := utf8.RuneCountInString(username)
	if n < UsernameMinLen {
		return fmt.Errorf("username must be at least %d characters", UsernameMinLen)
	}
	if n > UsernameMaxLen {
		return fmt.Errorf("username must be at most %d characters", UsernameMaxLen)
	}
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("username may only contain letters, digits, underscore, and hyphen")
	}
	return nil
}
