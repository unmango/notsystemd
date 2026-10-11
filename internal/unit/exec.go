package unit

import (
	"fmt"
	"os"
	"strings"
)

// Command is one command from an Exec*= directive.
type Command struct {
	// Words are the command's path and arguments before environment
	// variable expansion.
	Words []Word

	// IgnoreFailure is the '-' prefix: a non-zero exit is not a failure.
	IgnoreFailure bool
	// Privileged is the '+' or '!' prefix: the command runs as the user
	// notsystemd runs as, rather than the unit's User=.
	Privileged bool
	// NoExpand is the ':' prefix: environment variables are not expanded.
	NoExpand bool
	// Argv0 is the '@' prefix: the second word becomes argv[0].
	Argv0 bool
}

// Word is one word of a command line.
type Word struct {
	Text string
	// Splits marks an unquoted word that is exactly "$NAME", which expands to
	// the variable's value split on whitespace.
	Splits bool
}

// ParseExec splits the value of an Exec*= directive into its commands, which
// are separated by a lone ";".
func ParseExec(value string) ([]Command, error) {
	words, err := splitWords(value)
	if err != nil {
		return nil, err
	}

	var commands []Command
	var current []Word
	for _, w := range words {
		if w.Text == ";" && !w.quoted {
			if len(current) > 0 {
				c, err := newCommand(current)
				if err != nil {
					return nil, err
				}
				commands = append(commands, c)
			}
			current = nil
			continue
		}
		current = append(current, w.Word)
	}
	if len(current) > 0 {
		c, err := newCommand(current)
		if err != nil {
			return nil, err
		}
		commands = append(commands, c)
	}
	return commands, nil
}

func newCommand(words []Word) (Command, error) {
	var c Command
	first := words[0].Text
	for len(first) > 0 {
		switch first[0] {
		case '-':
			c.IgnoreFailure = true
		case '+', '!':
			c.Privileged = true
		case ':':
			c.NoExpand = true
		case '@':
			c.Argv0 = true
		default:
			words[0].Text = first
			c.Words = words
			if c.Argv0 && len(words) < 2 {
				return c, fmt.Errorf("%q: '@' needs an argv[0] after the path", first)
			}
			return c, nil
		}
		first = first[1:]
	}
	return c, fmt.Errorf("command has no path")
}

// Argv expands the command against env and returns the executable path and
// the argv to run it with.
func (c Command) Argv(env map[string]string) (string, []string) {
	var argv []string
	for _, w := range c.Words {
		switch {
		case c.NoExpand:
			argv = append(argv, w.Text)
		case w.Splits:
			argv = append(argv, strings.Fields(env[w.Text[1:]])...)
		default:
			argv = append(argv, expand(w.Text, env))
		}
	}
	if len(argv) == 0 {
		return "", nil
	}
	path := argv[0]
	if c.Argv0 {
		argv = argv[1:]
	}
	return path, argv
}

// String renders the command roughly as written, for logs.
func (c Command) String() string {
	words := make([]string, len(c.Words))
	for i, w := range c.Words {
		words[i] = w.Text
	}
	return strings.Join(words, " ")
}

// expand replaces ${NAME} with its value and $$ with $. A bare $NAME inside a
// longer word is left alone, as systemd does.
func expand(s string, env map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				b.WriteByte(s[i])
				continue
			}
			b.WriteString(env[s[i+2:i+2+end]])
			i += 2 + end
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

type word struct {
	Word
	quoted bool
}

// splitWords splits a command line on whitespace, honouring single and double
// quotes and C-style backslash escapes.
func splitWords(s string) ([]word, error) {
	var words []word
	var b strings.Builder
	inWord, quoted := false, false
	var quote byte

	flush := func() {
		if inWord {
			text := b.String()
			words = append(words, word{
				Word:   Word{Text: text, Splits: !quoted && isVarRef(text)},
				quoted: quoted,
			})
		}
		b.Reset()
		inWord, quoted = false, false
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote = c
			inWord, quoted = true, true
		case quote == 0 && (c == ' ' || c == '\t' || c == '\n'):
			flush()
		case c == '\\':
			if i+1 >= len(s) {
				return nil, fmt.Errorf("%q: trailing backslash", s)
			}
			i++
			b.WriteString(unescape(s[i]))
			// An escaped character is literal: `\;` is not a separator.
			inWord, quoted = true, true
		default:
			b.WriteByte(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("%q: unterminated quote", s)
	}
	flush()
	return words, nil
}

func unescape(c byte) string {
	switch c {
	case 'n':
		return "\n"
	case 't':
		return "\t"
	case 'r':
		return "\r"
	case 'a':
		return "\a"
	case 'b':
		return "\b"
	case 'f':
		return "\f"
	case 'v':
		return "\v"
	case 's':
		return " "
	default:
		return string(c)
	}
}

func isVarRef(s string) bool {
	if len(s) < 2 || s[0] != '$' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 1 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// ParseEnvironment splits the value of an Environment= directive into its
// KEY=VALUE assignments.
func ParseEnvironment(value string) ([]string, error) {
	words, err := splitWords(value)
	if err != nil {
		return nil, err
	}
	var assignments []string
	for _, w := range words {
		if !strings.Contains(w.Text, "=") {
			return nil, fmt.Errorf("%q: expected KEY=VALUE", w.Text)
		}
		assignments = append(assignments, w.Text)
	}
	return assignments, nil
}

// ReadEnvironmentFile reads KEY=VALUE lines the way EnvironmentFile= does:
// '#' and ';' start comments, values may be single or double quoted, and a
// trailing backslash continues a value on the next line.
func ReadEnvironmentFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var assignments []string
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		for strings.HasSuffix(line, "\\") && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, "\\") + strings.TrimSpace(lines[i])
		}
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			q := value[0]
			value = value[1 : len(value)-1]
			if q == '"' {
				value = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`).Replace(value)
			}
		}
		assignments = append(assignments, key+"="+value)
	}
	return assignments, nil
}
