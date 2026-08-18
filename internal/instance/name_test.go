package instance

import (
	"strings"
	"testing"

	"github.com/sarcasticbird/wrap/internal/target"
)

func TestValidateNameRejectsUnsafeLabels(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", " ", "a/b", "a\\b", "line\nbreak", strings.Repeat("x", 65)} {
		if err := ValidateName(value); err == nil {
			t.Errorf("ValidateName(%q) succeeded", value)
		}
	}
	for _, value := range []string{"api", "list", "release review", "日本語"} {
		if err := ValidateName(value); err != nil {
			t.Errorf("ValidateName(%q) = %v", value, err)
		}
	}
}

func TestChooseNameDerivesAndSuffixesDeterministically(t *testing.T) {
	t.Parallel()

	targetValue := target.Target{Directory: "/work/widget", WindowName: "editor"}
	records := []Record{{Name: "widget"}, {Name: "widget-2"}}
	got, err := ChooseName("", targetValue, records)
	if err != nil || got != "widget-3" {
		t.Fatalf("ChooseName() = %q, %v; want widget-3", got, err)
	}
	got, err = ChooseName("api", targetValue, records)
	if err != nil || got != "api" {
		t.Fatalf("ChooseName(requested) = %q, %v", got, err)
	}
	if _, err := ChooseName("widget", targetValue, records); err == nil {
		t.Fatal("requested duplicate name succeeded")
	}
}
