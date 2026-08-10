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
}

// render returns the engine.toml content, or "" when the configuration is
// empty (no debug, no log format, no mirrors). Output is deterministic:
// registry hosts are sorted alphabetically and sections are separated by
// blank lines.
func (c engineTOML) render() string {
	sections := make([]string, 0, 2+len(c.RegistryMirrors))
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
	return strings.Join(sections, "\n")
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
				b.WriteString(fmt.Sprintf("\\u%04x", c))
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}
