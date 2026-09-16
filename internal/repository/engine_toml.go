package repository

import (
	"fmt"
	"sort"
	"strings"
)

// engineTOML is the legacy BuildKit-style Dagger engine configuration.
// Engines >= v0.19 automatically read it from /etc/dagger/engine.toml.
type engineTOML struct {
	Debug           bool
	LogFormat       string
	RegistryMirrors map[string][]string
	MirrorHTTP      []string // mirror host[:port] strings dialed over plaintext HTTP
}

// render returns the engine.toml content, or "" when the configuration is
// empty (no debug, no log format, no mirrors). Output is deterministic:
// registry hosts are sorted alphabetically and sections are separated by
// blank lines.
func (c engineTOML) render() string {
	sections := make([]string, 0, 2+len(c.RegistryMirrors)+len(c.MirrorHTTP))
	if c.Debug {
		sections = append(sections, "debug = true\n")
	}
	if c.LogFormat != "" {
		sections = append(sections, fmt.Sprintf("[log]\n  format = %s\n", tomlQuote(c.LogFormat)))
	}
	for _, host := range mirrorHosts(c.RegistryMirrors) {
		mirrors := c.RegistryMirrors[host]
		quoted := make([]string, len(mirrors))
		for i, mirror := range mirrors {
			quoted[i] = tomlQuote(mirror)
		}
		sections = append(sections, fmt.Sprintf("[registry.%s]\n  mirrors = [%s]\n", tomlQuote(host), strings.Join(quoted, ", ")))
	}
	for _, mirror := range sortedUnique(c.MirrorHTTP) {
		sections = append(sections, fmt.Sprintf("[registry.%s]\n  http = true\n", tomlQuote(mirror)))
	}
	return strings.Join(sections, "\n")
}

// sortedUnique returns the non-empty entries of in, sorted alphabetically and
// deduplicated, for deterministic output.
func sortedUnique(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// mirrorHosts returns the hosts that have at least one mirror, sorted
// alphabetically for deterministic output.
func mirrorHosts(mirrors map[string][]string) []string {
	hosts := make([]string, 0, len(mirrors))
	for host, list := range mirrors {
		if len(list) > 0 {
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	return hosts
}

// tomlQuote returns s as a quoted TOML basic string ("...") with all special
// characters escaped.
func tomlQuote(s string) string {
	return fmt.Sprintf("\"%s\"", tomlEscape(s))
}

// tomlEscape escapes s for use in a TOML basic ("...") string: backslash,
// double quote, \b \f \n \r \t, and any remaining control byte as \u00XX.
func tomlEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString("\\\\")
		case '"':
			b.WriteString("\\\"")
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, "\\u%04x", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}
