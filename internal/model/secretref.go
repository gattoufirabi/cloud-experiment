package model

import "regexp"

// SecretRefRe matches a variable whose whole value references an organization secret:
// ${secret:NAME}.
var SecretRefRe = regexp.MustCompile(`^\$\{secret:([A-Za-z0-9][A-Za-z0-9_.-]{0,63})\}$`)

// SecretRef returns the secret a value references, if any.
func SecretRef(value string) (string, bool) {
	m := SecretRefRe.FindStringSubmatch(value)
	if m == nil {
		return "", false
	}
	return m[1], true
}
