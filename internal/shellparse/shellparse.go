// Package shellparse splits a shell command line into the simple commands
// it would run, so cloudundo can see every aws invocation an agent issues,
// including ones chained with && or wrapped in sudo, env or bash -c.
//
// It is deliberately conservative: anything it cannot resolve statically
// (variables, command substitution, xargs) marks the command Uncertain
// instead of guessing.
package shellparse

import (
	"path/filepath"
	"strings"
)

// Simple is one command the shell would execute.
type Simple struct {
	Args []string
	// Uncertain means some arguments come from somewhere cloudundo cannot
	// see (a variable, $(...), backticks, xargs input).
	Uncertain bool
	// Env holds variables set for this command: inline (AWS_PROFILE=x aws ...),
	// through env, or by an earlier "export" in the same line.
	Env map[string]string
}

// maxDepth bounds bash -c "..." recursion.
const maxDepth = 4

// Split returns every simple command in line, with wrappers like sudo,
// env, timeout and bash -c removed.
func Split(line string) []Simple {
	return split(line, 0)
}

func split(line string, depth int) []Simple {
	var out []Simple
	cmds, subs := tokenize(line)
	exported := map[string]string{}
	for _, raw := range cmds {
		for _, s := range unwrap(raw, depth) {
			s.Env = mergeEnv(exported, s.Env)
			if len(s.Args) > 0 && s.Args[0] == "export" {
				for _, a := range s.Args[1:] {
					if k, v, ok := assignment(a); ok {
						exported[k] = v
					}
				}
			}
			out = append(out, s)
		}
	}
	// Commands inside $( ... ) and backticks run too.
	if depth < maxDepth {
		for _, s := range subs {
			out = append(out, split(s, depth+1)...)
		}
	}
	return out
}

// readDollarParen returns the index of the ")" closing the "$(" at i.
func readDollarParen(rs []rune, i int) int {
	depth := 0
	for j := i + 1; j < len(rs); j++ {
		switch rs[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return len(rs) - 1
}

// readBacktick returns the index of the backtick closing the one at i.
func readBacktick(rs []rune, i int) int {
	for j := i + 1; j < len(rs); j++ {
		if rs[j] == '`' {
			return j
		}
	}
	return len(rs) - 1
}

// substitution copies a $( ... ) or `...` starting at i into buf and
// returns the inner command and the index of its last rune.
func substitution(rs []rune, i int, buf *strings.Builder) (inner string, end int) {
	if rs[i] == '$' {
		end = readDollarParen(rs, i)
		inner = string(rs[min(i+2, end):end])
	} else {
		end = readBacktick(rs, i)
		inner = string(rs[min(i+1, end):end])
	}
	buf.WriteString(string(rs[i : end+1]))
	return inner, end
}

type word struct {
	text      string
	uncertain bool
}

// tokenize breaks line into simple commands made of words. Operators
// (; & | && || newline parentheses) end a command; redirections and their
// targets are dropped.
func tokenize(line string) ([][]word, []string) {
	var (
		subs      []string
		cmds      [][]word
		cur       []word
		buf       strings.Builder
		inWord    bool
		uncertain bool
		skipNext  bool // next word is a redirection target
	)

	flushWord := func() {
		if !inWord {
			return
		}
		w := word{text: buf.String(), uncertain: uncertain}
		if skipNext {
			skipNext = false
		} else {
			cur = append(cur, w)
		}
		buf.Reset()
		inWord = false
		uncertain = false
	}
	flushCmd := func() {
		flushWord()
		if len(cur) > 0 {
			cmds = append(cmds, cur)
		}
		cur = nil
		skipNext = false
	}

	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\\' && i+1 < len(rs):
			if rs[i+1] == '\n' { // line continuation
				i++
				continue
			}
			buf.WriteRune(rs[i+1])
			inWord = true
			i++
		case c == '\'':
			inWord = true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				buf.WriteRune(rs[j])
				j++
			}
			i = j
		case c == '"':
			inWord = true
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' && j+1 < len(rs) && strings.ContainsRune("\"\\$`", rs[j+1]) {
					buf.WriteRune(rs[j+1])
					j += 2
					continue
				}
				if rs[j] == '$' || rs[j] == '`' {
					uncertain = true
					if rs[j] == '`' || (j+1 < len(rs) && rs[j+1] == '(') {
						inner, end := substitution(rs, j, &buf)
						subs = append(subs, inner)
						j = end + 1
						continue
					}
				}
				buf.WriteRune(rs[j])
				j++
			}
			i = j
		case c == '$' || c == '`':
			uncertain = true
			inWord = true
			if c == '`' || (i+1 < len(rs) && rs[i+1] == '(') {
				inner, end := substitution(rs, i, &buf)
				subs = append(subs, inner)
				i = end
				continue
			}
			buf.WriteRune(c)
		case c == ' ' || c == '\t':
			flushWord()
		case c == '\n' || c == ';' || c == '&' || c == '|' || c == '(' || c == ')':
			flushCmd()
		case c == '>' || c == '<':
			// "2>" style: a pending all-digit word is the fd number.
			if inWord && isDigits(buf.String()) {
				buf.Reset()
				inWord = false
			} else {
				flushWord()
			}
			for i+1 < len(rs) && (rs[i+1] == '>' || rs[i+1] == '&') {
				i++
			}
			// "2>&1": the target is an fd, not a file word.
			if i+1 < len(rs) && rs[i] == '&' && rs[i+1] >= '0' && rs[i+1] <= '9' {
				i++
				continue
			}
			skipNext = true
		case c == '#' && !inWord:
			// Comment to end of line.
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			flushCmd()
		default:
			buf.WriteRune(c)
			inWord = true
		}
	}
	flushCmd()
	return cmds, subs
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// unwrap strips env assignments and wrapper programs, recursing into
// shell -c strings.
func unwrap(ws []word, depth int) []Simple {
	uncertain := false
	env := map[string]string{}
	setEnv := func(w string) {
		if k, v, ok := assignment(w); ok {
			env[k] = v
		}
	}
	i := 0
	for i < len(ws) {
		w := ws[i].text
		switch {
		case shellKeywords[w]:
			// "do aws s3 rm ..." inside a loop, "if aws ...", "! aws ...".
			i++
		case isAssignment(w):
			setEnv(w)
			i++
		case base(w) == "sudo" || base(w) == "doas":
			i++
			for i < len(ws) && strings.HasPrefix(ws[i].text, "-") {
				if ws[i].text == "-u" || ws[i].text == "-g" {
					i++
				}
				i++
			}
		case base(w) == "env":
			i++
			for i < len(ws) && (strings.HasPrefix(ws[i].text, "-") || isAssignment(ws[i].text)) {
				setEnv(ws[i].text)
				i++
			}
		case base(w) == "time" || base(w) == "command" || base(w) == "exec" || base(w) == "nohup":
			i++
			for i < len(ws) && strings.HasPrefix(ws[i].text, "-") {
				i++
			}
		case base(w) == "nice" || base(w) == "timeout":
			i++
			for i < len(ws) && strings.HasPrefix(ws[i].text, "-") {
				i++
			}
			if base(w) == "timeout" && i < len(ws) {
				i++ // duration
			}
		case base(w) == "xargs":
			// xargs appends arguments we cannot see.
			uncertain = true
			i++
			for i < len(ws) && strings.HasPrefix(ws[i].text, "-") {
				if ws[i].text == "-I" || ws[i].text == "-n" || ws[i].text == "-P" || ws[i].text == "-d" {
					i++
				}
				i++
			}
		case isShell(w):
			// bash -c "string" / sh -lc "string"
			j := i + 1
			for j < len(ws) && strings.HasPrefix(ws[j].text, "-") {
				if strings.Contains(ws[j].text, "c") && !strings.HasPrefix(ws[j].text, "--") {
					if j+1 < len(ws) && depth < maxDepth {
						inner := split(ws[j+1].text, depth+1)
						for k := range inner {
							inner[k].Env = mergeEnv(env, inner[k].Env)
							if uncertain || ws[j+1].uncertain {
								inner[k].Uncertain = true
							}
						}
						return inner
					}
				}
				j++
			}
			return wordsToSimple(ws[i:], uncertain, env)
		default:
			return wordsToSimple(ws[i:], uncertain, env)
		}
	}
	return nil
}

func wordsToSimple(ws []word, uncertain bool, env map[string]string) []Simple {
	if len(ws) == 0 {
		return nil
	}
	s := Simple{Uncertain: uncertain}
	if len(env) > 0 {
		s.Env = env
	}
	for _, w := range ws {
		s.Args = append(s.Args, w.text)
		if w.uncertain {
			s.Uncertain = true
		}
	}
	return []Simple{s}
}

// shellKeywords can precede a command in loops and conditionals.
var shellKeywords = map[string]bool{
	"do": true, "then": true, "else": true, "elif": true, "if": true,
	"while": true, "until": true, "!": true, "{": true, "}": true,
	"done": true, "fi": true,
}

// assignment splits NAME=value.
func assignment(w string) (string, string, bool) {
	if !isAssignment(w) {
		return "", "", false
	}
	eq := strings.IndexByte(w, '=')
	return w[:eq], w[eq+1:], true
}

// mergeEnv returns base overlaid with top, or nil when both are empty.
func mergeEnv(base, top map[string]string) map[string]string {
	if len(base) == 0 && len(top) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(top))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range top {
		out[k] = v
	}
	return out
}

func isAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range w[:eq] {
		ok := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func isShell(w string) bool {
	switch base(w) {
	case "bash", "sh", "zsh", "dash", "ksh":
		return true
	}
	return false
}

func base(w string) string { return filepath.Base(w) }
