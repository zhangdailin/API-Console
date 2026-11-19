package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}
type Release struct {
	Tag        string  `json:"tag_name"`
	URL        string  `json:"html_url"`
	Notes      string  `json:"body"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}
type ReleaseSource interface {
	Latest(context.Context) (Release, error)
	Download(context.Context, Asset, io.Writer, int64) error
}
type GitHub struct {
	Repository string
	Client     *http.Client
	token      string
}

func trustedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	switch u.Hostname() {
	case "api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	}
	return false
}
func NewGitHub(repo string) *GitHub {
	return &GitHub{Repository: repo, token: strings.TrimSpace(os.Getenv("ORCHIDS_UPDATE_GITHUB_TOKEN")), Client: &http.Client{Timeout: 15 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !trustedURL(r.URL.String()) {
			return errors.New("untrusted release redirect")
		}
		if r.URL.Hostname() != "api.github.com" {
			r.Header.Del("Authorization")
		}
		return nil
	}}}
}
func (g *GitHub) Latest(ctx context.Context) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+g.Repository+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("User-Agent", "API-Console-Updater")
	r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		r.Header.Set("Authorization", "Bearer "+g.token)
	}
	res, err := g.Client.Do(r)
	if err != nil {
		return Release{}, &releaseCheckError{Code: "github_unreachable", Message: "Could not reach GitHub to check for updates."}
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return Release{}, githubCheckFailure(res, time.Now().UTC())
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024+1))
	if err != nil {
		return Release{}, err
	}
	if len(b) > 2*1024*1024 {
		return Release{}, errors.New("release metadata too large")
	}
	var release Release
	err = json.Unmarshal(b, &release)
	if err != nil {
		return Release{}, err
	}
	p := versionParts(release.Tag)
	if release.Draft || release.Prerelease || p == nil || p[4] != "" {
		return Release{}, errors.New("最新发行版不是有效的稳定版本")
	}
	return release, nil
}
func (g *GitHub) Download(ctx context.Context, a Asset, w io.Writer, limit int64) error {
	u, err := url.Parse(a.URL)
	if err != nil || !trustedURL(a.URL) || u.Hostname() != "github.com" || !strings.HasPrefix(u.EscapedPath(), "/"+g.Repository+"/releases/download/") || u.RawQuery != "" {
		return errors.New("untrusted release asset URL")
	}
	if a.Size < 0 || a.Size > limit {
		return errors.New("release asset size exceeds limit")
	}
	r, err := http.NewRequestWithContext(ctx, "GET", a.URL, nil)
	if err != nil {
		return err
	}
	res, err := g.Client.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("下载失败：HTTP %d", res.StatusCode)
	}
	if res.ContentLength > limit {
		return errors.New("release asset size exceeds limit")
	}
	n, err := io.Copy(w, io.LimitReader(res.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("release asset size exceeds limit")
	}
	if a.Size > 0 && n != a.Size {
		return errors.New("release asset truncated")
	}
	return nil
}
