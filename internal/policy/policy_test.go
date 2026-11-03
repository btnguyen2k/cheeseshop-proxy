package policy

import (
	"strings"
	"testing"
)

func TestAllowlist(t *testing.T) {
	list, err := New([]string{"Friendly_Bard", "Company.*", "Partial*"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		allowed bool
	}{
		{"friendly-bard", true}, {"FRIENDLY.BARD", true},
		{"FrIeNdLy-._.-bArD", true}, {"friendly-bard-other", false},
		{"company.core", true}, {"COMPANY__core", true},
		{"company-core-subpackage", true}, {"Company", false}, {"CompanyExtra", false},
		{"Partial", true}, {"PartialPackage", true}, {"Other.Partial", false},
		{"", false}, {"../Company.Core", false}, {"_company", false},
		{"company_", false}, {"company\n", false}, {"Company.\u4e2d\u6587", false},
		{"company-" + strings.Repeat("x", 150), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := list.Allows(test.name); got != test.allowed {
				t.Fatalf("Allows(%q) = %v, want %v", test.name, got, test.allowed)
			}
		})
	}
	empty, err := New(nil)
	if err != nil || empty.Allows("requests") {
		t.Fatal("an empty allowlist must expose no packages")
	}
	all, err := New([]string{"*"})
	if err != nil || !all.Allows("Any.Valid_Project") || all.Allows("../invalid") {
		t.Fatal("an explicit * must allow all valid project names only")
	}
}

func TestInvalidPatterns(t *testing.T) {
	for _, pattern := range []string{"", " company-*", "company-* ", "company-**", "*.core", "company-*.core", ".company", "company.", "_company", "**", "company\n", "-*"} {
		t.Run(pattern, func(t *testing.T) {
			if _, err := New([]string{pattern}); err == nil {
				t.Fatalf("accepted invalid pattern %q", pattern)
			}
		})
	}
}
