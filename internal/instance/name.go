package instance

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sarcasticbird/wrap/internal/target"
)

func ValidateName(value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return errors.New("name must not be empty or have outer whitespace")
	}
	if !utf8.ValidString(value) {
		return errors.New("name must be valid UTF-8")
	}
	if utf8.RuneCountInString(value) > 64 {
		return errors.New("name must be 64 characters or fewer")
	}
	for _, char := range value {
		if char == '/' || char == '\\' || !unicode.IsPrint(char) {
			return fmt.Errorf("name contains unsafe character %q", char)
		}
	}
	return nil
}

func ChooseName(requested string, targetValue target.Target, records []Record) (string, error) {
	used := make(map[string]struct{}, len(records))
	for _, record := range records {
		used[record.Name] = struct{}{}
	}
	if requested != "" {
		if err := ValidateName(requested); err != nil {
			return "", err
		}
		if _, exists := used[requested]; exists {
			return "", fmt.Errorf("wrap name %q is already in use", requested)
		}
		return requested, nil
	}
	base := derivedName(targetValue)
	if _, exists := used[base]; !exists {
		return base, nil
	}
	for suffix := 2; ; suffix++ {
		candidate := base + "-" + strconv.Itoa(suffix)
		if _, exists := used[candidate]; !exists {
			return candidate, nil
		}
	}
}

func derivedName(targetValue target.Target) string {
	value := filepath.Base(filepath.Clean(targetValue.Directory))
	if value == "." || value == string(filepath.Separator) || value == "" {
		value = targetValue.WindowName
	}
	var builder strings.Builder
	for _, char := range value {
		switch {
		case char == '/' || char == '\\' || !unicode.IsPrint(char):
			builder.WriteRune('_')
		default:
			builder.WriteRune(char)
		}
		if utf8.RuneCountInString(builder.String()) == 48 {
			break
		}
	}
	value = strings.TrimSpace(builder.String())
	if value == "" {
		return "wrap"
	}
	return value
}
