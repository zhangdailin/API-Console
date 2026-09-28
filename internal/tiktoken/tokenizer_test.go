package tiktoken

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEstimateTextTokens(t *testing.T) {
	tests := []struct {
		name string
		text string
		min  int
		max  int
	}{
		{
			name: "Pure English",
			text: "This is a test sentence in English.",
			min:  8,
			max:  12,
		},
		{
			name: "Pure Chinese",
			text: "这是一个测试句子。",
			min:  10,
			max:  15,
		},
		{
			name: "Mixed",
			text: "This is a test 这是测试",
			min:  10,
			max:  16,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := EstimateTextTokens(tt.text)
			if tokens < tt.min || tokens > tt.max {
				t.Errorf("EstimateTextTokens(%q) = %d, want between %d and %d", tt.text, tokens, tt.min, tt.max)
			}
		})
	}
}

func TestEstimatorMatchesEstimateTextTokens(t *testing.T) {
	tests := []struct {
		name   string
		parts  []string
		joined string
	}{
		{
			name:   "english split mid word",
			parts:  []string{"hel", "lo ", "wor", "ld!"},
			joined: "hello world!",
		},
		{
			name:   "mixed ascii and cjk",
			parts:  []string{"This ", "is 测", "试 12", "3!"},
			joined: "This is 测试 123!",
		},
		{
			name:   "spaces punctuation and numbers",
			parts:  []string{"foo", "-bar", " 42", ",baz"},
			joined: "foo-bar 42,baz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var estimator Estimator
			for _, part := range tt.parts {
				estimator.Add(part)
			}
			joined := tt.joined
			if joined == "" {
				joined = strings.Join(tt.parts, "")
			}
			want := EstimateTextTokens(joined)
			if got := estimator.Count(); got != want {
				t.Fatalf("count=%d want=%d joined=%q", got, want, joined)
			}
			estimator.Reset()
			if estimator.Count() != 0 {
				t.Fatal("expected reset estimator")
			}
		})
	}
}

func TestEstimatorAddBytesMatchesEstimateTextTokens(t *testing.T) {
	tests := []string{
		`{"file_path":"/tmp/a.txt","content":"hello world"}`,
		`{"name":"Write","description":"update config and keep comments"}`,
		`{"description":"混合 UTF-8 内容","properties":{"path":{"type":"string"}}}`,
	}

	for _, text := range tests {
		var estimator Estimator
		estimator.AddBytes([]byte(text))
		if got, want := estimator.Count(), EstimateTextTokens(text); got != want {
			t.Fatalf("AddBytes count=%d want=%d text=%q", got, want, text)
		}
	}
}

// referenceEstimator is the original byte-at-a-time transcription of the
// estimator the wide (SWAR) scanner replaced. Every differential test below
// compares against it, so the fast path can never drift from the semantics the
// token accounting was written against.
type referenceEstimator struct {
	tokens float64
	inWord bool
}

func (e *referenceEstimator) addASCIIByte(b byte) {
	if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') {
		e.inWord = true
		return
	}
	if e.inWord {
		e.tokens++
		e.inWord = false
	}
	if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
		e.tokens++
	}
}

func (e *referenceEstimator) add(text string) {
	for i := 0; i < len(text); {
		b := text[i]
		if b < utf8.RuneSelf {
			e.addASCIIByte(b)
			i++
			continue
		}
		if e.inWord {
			e.tokens++
			e.inWord = false
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		e.tokens += 1.5
		i += size
	}
}

func (e *referenceEstimator) count() int {
	tokens := e.tokens
	if e.inWord {
		tokens++
	}
	return int(math.Round(tokens))
}

// TestEstimatorMatchesReference drives the table-driven scanner over the shapes
// the word-run fast path exists for — long runs, run boundaries landing on every
// alignment, and multibyte runes — and requires bit-for-bit agreement with the
// reference transcription, both accumulating and reset between calls.
func TestEstimatorMatchesReference(t *testing.T) {
	inputs := []string{
		"",
		"a",
		"abcdefgh",
		"abcdefghi",
		"abcdefghijklmnopqrstuvwxyz",
		"aaa bbb ccc ddd eee fff ggg hhh",
		"aaaaaaaaaaaaaaaaaaaaaaa",
		"aaaaaaaaaaaaaaaaaaaaaaa!",
		"!aaaaaaaaaaaaaaaaaaaaaaa",
		"aaaaaaaaaaaaaaaabbbbbbbbbbbbbbbbcccccccccccccccc",
		"https://example.com/a/b/c?d=e&f=g#hijklmnop",
		"ZVl0hR2kAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"混合UTF-8内容 alongside a very long asciiword run here",
		"中文中文中文中文中文中文中文中文中文中文中文中文中文中文",
		"a1B2c3D4e5F6g7H8",
		"tab\tseparated\tfields\there",
		"line\nbreaks\r\nand more",
		"punct!!!???,,,;;;:::[{{((",
		"           ",
		"\u2028\u2029\u00e9\u4e2d",
	}
	// Every alignment of a long run against the eight-byte window.
	for pad := 0; pad < 24; pad++ {
		inputs = append(inputs, strings.Repeat("x", pad)+strings.Repeat("word", 5)+strings.Repeat("y", pad))
	}

	for _, in := range inputs {
		var fast Estimator
		fast.Add(in)

		var ref referenceEstimator
		ref.add(in)

		if got, want := fast.Count(), ref.count(); got != want {
			t.Fatalf("Add(%q) = %d, reference = %d", in, got, want)
		}

		var fastBytes Estimator
		fastBytes.AddBytes([]byte(in))
		if got, want := fastBytes.Count(), ref.count(); got != want {
			t.Fatalf("AddBytes(%q) = %d, reference = %d", in, got, want)
		}
	}

	// Chunked accumulation must match one whole-string pass, which is how the
	// streaming path actually feeds deltas.
	chunks := []string{"pack", "et d", "eltas keep", " arriving", " 中文", " now"}
	var fast Estimator
	var ref referenceEstimator
	for _, chunk := range chunks {
		fast.Add(chunk)
		ref.add(chunk)
	}
	if got, want := fast.Count(), ref.count(); got != want {
		t.Fatalf("chunked Add = %d, reference = %d", got, want)
	}
}

func BenchmarkEstimateTextTokens_LongRun(b *testing.B) {
	text := strings.Repeat("x", 512)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = EstimateTextTokens(text)
	}
}

func BenchmarkEstimateTextTokens_Prose(b *testing.B) {
	text := strings.Repeat("the quick brown fox jumps over the lazy dog ", 12)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = EstimateTextTokens(text)
	}
}

func BenchmarkEstimateTextTokens_FinalBuilderFlow(b *testing.B) {
	parts := []string{"Write", `{"file_path":"/tmp/a.txt","content":"hello world"}`}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var builder strings.Builder
		for _, part := range parts {
			builder.WriteString(part)
		}
		_ = EstimateTextTokens(builder.String())
	}
}

func BenchmarkEstimateTextTokens_StreamingEstimator(b *testing.B) {
	parts := []string{"Write", `{"file_path":"/tmp/a.txt","content":"hello world"}`}
	var estimator Estimator
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		estimator.Reset()
		for _, part := range parts {
			estimator.Add(part)
		}
		_ = estimator.Count()
	}
}

func BenchmarkEstimateTextTokens_StreamingEstimatorBytes(b *testing.B) {
	parts := [][]byte{[]byte("Write"), []byte(`{"file_path":"/tmp/a.txt","content":"hello world"}`)}
	var estimator Estimator
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		estimator.Reset()
		for _, part := range parts {
			estimator.AddBytes(part)
		}
		_ = estimator.Count()
	}
}
