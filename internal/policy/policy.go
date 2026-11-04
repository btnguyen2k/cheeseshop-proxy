package policy

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	projectName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	separators  = regexp.MustCompile(`[-_.]+`)
)

type Allowlist struct {
	all      bool
	exact    map[string]struct{}
	prefixes []string
}

func ValidName(name string) bool {
	return projectName.MatchString(name)
}

func Normalize(name string) string {
	return strings.ToLower(separators.ReplaceAllString(name, "-"))
}

func New(patterns []string) (*Allowlist, error) {
	list := &Allowlist{exact: make(map[string]struct{})}
	for _, pattern := range patterns {
		if pattern == "*" {
			list.all = true
		} else if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if !ValidName(strings.TrimRight(prefix, "._-")) || strings.Contains(prefix, "*") {
				return nil, fmt.Errorf("invalid package prefix %q", pattern)
			}
			list.prefixes = append(list.prefixes, Normalize(prefix))
		} else {
			if !ValidName(pattern) {
				return nil, fmt.Errorf("invalid project name %q", pattern)
			}
			list.exact[Normalize(pattern)] = struct{}{}
		}
	}
	return list, nil
}

func (a *Allowlist) Allows(name string) bool {
	if !ValidName(name) {
		return false
	}
	if a.all {
		return true
	}
	normalized := Normalize(name)
	if _, ok := a.exact[normalized]; ok {
		return true
	}
	for _, prefix := range a.prefixes {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}
