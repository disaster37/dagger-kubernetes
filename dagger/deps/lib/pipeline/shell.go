package pipeline

import (
	"strings"
)

// shellQuote wraps a string in single quotes for safe insertion in a shell command.
// Embedded single quotes are escaped as '\'' (end quote, escaped quote, restart quote).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// ghExprQuote escapes a string for safe use inside a GitHub Actions expression string literal
// delimited by single quotes. GitHub Actions uses '' to represent a literal single quote.
func ghExprQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// shellNeedsQuoting returns true if a shell command argument needs quoting.
// Flags (starting with -- or - with a non-numeric second char) and shell variable
// references (containing ${) are excluded; all other values should be quoted.
func shellNeedsQuoting(arg string) bool {
	if strings.HasPrefix(arg, "--") {
		return false
	}
	if len(arg) >= 2 && arg[0] == '-' && !(arg[1] >= '0' && arg[1] <= '9') {
		return false
	}
	if strings.Contains(arg, "${") {
		return false
	}
	return true
}

// quoteArgsForShell applies shellQuote to each arg that needs quoting,
// skipping flags and shell variable references that must remain unquoted.
func quoteArgsForShell(args []string) []string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if shellNeedsQuoting(arg) {
			quoted[i] = shellQuote(arg)
		} else {
			quoted[i] = arg
		}
	}
	return quoted
}
