package driver

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/unicode"
)

var charsetAliases = map[string]string{
	"UTF8":   "UTF-8",
	"SJIS":   "Shift_JIS",
	"UHC":    "EUC-KR",
	"EUC_JP": "EUC-JP",
	"EUC_KR": "EUC-KR",
	"EUC_CN": "GBK",
	"LATIN7": "ISO-8859-13",
	"LATIN9": "ISO-8859-15",
	"KOI8R":  "KOI8-R",
	"KOI8U":  "KOI8-U",
	"WIN866": "CP866",
}

// LookupCharset resolves an IANA, WHATWG, or PostgreSQL charset name into an Encoding.
func LookupCharset(charset string) encoding.Encoding {
	label := strings.TrimSpace(charset)
	if label == "" {
		return nil
	}
	upper := strings.ToUpper(label)
	switch {
	case charsetAliases[upper] != "":
		label = charsetAliases[upper]
	case strings.HasPrefix(upper, "WIN125") || upper == "WIN874":
		label = "windows-" + upper[3:]
	case strings.HasPrefix(upper, "ISO_8859_"):
		label = "ISO-8859-" + upper[len("ISO_8859_"):]
	}
	if enc, _ := ianaindex.IANA.Encoding(label); enc != nil && enc != encoding.Replacement {
		return enc
	}
	if enc, _ := htmlindex.Get(label); enc != nil && enc != encoding.Replacement {
		return enc
	}
	return nil
}

func decodeLossless(raw string, enc encoding.Encoding) (string, bool) {
	if enc == nil {
		return "", false
	}
	if enc == unicode.UTF8 {
		return raw, utf8.ValidString(raw)
	}
	text, err := enc.NewDecoder().String(raw)
	if err != nil {
		return "", false
	}
	if !strings.ContainsRune(text, utf8.RuneError) {
		return text, true
	}
	orig, err := enc.NewEncoder().String(text)
	return text, err == nil && orig == raw
}

// DecodeErrorText converts server error bytes to UTF-8, quoting undecodable bytes.
// reportedCharset is authoritative (e.g. PG client_encoding); fallbackCharset is only
// applied when raw is not already valid UTF-8 so a fallback like GBK never mangles UTF-8.
func DecodeErrorText(raw, reportedCharset, fallbackCharset string) string {
	if raw == "" {
		return ""
	}
	if text, ok := decodeLossless(raw, LookupCharset(reportedCharset)); ok {
		return text
	}
	if utf8.ValidString(raw) {
		return raw
	}
	if text, ok := decodeLossless(raw, LookupCharset(fallbackCharset)); ok {
		return text
	}
	return strconv.Quote(raw)
}
