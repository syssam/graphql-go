// Package jsonr decodes the one JSON shape the engine reads on every request:
// the variables object.
//
// encoding/json decodes into map[string]any through reflection, one
// allocation per value and a dozen for the decoder itself, which made two
// scalar variables cost a microsecond -- two fifths of a small request. This
// package decodes the same input into the same values without reflection.
//
// "The same values" is the contract, and it is held by test rather than by
// care: FuzzObject and TestObjectAgreesWithEncodingJSON decode every input
// with both and require them to agree, on what is accepted and on what comes
// out. The types are encoding/json's with UseNumber: map[string]any, []any,
// string, json.Number, bool and nil.
package jsonr

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxDepth is encoding/json's own bound on nesting. The decoder recurses, so
// an unbounded input would otherwise pick the depth of its stack.
const maxDepth = 10000

// shareLimit is the largest input whose strings are substrings of the one copy
// taken of it. Sharing is what makes a small object cheap, and it means any
// one value kept past the request keeps that whole copy reachable: an id put
// in a cache, from variables that also held an upload, pinned the upload.
// Past the limit every string is its own allocation, as encoding/json made
// them, so what a kept value can pin is at most this and never what a client
// chose to send.
const shareLimit = 1 << 10

// Object decodes data as a JSON object. JSON null, and input holding nothing
// but whitespace, decode to a nil map; any other value is an error.
//
// Nothing in the result aliases data, which a transport may reuse as soon as
// the request returns: strings and numbers are substrings of one copy of it
// taken here, or their own allocations once it is longer than shareLimit.
func Object(data []byte) (map[string]any, error) {
	d := decoder{s: string(data), own: len(data) > shareLimit}
	d.space()
	if d.i == len(d.s) {
		return nil, nil
	}
	var m map[string]any
	switch d.s[d.i] {
	case '{':
		var err error
		if m, err = d.object(); err != nil {
			return nil, err
		}
	case 'n':
		if err := d.literal("null"); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("expected an object, found %s", d.found())
	}
	d.space()
	if d.i != len(d.s) {
		return nil, fmt.Errorf("unexpected %s after the value", d.found())
	}
	return m, nil
}

type decoder struct {
	s     string
	i     int
	depth int
	// own makes each string and number an allocation of its own instead of a
	// substring of s.
	own bool
}

// keep is a substring of the input as the result holds it.
func (d *decoder) keep(s string) string {
	if d.own {
		return strings.Clone(s)
	}
	return s
}

var errEOF = errors.New("unexpected end of JSON input")

// found describes what is at the cursor, for an error message.
func (d *decoder) found() string {
	if d.i >= len(d.s) {
		return "the end of the input"
	}
	return fmt.Sprintf("%q at offset %d", d.s[d.i], d.i)
}

func (d *decoder) space() {
	for d.i < len(d.s) {
		switch d.s[d.i] {
		case ' ', '\t', '\n', '\r':
			d.i++
		default:
			return
		}
	}
}

func (d *decoder) value() (any, error) {
	if d.i >= len(d.s) {
		return nil, errEOF
	}
	switch c := d.s[d.i]; {
	case c == '"':
		s, err := d.str()
		if err != nil {
			return nil, err
		}
		return s, nil
	case c == '{':
		m, err := d.object()
		if err != nil {
			return nil, err
		}
		return m, nil
	case c == '[':
		a, err := d.array()
		if err != nil {
			return nil, err
		}
		return a, nil
	case c == '-' || (c >= '0' && c <= '9'):
		n, err := d.number()
		if err != nil {
			return nil, err
		}
		return n, nil
	case c == 't':
		return true, d.literal("true")
	case c == 'f':
		return false, d.literal("false")
	case c == 'n':
		return nil, d.literal("null")
	}
	return nil, fmt.Errorf("invalid character %s looking for a value", d.found())
}

func (d *decoder) literal(word string) error {
	if len(d.s)-d.i < len(word) {
		// A prefix of the word is input that ended early; anything else is a
		// character that does not belong.
		if rest := d.s[d.i:]; rest == word[:len(rest)] {
			return errEOF
		}
	} else if d.s[d.i:d.i+len(word)] == word {
		d.i += len(word)
		return nil
	}
	return fmt.Errorf("invalid literal at offset %d, expected %s", d.i, word)
}

func (d *decoder) enter() error {
	if d.depth++; d.depth > maxDepth {
		return fmt.Errorf("exceeded the maximum nesting depth of %d", maxDepth)
	}
	return nil
}

func (d *decoder) object() (map[string]any, error) {
	if err := d.enter(); err != nil {
		return nil, err
	}
	defer func() { d.depth-- }()
	d.i++ // '{'
	m := map[string]any{}
	d.space()
	if d.i < len(d.s) && d.s[d.i] == '}' {
		d.i++
		return m, nil
	}
	for {
		if d.i >= len(d.s) {
			return nil, errEOF
		}
		if d.s[d.i] != '"' {
			return nil, fmt.Errorf("invalid character %s looking for an object key", d.found())
		}
		key, err := d.str()
		if err != nil {
			return nil, err
		}
		d.space()
		if d.i >= len(d.s) {
			return nil, errEOF
		}
		if d.s[d.i] != ':' {
			return nil, fmt.Errorf("invalid character %s after an object key", d.found())
		}
		d.i++
		d.space()
		v, err := d.value()
		if err != nil {
			return nil, err
		}
		// A repeated key keeps its last value, as encoding/json does.
		m[key] = v
		d.space()
		if d.i >= len(d.s) {
			return nil, errEOF
		}
		switch d.s[d.i] {
		case ',':
			d.i++
			d.space()
		case '}':
			d.i++
			return m, nil
		default:
			return nil, fmt.Errorf("invalid character %s after an object value", d.found())
		}
	}
}

func (d *decoder) array() ([]any, error) {
	if err := d.enter(); err != nil {
		return nil, err
	}
	defer func() { d.depth-- }()
	d.i++ // '['
	// Empty and not nil: [] is a list with nothing in it, which is how
	// encoding/json decodes it and what tells it apart from null.
	a := []any{}
	d.space()
	if d.i < len(d.s) && d.s[d.i] == ']' {
		d.i++
		return a, nil
	}
	for {
		v, err := d.value()
		if err != nil {
			return nil, err
		}
		a = append(a, v)
		d.space()
		if d.i >= len(d.s) {
			return nil, errEOF
		}
		switch d.s[d.i] {
		case ',':
			d.i++
			d.space()
		case ']':
			d.i++
			return a, nil
		default:
			return nil, fmt.Errorf("invalid character %s after an array element", d.found())
		}
	}
}

// number reads a JSON number and keeps its text, so no precision is decided
// here: -? (0 | [1-9][0-9]*) (\.[0-9]+)? ([eE][+-]?[0-9]+)?
func (d *decoder) number() (json.Number, error) {
	start := d.i
	if d.s[d.i] == '-' {
		d.i++
	}
	switch {
	case d.i >= len(d.s):
		return "", errEOF
	case d.s[d.i] == '0':
		d.i++
	case d.s[d.i] >= '1' && d.s[d.i] <= '9':
		d.digits()
	default:
		return "", fmt.Errorf("invalid character %s in a number", d.found())
	}
	if d.i < len(d.s) && d.s[d.i] == '.' {
		d.i++
		if err := d.needDigits(); err != nil {
			return "", err
		}
	}
	if d.i < len(d.s) && (d.s[d.i] == 'e' || d.s[d.i] == 'E') {
		d.i++
		if d.i < len(d.s) && (d.s[d.i] == '+' || d.s[d.i] == '-') {
			d.i++
		}
		if err := d.needDigits(); err != nil {
			return "", err
		}
	}
	return json.Number(d.keep(d.s[start:d.i])), nil
}

func (d *decoder) digits() {
	for d.i < len(d.s) && d.s[d.i] >= '0' && d.s[d.i] <= '9' {
		d.i++
	}
}

func (d *decoder) needDigits() error {
	if d.i >= len(d.s) {
		return errEOF
	}
	if d.s[d.i] < '0' || d.s[d.i] > '9' {
		return fmt.Errorf("invalid character %s in a number", d.found())
	}
	d.digits()
	return nil
}

// str reads a string. One with no escape and nothing but valid UTF-8, which
// is nearly every string, is returned as a substring of the input.
func (d *decoder) str() (string, error) {
	d.i++ // opening quote
	start := d.i
	ascii := true
	for d.i < len(d.s) {
		switch c := d.s[d.i]; {
		case c == '"':
			s := d.s[start:d.i]
			d.i++
			if ascii || utf8.ValidString(s) {
				return d.keep(s), nil
			}
			return repair(s), nil
		case c == '\\':
			return d.escaped(start)
		case c < 0x20:
			return "", fmt.Errorf("invalid control character %s in a string", d.found())
		case c >= utf8.RuneSelf:
			ascii = false
		}
		d.i++
	}
	return "", errEOF
}

// repair replaces each byte that is not part of a valid encoding with U+FFFD,
// one for one, which is what encoding/json does rather than refuse the input.
func repair(s string) string {
	b := make([]byte, 0, len(s)+utf8.UTFMax)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		b = utf8.AppendRune(b, r)
		i += size
	}
	return string(b)
}

// escaped finishes a string that holds a backslash, building it rather than
// slicing it. The cursor is at the first backslash.
func (d *decoder) escaped(start int) (string, error) {
	b := make([]byte, 0, len(d.s[start:d.i])+16)
	b = append(b, repair(d.s[start:d.i])...)
	for d.i < len(d.s) {
		c := d.s[d.i]
		switch {
		case c == '"':
			d.i++
			return string(b), nil
		case c < 0x20:
			return "", fmt.Errorf("invalid control character %s in a string", d.found())
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(d.s[d.i:])
			b = utf8.AppendRune(b, r)
			d.i += size
			continue
		case c != '\\':
			b = append(b, c)
			d.i++
			continue
		}
		if d.i+1 >= len(d.s) {
			return "", errEOF
		}
		switch e := d.s[d.i+1]; e {
		case '"', '\\', '/':
			b = append(b, e)
		case 'b':
			b = append(b, '\b')
		case 'f':
			b = append(b, '\f')
		case 'n':
			b = append(b, '\n')
		case 'r':
			b = append(b, '\r')
		case 't':
			b = append(b, '\t')
		case 'u':
			r, err := d.hex4(d.i + 2)
			if err != nil {
				return "", err
			}
			d.i += 6
			// A surrogate is half of a pair. With its other half it is one
			// code point; alone it is U+FFFD, and whatever follows it is read
			// on its own.
			if utf16.IsSurrogate(r) {
				if d.i+1 < len(d.s) && d.s[d.i] == '\\' && d.s[d.i+1] == 'u' {
					if r2, err := d.hex4(d.i + 2); err == nil {
						if pair := utf16.DecodeRune(r, r2); pair != utf8.RuneError {
							b = utf8.AppendRune(b, pair)
							d.i += 6
							continue
						}
					}
				}
				r = utf8.RuneError
			}
			b = utf8.AppendRune(b, r)
			continue
		default:
			return "", fmt.Errorf("invalid escape %q at offset %d", e, d.i)
		}
		d.i += 2
	}
	return "", errEOF
}

// hex4 reads the four hex digits of a \u escape starting at offset at.
func (d *decoder) hex4(at int) (rune, error) {
	if at+4 > len(d.s) {
		return 0, errEOF
	}
	var r rune
	for _, c := range []byte(d.s[at : at+4]) {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c -= 'a' - 10
		case c >= 'A' && c <= 'F':
			c -= 'A' - 10
		default:
			return 0, fmt.Errorf("invalid hex digit in a \\u escape at offset %d", at)
		}
		r = r<<4 | rune(c)
	}
	return r, nil
}
