package main

import (
	"strconv"
	"unicode/utf8"
)

const hexd = "0123456789abcdef"

func appendStr(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', hexd[c>>4], hexd[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, s[start:i]...)
			b = append(b, `�`...)
			i += size
			start = i
			continue
		}
		i += size
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

func appendPost(b []byte, p post) []byte {
	b = append(b, `{"id":`...)
	b = strconv.AppendInt(b, p.ID, 10)
	b = append(b, `,"body":`...)
	b = appendStr(b, p.Body)
	b = append(b, `,"created_at":`...)
	b = appendStr(b, p.CreatedAt)
	b = append(b, `,"author":`...)
	b = appendStr(b, p.Author)
	b = append(b, `,"like_count":`...)
	b = strconv.AppendInt(b, p.LikeCount, 10)
	return append(b, '}')
}
