package known

import (
	"slices"
	"strings"
)

// glob reports whether name matches pattern as a shell's case statement
// matches it under a UTF-8 locale. "*" matches any string, "/" included;
// "?" any one character; "[...]" one character of a set (see bracket); a
// backslash makes the next character plain, and one ending the pattern is
// plain itself (see tokens). A "[" that no "]" closes is a plain "[".
func glob(pattern, name string) bool {
	toks := tokens([]rune(pattern))
	s := []rune(name)
	// Every token but "*" takes one character, so on a mismatch it's enough
	// to give the last "*" one more character and go on from there.
	t, i, star, starAt := 0, 0, -1, 0
	for i < len(s) {
		switch {
		case t < len(toks) && toks[t] == nil:
			star, starAt = t, i
			t++
		case t < len(toks) && toks[t](s[i]):
			t, i = t+1, i+1
		case star >= 0:
			starAt++
			t, i = star+1, starAt
		default:
			return false
		}
	}
	for t < len(toks) && toks[t] == nil {
		t++
	}
	return t == len(toks)
}

// tokens returns, for each character a pattern takes, the test it puts on
// that character, with nil for a "*".
func tokens(p []rune) []func(rune) bool {
	var toks []func(rune) bool
	afterStar := false // the tokens so far end in "*" and "?", a "*" among them
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '*':
			toks = append(toks, nil)
			afterStar = true
			continue
		case c == '?':
			toks = append(toks, func(rune) bool { return true })
			continue
		case c == '\\' && i+1 == len(p):
			// A backslash ending the pattern matches a backslash, except
			// right after a "*", or after "*" and "?" together: the shell's
			// shortcut for "*" never tries it there.
			if afterStar {
				return append(toks, never)
			}
			toks = append(toks, is('\\'))
		case c == '\\':
			i++
			toks = append(toks, is(p[i]))
		case c == '[':
			in, n := bracket(p[i+1:])
			if n == 0 {
				in = is('[')
			}
			toks = append(toks, in)
			i += n
		default:
			toks = append(toks, is(c))
		}
		afterStar = false
	}
	return toks
}

func is(c rune) func(rune) bool { return func(r rune) bool { return r == c } }

func never(rune) bool { return false }

// bracket reads the set after a "[" and returns its test and the number of
// characters it takes, its closing "]" included, or 0 when no "]" closes it.
// A "!" or "^" first matches the characters outside the set. A "]" first,
// and a "-" first or last, are members; "a-z" is a range; a backslash makes
// the next character plain; "[:digit:]" and the other classes have their
// ASCII members, and an unknown class has none. Any other "[" is a member,
// so equivalence classes and collating symbols, "[=a=]" and "[.a.]", aren't
// supported. Bash versions differ on a "[" in a set that doesn't start a
// whole class, and on a set the pattern's end cuts off in a range or after
// a backslash; this matches bash 5.3.
func bracket(p []rune) (func(rune) bool, int) {
	i := 0
	negate := len(p) > 0 && (p[0] == '!' || p[0] == '^')
	if negate {
		i++
	}
	first := i
	var members []func(rune) bool
	for i < len(p) {
		c := p[i]
		i++
		switch {
		case c == ']' && i-1 > first:
			return func(r rune) bool {
				return slices.ContainsFunc(members, func(m func(rune) bool) bool { return m(r) }) != negate
			}, i
		case c == '[' && i < len(p) && p[i] == ':':
			if end := closing(p, i+1); end >= 0 {
				members = append(members, class(string(p[i+1:end])))
				i = end + 2
				continue
			}
		case c == '\\' && i < len(p):
			c = p[i]
			i++
		}
		if i+1 < len(p) && p[i] == '-' && p[i+1] != ']' {
			lo, hi := c, p[i+1]
			i += 2
			if hi == '\\' && i < len(p) {
				hi = p[i]
				i++
			}
			members = append(members, func(r rune) bool { return lo <= r && r <= hi })
			continue
		}
		members = append(members, is(c))
	}
	return nil, 0
}

// closing returns the index of the ":]" closing a class name that starts at
// from, or -1.
func closing(p []rune, from int) int {
	for j := from; j+1 < len(p); j++ {
		if p[j] == ':' && p[j+1] == ']' {
			return j
		}
	}
	return -1
}

// classes are the ASCII members of each character class but cntrl.
var classes = map[string]string{
	"alpha":  lower + upper,
	"digit":  digits,
	"alnum":  lower + upper + digits,
	"word":   lower + upper + digits + "_",
	"upper":  upper,
	"lower":  lower,
	"xdigit": digits + "abcdefABCDEF",
	"space":  space,
	"blank":  " \t",
	"punct":  punct,
	"print":  " " + lower + upper + digits + punct,
	"graph":  lower + upper + digits + punct,
}

const (
	lower  = "abcdefghijklmnopqrstuvwxyz"
	upper  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digits = "0123456789"
	punct  = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"
)

// class returns the test for the character class name.
func class(name string) func(rune) bool {
	if name == "cntrl" {
		return func(r rune) bool { return r < ' ' || r == 0x7f }
	}
	members, ok := classes[name]
	if !ok {
		return never
	}
	return func(r rune) bool { return strings.ContainsRune(members, r) }
}
