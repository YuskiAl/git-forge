// Hand-written JSON helpers: Node's JSON.stringify field order and escaping,
// without encoding/json reflection on the hot paths. Escaping matches
// JSON.stringify: '"' and '\' escaped, control chars < 0x20 as \n \r \t \b \f
// or \u00XX, everything else (including < > & and non-ASCII) passed through
// as UTF-8 bytes.
package main

import (
	"strconv"
	"strings"
)

// appendJSONString appends s as a JSON string literal.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				dst = append(dst, '\\', 'u', '0', '0')
				dst = append(dst, hexDigit(c>>4), hexDigit(c&0xf))
			}
			start = i + 1
		}
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

func hexDigit(v byte) byte {
	if v < 10 {
		return '0' + v
	}
	return 'a' + v - 10
}

func appendJSONInt(dst []byte, v int64) []byte {
	return strconv.AppendInt(dst, v, 10)
}

func appendJSONBool(dst []byte, v bool) []byte {
	if v {
		return append(dst, "true"...)
	}
	return append(dst, "false"...)
}

func appendJSONNull(dst []byte) []byte { return append(dst, "null"... ) }

// jsonString returns the JSON literal for s.
func jsonString(s string) string {
	return string(appendJSONString(nil, s))
}

// parseJSInt parses a leading decimal integer like JavaScript parseInt:
// optional sign, digits, stops at the first non-digit. Returns (value, ok).
func parseJSInt(s string) (int, bool) {
	i := 0
	neg := false
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		neg = s[i] == '-'
		i++
	}
	start := i
	v := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		v = v*10 + int(s[i]-'0')
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		v = -v
	}
	return v, true
}

// pageParams mirrors http.ts pageParams.
func pageParams(query map[string][]string) (page, perPage int) {
	get := func(k string) string {
		if vs, ok := query[k]; ok && len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	page = 1
	if v, ok := parseJSInt(get("page")); ok && v != 0 {
		page = v
	} else if s := get("page"); s != "" {
		_ = s
	}
	if page < 1 {
		page = 1
	}
	perPage = 30
	if v, ok := parseJSInt(get("per_page")); ok && v != 0 {
		perPage = v
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	return page, perPage
}

// queryEscape mirrors URLSearchParams percent-encoding for link headers
// (space -> +, alphanumerics and -_.~ kept, everything else %XX uppercase).
func queryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	return b.String()
}

// linkHeader mirrors http.ts linkHeader: ordered params, next then prev.
func linkHeader(host, basePath string, query [][2]string, page, perPage int, hasNext, hasPrev bool) string {
	build := func(p int) string {
		var sb strings.Builder
		sb.WriteString("<http://")
		sb.WriteString(host)
		sb.WriteString(basePath)
		sb.WriteByte('?')
		first := true
		write := func(k, v string) {
			if !first {
				sb.WriteByte('&')
			}
			first = false
			sb.WriteString(queryEscape(k))
			sb.WriteByte('=')
			sb.WriteString(queryEscape(v))
		}
		for _, kv := range query {
			write(kv[0], kv[1])
		}
		write("page", strconv.Itoa(p))
		write("per_page", strconv.Itoa(perPage))
		sb.WriteString(">")
		return sb.String()
	}
	var links []string
	if hasNext {
		links = append(links, build(page+1)+`; rel="next"`)
	}
	if hasPrev {
		links = append(links, build(page-1)+`; rel="prev"`)
	}
	if len(links) == 0 {
		return ""
	}
	return strings.Join(links, ", ")
}
