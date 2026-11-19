package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/buildinfo"
	"orchids-api/internal/testutil"
)

type fixtureSource struct {
	release Release
	files   map[string][]byte
	err     error
	calls   int
}

func (f *fixtureSource) Latest(context.Context) (Release, error) { f.calls++; return f.release, f.err }
func (f *fixtureSource) Download(_ context.Context, a Asset, w io.Writer, limit int64) error {
	b, ok := f.files[a.Name]
	if !ok {
		return errors.New("missing asset")
	}
	if int64(len(b)) > limit {
		return errors.New("too large")
	}
	_, e := w.Write(b)
	return e
}
func fixture(t *testing.T, binary []byte) *Manager {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "server")
	testutil.NoError(t, os.WriteFile(exe, []byte("old binary"), 0755))
	h := sha256.Sum256(binary)
	name := "orchids-server-linux-amd64"
	f := &fixtureSource{release: Release{Tag: "v1.0.3", Assets: []Asset{{Name: name, URL: "binary"}, {Name: name + ".sha256", URL: "checksum"}, {Name: name + ".build-info.txt", URL: "metadata"}}}, files: map[string][]byte{name: binary, name + ".sha256": []byte(hex.EncodeToString(h[:]) + "  " + name), name + ".build-info.txt": []byte("version=v1.0.3\ncommit=abcdef0\ngoos=linux\ngoarch=amd64\n")}}
	return &Manager{Info: buildinfo.Info{Version: "v1.0.2", OS: "linux", Arch: "amd64", Commit: "oldcommit"}, Dir: dir, Executable: exe, Source: f, launch: func(string, string, string, string, string, string) error { return nil }, rename: os.Rename}
}
func TestSemVer(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{{"v1.0.3", "1.0.2", 1}, {"1.0.2+local", "v1.0.2", 0}, {"1.0.0-alpha.2", "1.0.0-alpha.10", -1}, {"1.0.0-beta", "1.0.0", -1}, {"1.0.0-2", "1.0.0-alpha", -1}, {"1.0.0-alpha", "1.0.0-alpha.1", -1}, {"999999999999999999999.0.0", "2.0.0", 1}} {
		c, ok := Compare(tt.a, tt.b)
		testutil.Falsef(t, !ok || c != tt.want, "Compare(%s,%s)=%d/%t", tt.a, tt.b, c, ok)
	}
	for _, v := range []string{"dev", "v01.0.0", "1.0", "1.0.0-01", "1.0.0+"} {
		_, ok := Compare(v, "1.0.0")
		testutil.Falsef(t, ok, "accepted %s", v)
	}
}
func TestCheckCacheAndUnknown(t *testing.T) {
	m := fixture(t, nil)
	f := m.Source.(*fixtureSource)
	now := time.Now().UTC()
	m.now = func() time.Time { return now }
	a := m.Check(context.Background(), false)
	testutil.Falsef(t, !a.Available || !a.HasUpdate, "%+v", a)
	m.Check(context.Background(), false)
	testutil.Equal(t, f.calls, 1)
	f.err = errors.New("offline")
	now = now.Add(time.Minute)
	a = m.Check(context.Background(), true)
	testutil.False(t, a.Warning == "" || !a.Available, "valid stale cache not identified")
	m.checkedAt = time.Now().Add(-21 * time.Minute)
	a = m.Check(context.Background(), true)
	testutil.False(t, a.Available || a.Warning == "", "failed check presented as latest")
	f.err = nil
	now = now.Add(2 * time.Minute)
	m.Info.Version = "dev"
	a = m.Check(context.Background(), true)
	testutil.False(t, a.Warning == "" || a.HasUpdate, "unknown source version guessed")
}
func TestAssetAndChecksumContracts(t *testing.T) {
	_, _, e := selectAssets(Release{}, "linux", "amd64")
	testutil.Error(t, e)
	for _, value := range []string{"bad", "abcd  orchids-server-linux-amd64", ""} {
		_, e := checksumDigest(value, "orchids-server-linux-amd64")
		testutil.Error(t, e)
	}
	_, e = checksumDigest(strings.Repeat("a", 64)+" *orchids-server-linux-amd64", "orchids-server-linux-amd64")
	testutil.NoError(t, e)
}
func TestIntegrityFailurePreservesExecutable(t *testing.T) {
	m := fixture(t, []byte("bad new binary"))
	f := m.Source.(*fixtureSource)
	f.files["orchids-server-linux-amd64.sha256"] = []byte(strings.Repeat("0", 64) + "  orchids-server-linux-amd64")
	op := Operation{ID: "test", Target: buildinfo.Info{Version: "v1.0.3"}}
	e := m.perform(context.Background(), &op)
	testutil.Falsef(t, e == nil || !strings.Contains(e.Error(), "SHA-256"), "%v", e)
	b, _ := os.ReadFile(m.Executable)
	testutil.Equal(t, string(b), "old binary")
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTrustedDownloadsAndLimits(t *testing.T) {
	for _, raw := range []string{"http://github.com/file", "https://evil.example/file", "https://github.com:444/file", "https://user@github.com/file"} {
		testutil.Falsef(t, trustedURL(raw), "trusted %s", raw)
	}
	g := NewGitHub("zhangdailin/API-Console")
	g.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("12345")), Header: make(http.Header), ContentLength: -1, Request: r}, nil
	})
	var output bytes.Buffer
	asset := Asset{URL: "https://github.com/zhangdailin/API-Console/releases/download/v1.0.3/server"}
	e := g.Download(context.Background(), asset, &output, 4)
	testutil.Error(t, e)
	asset.URL = "https://github.com/other/repo/releases/download/v1.0.3/server"
	e = g.Download(context.Background(), asset, &output, 10)
	testutil.Error(t, e)
	e = g.Client.CheckRedirect(&http.Request{URL: mustURL(t, "https://evil.example/payload")}, nil)
	testutil.Error(t, e)
}
func TestActionHTTPGuards(t *testing.T) {
	m := fixture(t, nil)
	m.Reason = "unsupported"
	for _, tt := range []struct {
		method, origin, content string
		want                    int
	}{{"GET", "", "", 405}, {"POST", "https://evil.example", "application/json", 403}, {"POST", "", "text/plain", 415}, {"POST", "", "application/json", 409}} {
		r := httptest.NewRequest(tt.method, "https://console.example/api/system/update", strings.NewReader(`{"version":"v1.0.3"}`))
		r.Header.Set("Origin", tt.origin)
		r.Header.Set("Content-Type", tt.content)
		w := httptest.NewRecorder()
		m.HandleAction("update")(w, r)
		testutil.Equal(t, w.Code, tt.want)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, e := url.Parse(raw)
	testutil.NoError(t, e)
	return u
}
