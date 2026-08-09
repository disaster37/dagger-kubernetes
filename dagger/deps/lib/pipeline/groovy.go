package pipeline

import (
	"strings"
)

// groovyBuilder helps build structured Groovy (Jenkins declarative pipeline)
// strings with proper escaping of $ and quotes to avoid GString interpolation
// collisions.
type groovyBuilder struct {
	b     strings.Builder
	indent int
}

func (g *groovyBuilder) write(s string) {
	g.b.WriteString(strings.Repeat("    ", g.indent))
	g.b.WriteString(s)
}

func (g *groovyBuilder) writeln(s string) {
	g.write(s)
	g.b.WriteByte('\n')
}

func (g *groovyBuilder) openBlock(s string) {
	g.writeln(s + " {")
	g.indent++
}

func (g *groovyBuilder) closeBlock() {
	g.indent--
	g.writeln("}")
}

func (g *groovyBuilder) groovyString(s string) string {
	// Escape $ to avoid GString interpolation, escape backslashes, escape single quotes
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "$", "\\$")
	s = strings.ReplaceAll(s, "'", "\\'")
	return "'" + s + "'"
}

// groovyEscapeDQ escapes a string for safe use inside a Groovy double-quoted
// string literal. Prevents GString ${} interpolation and " boundary breakout.
// Used as a package-level function so renderers can escape DefaultBranch values.
func groovyEscapeDQ(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "$", `\$`)
	return s
}

// groovyString is also available at package level for use in renderers.
func groovyString(s string) string {
	var gb groovyBuilder
	return gb.groovyString(s)
}
