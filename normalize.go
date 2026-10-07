// vim: set ts=4 sw=4 noet:
package main

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// The same functions are applied when importing the census and when reading a
// request, so a client may send either the reduced value ("5678A", "AL") or
// the full one ("12345678-A", "Álvarez") and both will match.

// documentKey reduces an identity document to the characters that are stored.
func (cfg Config) documentKey(doc string) string {
	return reduceDocument(normalizeDocument(doc), cfg.DocumentChars, cfg.FirstChars, cfg.FirstChars && cfg.FirstCharsAddLetter)
}

// normalizeDocument uppercases and keeps only letters and digits.
func normalizeDocument(doc string) string {
	return strings.Map(func(r rune) rune {
		r = unicode.ToUpper(r)
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, doc)
}

// reduceDocument keeps the n characters of a normalized document that are
// indexed: the last ones, the first ones, or the first ones plus the letter.
func reduceDocument(doc string, n int, first, addLetter bool) string {
	switch {
	case n == 0:
		return doc
	case first && addLetter:
		if len(doc) <= n+1 {
			return doc
		}
		return doc[:n] + doc[len(doc)-1:]
	case first:
		if len(doc) <= n {
			return doc
		}
		return doc[:n]
	default:
		if len(doc) <= n {
			return doc
		}
		return doc[len(doc)-n:]
	}
}

// nameKey uppercases, removes diacritics (À->A, Ç->C, Ñ->N) and truncates.
func (cfg Config) nameKey(name string) string {
	return truncateUTF8String(normalizeName(name), cfg.NameChars)
}

// normalizeName uppercases and removes diacritics, without truncating.
func normalizeName(name string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	s, _, err := transform.String(t, strings.TrimSpace(name))
	if err != nil {
		s = name
	}
	return strings.ToUpper(s)
}

// dayKey returns the day of month with two digits ("5" -> "05").
func dayKey(day string) string {
	day = strings.TrimSpace(day)
	if len(day) == 1 {
		return "0" + day
	}
	return day
}

// yearKey returns the last two digits of the year ("1991" -> "91").
func yearKey(year string) string {
	year = strings.TrimSpace(year)
	if len(year) > 2 {
		return year[len(year)-2:]
	}
	return year
}

// birthdateKeys extracts day and two digit year from FNAC, which the INE
// exports with the day first and the year last (DD/MM/YYYY or DDMMYYYY).
func birthdateKeys(fnac string) (day, year string, ok bool) {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, fnac)
	if len(digits) < 4 {
		return "", "", false
	}
	return digits[:2], digits[len(digits)-2:], true
}

var latin1 = charmap.ISO8859_1.NewDecoder()

// decodeField converts an ISO-8859-1 field from the INE export to UTF-8.
func decodeField(s string) string {
	if d, err := latin1.String(s); err == nil {
		s = d
	}
	return strings.TrimSpace(s)
}

func truncateUTF8String(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
