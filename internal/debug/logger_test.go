package debug

import (
	"orchids-api/internal/testutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoggerLogInputTokenBreakdownWritesFile(t *testing.T) {
	wd, err := os.Getwd()
	testutil.NoError(t, err, "Getwd() error = %v")
	tmp := t.TempDir()
	err = os.Chdir(tmp)
	testutil.CheckNoError(t, err)
	defer func() {
		_ = os.Chdir(wd)
	}()

	logger := New(true, false)
	testutil.False(t, logger == nil || logger.dir == "", "expected enabled logger with directory")

	logger.LogInputTokenBreakdown("workbuddy", 101, 202, 303, 404, 1010)

	path := filepath.Join(logger.dir, "6_input_token_breakdown.json")
	raw, err := os.ReadFile(path)
	testutil.Falsef(t, err != nil, "ReadFile(%q) error = %v", path, err)

	content := string(raw)
	for _, want := range []string{
		`"prompt_profile": "workbuddy"`,
		`"base_prompt_tokens": 101`,
		`"system_context_tokens": 202`,
		`"history_tokens": 303`,
		`"tools_tokens": 404`,
		`"estimated_total": 1010`,
	} {
		testutil.MustContain(t, content, want)
	}
}

func TestLoggerLogUpstreamRequestRedactsCredentials(t *testing.T) {
	wd, err := os.Getwd()
	testutil.NoError(t, err, "Getwd() error = %v")
	tmp := t.TempDir()
	err = os.Chdir(tmp)
	testutil.CheckNoError(t, err)
	defer func() { _ = os.Chdir(wd) }()

	logger := New(true, false)
	logger.LogUpstreamRequest("https://example.test", map[string]string{
		"Authorization": "Bearer secret-jwt",
		"Cookie":        "session=secret-cookie",
		"Content-Type":  "application/json",
	}, map[string]any{"ok": true})

	path := filepath.Join(logger.dir, "3_upstream_request.json")
	raw, err := os.ReadFile(path)
	testutil.Falsef(t, err != nil, "ReadFile(%q) error = %v", path, err)
	content := string(raw)
	testutil.MustNotContainAny(t, content, "secret-jwt", "secret-cookie")
	testutil.MustContain(t, content, "[REDACTED]")
	info, err := os.Stat(path)
	testutil.Falsef(t, err != nil, "Stat(%q) error = %v", path, err)
	// Windows does not expose Unix permission bits through os.FileMode.
	got := info.Mode().Perm()
	testutil.Falsef(t, runtime.GOOS != "windows" && got != 0600, "debug log permissions=%o want 600", got)
}

func TestDebugStreamPreservesBeyondFormerLimit(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stream")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var written int64
	data := strings.Repeat("x", 9<<20)
	(&Logger{}).writeLimitedStream(f, &written, data)
	info, err := f.Stat()
	if err != nil || written != int64(len(data)) || info.Size() != int64(len(data)) {
		t.Fatal("debug stream truncated", written, err)
	}
}
