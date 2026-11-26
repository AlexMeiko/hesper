package driver

import (
	"strconv"
	"testing"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestLookupAndDecodeErrorText(t *testing.T) {
	for _, okName := range []string{"GBK", "GB2312", "EUC_CN", "UTF8", "WIN1252", "ISO_8859_5", "LATIN9", "KOI8R"} {
		if LookupCharset(okName) == nil {
			t.Fatalf("LookupCharset(%q) = nil", okName)
		}
	}
	for _, badName := range []string{"", "SQL_ASCII", "GKB", "replacement"} {
		if LookupCharset(badName) != nil {
			t.Fatalf("LookupCharset(%q) != nil", badName)
		}
	}

	utf8Msg := `用户 "postgres" 密码认证失败`
	gbkBytes, _ := simplifiedchinese.GBK.NewEncoder().String(utf8Msg)
	win1252Msg := "rôle « café »"
	win1252Bytes, _ := charmap.Windows1252.NewEncoder().String(win1252Msg)
	cyrillicMsg := "ошибка"
	iso5Bytes, _ := charmap.ISO8859_5.NewEncoder().String(cyrillicMsg)
	corrupt := "\xff\xfe\x81"

	cases := []struct {
		name, raw, reported, fallback, want string
	}{
		{"empty", "", "", "GBK", ""},
		{"fallback_gbk", gbkBytes, "", "GBK", utf8Msg},
		{"fallback_gb2312", gbkBytes, "", "GB2312", utf8Msg},
		{"fallback_preserves_utf8", utf8Msg, "", "GBK", utf8Msg},
		{"fallback_win1252", win1252Bytes, "", "Windows-1252", win1252Msg},
		{"authoritative_win1252", "\xc3\xa9", "WIN1252", "", "Ã©"},
		{"authoritative_iso_8859_5", iso5Bytes, "ISO_8859_5", "", cyrillicMsg},
		{"utf8_rescues_leaked_gbk", gbkBytes, "UTF8", "GBK", utf8Msg},
		{"literal_fffd", "bad \uFFFD", "UTF8", "GBK", "bad \uFFFD"},
		{"no_fallback_quotes", gbkBytes, "", "", strconv.Quote(gbkBytes)},
		{"corrupt_quotes", corrupt, "", "GBK", strconv.Quote(corrupt)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecodeErrorText(tc.raw, tc.reported, tc.fallback); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
