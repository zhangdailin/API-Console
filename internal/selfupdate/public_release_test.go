package selfupdate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

const testReleasePath = "/zhangdailin/API-Console/releases/"

func TestPublicReleaseCheckUsesPublishedAssetsWithoutREST(t *testing.T) {
	t.Setenv("ORCHIDS_UPDATE_GITHUB_TOKEN", "")
	g := NewGitHub("zhangdailin/API-Console")
	name := "orchids-server-linux-amd64"
	tag := "v1.0.3"
	download := testReleasePath + "download/" + tag + "/"
	for _, complete := range []bool{true, false} {
		t.Run(fmt.Sprint("complete=", complete), func(t *testing.T) {
			calls := 0
			g.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "github.com" || r.Header.Get("Authorization") != "" {
					t.Fatalf("unexpected request: %s", r.URL)
				}
				res := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: r}
				body := ""
				switch r.URL.Path {
				case testReleasePath + "latest":
					res.StatusCode = http.StatusFound
					res.Header.Set("Location", "https://github.com"+testReleasePath+"tag/"+tag)
				case testReleasePath + "expanded_assets/" + tag:
					body = fmt.Sprintf(`<a href="%[1]s%[2]s">binary</a><a href="%[1]s%[2]s.sha256">checksum</a>
						<a href="https://evil.example%[1]s%[2]s.build-info.txt">foreign metadata</a>
						<a href="%[1]s%[2]s.build-info.txt?untrusted=true">metadata with query</a>
						<a href="%[1]s../%[2]s.build-info.txt">traversal</a>
						<a href="/other/project/releases/download/v1.0.3/%[2]s.build-info.txt">other repo metadata</a>`, download, name)
					if complete {
						body += fmt.Sprintf(`<a href="%s%s.build-info.txt">metadata</a>`, download, name)
					}
				default:
					t.Fatalf("unexpected endpoint: %s", r.URL)
				}
				res.Body = io.NopCloser(strings.NewReader(body))
				return res, nil
			})
			release, err := g.Latest(context.Background())
			if err != nil || release.Tag != tag || calls != 2 {
				t.Fatalf("unexpected release check: %+v; requests=%d; error=%v", release, calls, err)
			}
			assets, err := selectAssets(release, "linux", "amd64")
			if complete {
				if err != nil || len(release.Assets) != 3 || assets.BuildInfo.URL != "https://github.com"+download+name+".build-info.txt" {
					t.Fatalf("did not find the three verified download links: %+v; %v", release.Assets, err)
				}
			} else if err == nil || len(release.Assets) != 2 {
				t.Fatalf("accepted missing build information: %+v; %v", release.Assets, err)
			}
		})
	}
}

func TestPublicReleaseRejectsRedirectOutsideStableRepositoryRelease(t *testing.T) {
	t.Setenv("ORCHIDS_UPDATE_GITHUB_TOKEN", "")
	for _, location := range []string{
		"https://example.test" + testReleasePath + "tag/v1.0.3",
		"https://github.com/other/project/releases/tag/v1.0.3",
		testReleasePath + "tag/v1.0.3-rc.1",
		testReleasePath + "tag/v1.0.3?redirect=1",
		testReleasePath + "tag/v1.0.3%2Fother",
	} {
		g := NewGitHub("zhangdailin/API-Console")
		g.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != testReleasePath+"latest" || r.URL.Host != "github.com" {
				t.Fatalf("followed an invalid release redirect: %s", r.URL)
			}
			return &http.Response{
				StatusCode: http.StatusFound, Header: http.Header{"Location": {location}},
				Body: http.NoBody, Request: r,
			}, nil
		})
		if _, err := g.Latest(context.Background()); err == nil {
			t.Errorf("accepted %q", location)
		}
	}
}
