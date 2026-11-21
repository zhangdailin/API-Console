package selfupdate

import (
	"bytes"
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

	"golang.org/x/net/html"
)

const maxReleaseMetadataSize = 2 << 20

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
	if g.token == "" {
		return g.latestPublic(ctx)
	}
	return g.latestAPI(ctx)
}

func releaseRequest(ctx context.Context, client *http.Client, endpoint string, headers http.Header) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		r.Header = headers
	}
	r.Header.Set("User-Agent", "API-Console-Updater")
	res, err := client.Do(r)
	if err != nil {
		return nil, &releaseCheckError{Code: "github_unreachable", Message: "Could not reach GitHub to check for updates."}
	}
	return res, nil
}

func readReleaseMetadata(res *http.Response) ([]byte, error) {
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, githubCheckFailure(res, time.Now().UTC())
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxReleaseMetadataSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxReleaseMetadataSize {
		return nil, errors.New("release metadata too large")
	}
	return b, nil
}

func (g *GitHub) latestAPI(ctx context.Context) (Release, error) {
	res, err := releaseRequest(ctx, g.Client, "https://api.github.com/repos/"+g.Repository+"/releases/latest", http.Header{
		"Accept":               {"application/vnd.github+json"},
		"Authorization":        {"Bearer " + g.token},
		"X-Github-Api-Version": {"2022-11-28"},
	})
	if err != nil {
		return Release{}, err
	}
	body, err := readReleaseMetadata(res)
	if err != nil {
		return Release{}, err
	}
	var release Release
	if err := json.Unmarshal(body, &release); err != nil {
		return Release{}, err
	}
	if release.Draft || release.Prerelease || !stableTag(release.Tag) {
		return Release{}, errors.New("the latest GitHub release is not a stable version")
	}
	return release, nil
}

func stableTag(tag string) bool {
	p := versionParts(tag)
	return p != nil && p[4] == ""
}

// The public release page identifies the stable tag without using a REST API
// quota. Its asset fragment lists the files actually attached to that tag.
func (g *GitHub) latestPublic(ctx context.Context) (Release, error) {
	const origin = "https://github.com"
	base := "/" + g.Repository + "/releases/"
	client := *g.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := releaseRequest(ctx, &client, origin+base+"latest", nil)
	if err != nil {
		return Release{}, err
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		return Release{}, githubCheckFailure(res, time.Now().UTC())
	}
	location, err := res.Location()
	res.Body.Close()
	if res.StatusCode < 300 || res.StatusCode >= 400 || err != nil ||
		!trustedURL(location.String()) || location.Hostname() != "github.com" ||
		location.RawQuery != "" || location.ForceQuery || location.Fragment != "" {
		return Release{}, errors.New("GitHub did not return a trusted latest release link")
	}
	tag, ok := strings.CutPrefix(location.Path, base+"tag/")
	if !ok || !stableTag(tag) {
		return Release{}, errors.New("the latest GitHub release is not a stable version in the configured repository")
	}

	res, err = releaseRequest(ctx, &client, origin+base+"expanded_assets/"+url.PathEscape(tag), nil)
	if err != nil {
		return Release{}, err
	}
	body, err := readReleaseMetadata(res)
	if err != nil {
		return Release{}, err
	}
	prefix := base + "download/" + tag + "/"
	release := Release{Tag: tag, URL: origin + base + "tag/" + url.PathEscape(tag)}
	seen := map[string]bool{}
	z := html.NewTokenizer(bytes.NewReader(body))
	for kind := z.Next(); kind != html.ErrorToken; kind = z.Next() {
		if kind != html.StartTagToken {
			continue
		}
		link := z.Token()
		if link.Data != "a" {
			continue
		}
		for _, attr := range link.Attr {
			if attr.Key != "href" {
				continue
			}
			u, err := url.Parse(attr.Val)
			if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
				continue
			}
			name, ok := strings.CutPrefix(u.Path, prefix)
			if !ok || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") || seen[name] {
				continue
			}
			seen[name] = true
			release.Assets = append(release.Assets, Asset{Name: name, URL: origin + base + "download/" + url.PathEscape(tag) + "/" + url.PathEscape(name)})
		}
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
