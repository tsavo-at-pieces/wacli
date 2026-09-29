package transcribe

import "fmt"

// SplitCommand splits a command template into argv without a shell.
//
// Whitespace separates arguments. Single and double quotes group text that
// contains whitespace; quoted and unquoted text next to each other join into
// one argument (--opt='a b' is "--opt=a b"), and an empty pair of quotes is an
// empty argument. Both quote styles are literal: there is no variable,
// glob, or tilde expansion, and backslashes are ordinary characters so
// Windows paths need no escaping. To put a double quote in an argument, wrap
// it in single quotes, and the other way round.
func SplitCommand(template string) ([]string, error) {
	var (
		args    []string
		current []rune
		inArg   bool
		quote   rune
	)
	for _, r := range template {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current = append(current, r)
		case r == '\'' || r == '"':
			quote = r
			inArg = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inArg {
				args = append(args, string(current))
				current = current[:0]
				inArg = false
			}
		default:
			current = append(current, r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote in command template", quote)
	}
	if inArg {
		args = append(args, string(current))
	}
	return args, nil
}
