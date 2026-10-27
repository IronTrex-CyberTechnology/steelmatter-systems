package contract

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	// MaxPayloadBytes is the 1 MiB payload limit (document 02).
	MaxPayloadBytes = 1 << 20
	// MaxDepth bounds nesting; payloads need at most 3 levels.
	MaxDepth = 32
	// MaxSafeInteger is 2^53-1, the largest integer exact in both runtimes.
	MaxSafeInteger int64 = 1<<53 - 1
)

// ParseStrict parses UTF-8 JSON (RFC 8259) under the Steelmatter input profile:
// no byte-order mark, no invalid UTF-8 or lone surrogates, no duplicate object
// keys, integers only (no fraction, exponent or negative zero) within
// ±(2^53-1), at most MaxDepth nesting, no trailing content.
//
// Values are map[string]any, []any, string, int64, bool or nil. The first
// fault in document order determines the error code.
func ParseStrict(data []byte) (any, *Error) {
	if len(data) > MaxPayloadBytes {
		return nil, errf(CodePayloadTooLarge, "", "payload is %d bytes; limit %d", len(data), MaxPayloadBytes)
	}
	if !utf8.Valid(data) {
		return nil, errf(CodeInvalidEncoding, "", "input is not valid UTF-8")
	}
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return nil, errf(CodeInvalidEncoding, "", "byte-order mark is not allowed")
	}
	p := &parser{data: data}
	p.skipWS()
	v, err := p.value(0, "")
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos != len(p.data) {
		return nil, p.syntax("", "unexpected content after the top-level value")
	}
	return v, nil
}

type parser struct {
	data []byte
	pos  int
}

func (p *parser) syntax(path, msg string) *Error {
	return errf(CodeInvalidJSON, path, "%s (offset %d)", msg, p.pos)
}

func (p *parser) skipWS() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value(depth int, path string) (any, *Error) {
	if depth > MaxDepth {
		return nil, p.syntax(path, "nesting exceeds maximum depth")
	}
	if p.pos >= len(p.data) {
		return nil, p.syntax(path, "unexpected end of input")
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.object(depth, path)
	case c == '[':
		return p.array(depth, path)
	case c == '"':
		return p.str(path)
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number(path)
	case p.literal("true"):
		return true, nil
	case p.literal("false"):
		return false, nil
	case p.literal("null"):
		return nil, nil
	default:
		return nil, p.syntax(path, "unexpected character")
	}
}

func (p *parser) literal(word string) bool {
	if strings.HasPrefix(string(p.data[p.pos:min(len(p.data), p.pos+len(word))]), word) {
		p.pos += len(word)
		return true
	}
	return false
}

func (p *parser) object(depth int, path string) (any, *Error) {
	p.pos++ // {
	obj := map[string]any{}
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return obj, nil
	}
	for {
		p.skipWS()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, p.syntax(path, "expected object key")
		}
		key, err := p.str(path)
		if err != nil {
			return nil, err
		}
		childPath := path + "/" + escapePointer(key)
		if _, dup := obj[key]; dup {
			return nil, errf(CodeDuplicateKey, childPath, "duplicate object key %q", key)
		}
		p.skipWS()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, p.syntax(childPath, "expected ':'")
		}
		p.pos++
		p.skipWS()
		v, err := p.value(depth+1, childPath)
		if err != nil {
			return nil, err
		}
		obj[key] = v
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, p.syntax(path, "unterminated object")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return obj, nil
		default:
			return nil, p.syntax(path, "expected ',' or '}'")
		}
	}
}

func (p *parser) array(depth int, path string) (any, *Error) {
	p.pos++ // [
	arr := []any{}
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return arr, nil
	}
	for i := 0; ; i++ {
		p.skipWS()
		v, err := p.value(depth+1, path+"/"+strconv.Itoa(i))
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, p.syntax(path, "unterminated array")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return arr, nil
		default:
			return nil, p.syntax(path, "expected ',' or ']'")
		}
	}
}

func (p *parser) str(path string) (string, *Error) {
	p.pos++ // opening quote
	var b strings.Builder
	for {
		if p.pos >= len(p.data) {
			return "", p.syntax(path, "unterminated string")
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return b.String(), nil
		case c < 0x20:
			return "", p.syntax(path, "unescaped control character in string")
		case c == '\\':
			p.pos++
			if p.pos >= len(p.data) {
				return "", p.syntax(path, "unterminated escape")
			}
			e := p.data[p.pos]
			p.pos++
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				r, err := p.hex4(path)
				if err != nil {
					return "", err
				}
				switch {
				case utf16.IsSurrogate(r) && r < 0xDC00:
					// High surrogate: must be followed by \u low surrogate.
					if p.pos+1 < len(p.data) && p.data[p.pos] == '\\' && p.data[p.pos+1] == 'u' {
						p.pos += 2
						lo, err := p.hex4(path)
						if err != nil {
							return "", err
						}
						if lo < 0xDC00 || lo > 0xDFFF {
							return "", errf(CodeInvalidEncoding, path, "unpaired surrogate escape")
						}
						b.WriteRune(utf16.DecodeRune(r, lo))
					} else {
						return "", errf(CodeInvalidEncoding, path, "unpaired surrogate escape")
					}
				case utf16.IsSurrogate(r):
					return "", errf(CodeInvalidEncoding, path, "unpaired surrogate escape")
				default:
					b.WriteRune(r)
				}
			default:
				return "", p.syntax(path, "invalid escape")
			}
		default:
			// Input is valid UTF-8 (checked up front); copy the whole rune.
			_, size := utf8.DecodeRune(p.data[p.pos:])
			b.Write(p.data[p.pos : p.pos+size])
			p.pos += size
		}
	}
}

func (p *parser) hex4(path string) (rune, *Error) {
	if p.pos+4 > len(p.data) {
		return 0, p.syntax(path, "short unicode escape")
	}
	// CodeQL go/incorrect-integer-conversion (alert #2) is a false positive here:
	// exactly four hex digits are parsed, so v is at most 0xFFFF and always fits
	// in a rune.
	v, err := strconv.ParseUint(string(p.data[p.pos:p.pos+4]), 16, 32)
	if err != nil {
		return 0, p.syntax(path, "invalid unicode escape")
	}
	p.pos += 4
	return rune(v), nil
}

// number accepts the full RFC 8259 grammar so that a float is reported as
// NON_INTEGER_NUMBER rather than a syntax error.
func (p *parser) number(path string) (any, *Error) {
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
	}
	if p.pos >= len(p.data) {
		return nil, p.syntax(path, "incomplete number")
	}
	switch c := p.data[p.pos]; {
	case c == '0':
		p.pos++
	case c >= '1' && c <= '9':
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	default:
		return nil, p.syntax(path, "invalid number")
	}
	intEnd := p.pos
	fractional := false
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		if p.pos >= len(p.data) || !isDigit(p.data[p.pos]) {
			return nil, p.syntax(path, "invalid fraction")
		}
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
		fractional = true
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if p.pos >= len(p.data) || !isDigit(p.data[p.pos]) {
			return nil, p.syntax(path, "invalid exponent")
		}
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
		fractional = true
	}
	lit := string(p.data[start:p.pos])
	if fractional {
		return nil, errf(CodeNonIntegerNumber, path, "number %s is not an integer", lit)
	}
	if lit == "-0" {
		return nil, errf(CodeNonIntegerNumber, path, "negative zero is not allowed")
	}
	digits := string(p.data[start:intEnd])
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n > MaxSafeInteger || n < -MaxSafeInteger {
		return nil, errf(CodeNumberOutOfRange, path, "integer %s exceeds ±(2^53-1)", lit)
	}
	return n, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
