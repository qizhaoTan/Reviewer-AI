// Package ignore defines the shared exclusion rules for diffs and file tools.
package ignore

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Validate accepts slash-separated patterns with *, ? and whole-segment **.
// Negation is intentionally unsupported: every rule is an exclusion.
func Validate(rules []string) error {
	for _, rule := range rules {
		p := strings.TrimSuffix(strings.TrimPrefix(rule, "/"), "/")
		if p == "" || strings.ContainsAny(p, "![]{}\\\x00\n\r") {
			return fmt.Errorf("invalid ignore pattern %q; use relative glob patterns with *, ? or ** (e.g. *.log, logs/**)", rule)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == "" || seg == "." || seg == ".." || (strings.Contains(seg, "**") && seg != "**") {
				return fmt.Errorf("invalid ignore pattern %q; use relative paths and ** as a complete path segment", rule)
			}
		}
	}
	return nil
}

// Match uses repository-relative paths. Patterns without a slash match names
// at any depth; slash-containing or leading-slash patterns start at the root.
// Matching a directory also excludes all its descendants.
func Match(rules []string, name string) bool {
	name = strings.TrimPrefix(path.Clean(filepath.ToSlash(name)), "./")
	for _, rule := range rules {
		rooted := strings.HasPrefix(rule, "/")
		p := strings.TrimSuffix(strings.TrimPrefix(rule, "/"), "/")
		for candidate := name; candidate != "." && candidate != "/"; candidate = path.Dir(candidate) {
			if !rooted && !strings.Contains(p, "/") {
				if ok, _ := path.Match(p, path.Base(candidate)); ok {
					return true
				}
			} else if matchSegments(strings.Split(p, "/"), strings.Split(candidate, "/")) {
				return true
			}
		}
	}
	return false
}

func matchSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		return matchSegments(pattern[1:], name) || (len(name) > 0 && matchSegments(pattern, name[1:]))
	}
	if len(name) == 0 {
		return false
	}
	ok, _ := path.Match(pattern[0], name[0])
	return ok && matchSegments(pattern[1:], name[1:])
}

// Excluded also checks symlink targets, so aliases cannot bypass exclusions.
func Excluded(rules []string, root, name string) bool {
	if Match(rules, name) {
		return true
	}
	if len(rules) == 0 {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	return err == nil && Match(rules, rel)
}

// RipgrepArgs puts exclusions after the caller's positive glob, so no_ignore
// and explicit search globs cannot override configured exclusions.
func RipgrepArgs(rules []string) []string {
	var args []string
	for _, rule := range rules {
		p := strings.TrimSuffix(rule, "/")
		if strings.Contains(p, "/") && !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		args = append(args, "--glob", "!"+p)
	}
	return args
}
