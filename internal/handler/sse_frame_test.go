package handler

import (
	"encoding/json"
	"math/rand"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

// The escaping fast path is a hand-written scanner, so it is only trustworthy if
// it is differentially tested against the encoder it is imitating. encoding/json
// with the default options (SetEscapeHTML on) is the reference: every case below
// demands byte-for-byte agreement with json.Marshal on a plain string.
func jsonReference(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(value)
	testutil.Falsef(t, err != nil, "json.Marshal(%q): %v", value, err)
	return string(raw)
}

func TestAppendJSONBytesMatchesEncodingJSON(t *testing.T) {
	cases := []string{
		"",
		"plain ascii",
		`quote " and backslash \`,
		"tab\tnewline\ncarriage\rbackspace\bformfeed\f",
		"nul\x00and\x01control\x1f",
		"html <script> & </script> is escaped",
		"CJK 中文内容 mixed with ascii",
		"emoji \U0001F600 and accents éüñ",
		"line separators \u2028 and \u2029",
		"del \x7f and high \u0080",
		"a\"b\\c<d>e&f",
		"\u00e9\u4e2d\u2028\u2029",
		"trailing backslash \\",
		"trailing quote \"",
		"1234567890abcdefghijklmnopqrstuvwxyz",
		"       ",
		strings.Repeat("x", 9),
		strings.Repeat("x", 8),
		strings.Repeat("x", 7),
		strings.Repeat("x", 16) + "<" + strings.Repeat("x", 16),
		strings.Repeat("x", 5) + "&" + strings.Repeat("y", 5) + "\"" + strings.Repeat("z", 5),
		strings.Repeat("a", 8) + "\u2028" + strings.Repeat("b", 8),
		strings.Repeat("中", 10) + "ascii" + strings.Repeat("中", 10),
		// Invalid UTF-8, which must defer to the stdlib rather than guess.
		"bad \xff\xfe bytes",
		"\x80",
		"ok\xc3",
	}
	// Every offset of a special byte inside consecutive eight-byte windows: a
	// wide check that skips a lane would escape the wrong thing here.
	for _, special := range []string{`"`, `\`, `<`, `>`, `&`, "\n", "\t", "\u2028"} {
		for pad := range 20 {
			cases = append(cases,
				strings.Repeat("a", pad)+special+strings.Repeat("b", 20),
				strings.Repeat("a", 20)+special+strings.Repeat("b", pad),
			)
		}
	}
	// Eight-byte windows of every byte value in a few alignments, including the
	// invalid-UTF-8 bytes that force the stdlib fallback.
	for b := range 256 {
		cases = append(cases, "ab"+string([]byte{byte(b)})+"cd")
	}

	for _, value := range cases {
		want := jsonReference(t, value)
		got, err := appendJSONBytes(make([]byte, 0, 64), value)
		testutil.Falsef(t, err != nil, "appendJSONBytes(%q): %v", value, err)
		testutil.Equal(t, string(got), want)

		// canAppendJSONRawString must agree with what the encoder actually did: it
		// answers true exactly when the reference output is the value itself
		// wrapped in quotes.
		raw := canAppendJSONRawString(value)
		wantRaw := want == `"`+value+`"`
		testutil.Equal(t, raw, wantRaw)
	}
}

func TestAppendJSONBytesMatchesEncodingJSONOnRandomInput(t *testing.T) {
	rng := rand.New(rand.NewSource(20240922))
	alphabet := []byte("abcXYZ019 <>&\"\\\n\t\r\b\f\x00\x1f\x7f\xc3\xa9\xe4\xb8\xad\xff\x80\xe2\x80\xa8\xe2\x80\xa9")
	for range 20000 {
		n := rng.Intn(48)
		buf := make([]byte, n)
		for j := range buf {
			buf[j] = alphabet[rng.Intn(len(alphabet))]
		}
		value := string(buf)

		want := jsonReference(t, value)
		got, err := appendJSONBytes(make([]byte, 0, 64), value)
		testutil.Falsef(t, err != nil, "appendJSONBytes(%q): %v", value, err)
		testutil.Equal(t, string(got), want)
		raw, wantRaw := canAppendJSONRawString(value), want == `"`+value+`"`
		testutil.Falsef(t, raw != wantRaw, "canAppendJSONRawString(%q) = %v, want %v", value, raw, wantRaw)
	}
}

// TestAppendJSONBytesAppendsToExistingPrefix guards the rolling-back of a
// partially written escape: the delta frame builders append into a shared
// scratch buffer, so a value that fails the scan midway must not leave bytes
// behind.
func TestAppendJSONBytesAppendsToExistingPrefix(t *testing.T) {
	prefix := []byte("PREFIX")
	for _, value := range []string{`ok`, "bad \xff", "escaped \" prefix then bad \xff", `quote "` + strings.Repeat("x", 20)} {
		dst := append([]byte(nil), prefix...)
		got, err := appendJSONBytes(dst, value)
		testutil.NoError(t, err)
		testutil.Falsef(t, !strings.HasPrefix(string(got), "PREFIX"), "prefix lost: %q", got)
		want := "PREFIX" + jsonReference(t, value)
		testutil.Falsef(t, string(got) != want, "got %q want %q", got, want)
	}
}

func BenchmarkAppendJSONBytesLongAscii(b *testing.B) {
	value := strings.Repeat("x", 512)
	dst := make([]byte, 0, 1024)
	b.SetBytes(int64(len(value)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst, _ = appendJSONBytes(dst[:0], value)
	}
}

func BenchmarkAppendJSONBytesCJKRuns(b *testing.B) {
	value := strings.Repeat("中文内容测试", 40)
	dst := make([]byte, 0, 4096)
	b.SetBytes(int64(len(value)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst, _ = appendJSONBytes(dst[:0], value)
	}
}
