// Package github is a minimal GitHub REST client: token validation, repo
// listing, branches and commits, and the source tarball of a commit. The
// personal access token is only sent to the API host.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// DefaultBase is the GitHub REST API.
const DefaultBase = "https://api.github.com"

// API is a minimal GitHub REST client.
type API struct {
	Token string
	Base  string // default DefaultBase (unit tests use a local server)
	HTTP  *http.Client
}

func (a *API) base() string {
	if a.Base != "" {
		return a.Base
	}
	return DefaultBase
}

// Repo is a github.com repository.
type Repo struct {
	Owner string
	Name  string
}

// URL is the repository's web address.
func (r Repo) URL() string { return "https://github.com/" + r.Owner + "/" + r.Name }

// String is owner/name.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

var repoRe = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))/([A-Za-z0-9._-]{1,100})$`)

// ParseRepo accepts "owner/repo", "github.com/owner/repo" and
// "https://github.com/owner/repo(.git)".
func ParseRepo(s string) (Repo, error) {
	orig := strings.TrimSpace(s)
	s = strings.TrimPrefix(orig, "https://")
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	m := repoRe.FindStringSubmatch(s)
	if m == nil || m[2] == "." || m[2] == ".." {
		return Repo{}, fmt.Errorf("unsupported repository %q: use https://github.com/<owner>/<repo>", orig)
	}
	return Repo{Owner: m[1], Name: m[2]}, nil
}

// TokenInfo describes what a token can do.
type TokenInfo struct {
	Login  string
	Scopes string // classic tokens only (X-OAuth-Scopes)
}

// RepoInfo is one repository the token can read.
type RepoInfo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// ErrNotFound means the API answered 404 (or 403 for private data without
// access).
var ErrNotFound = errors.New("not found")

func (a *API) do(ctx context.Context, path string, out any) (http.Header, error) {
	c := a.HTTP
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base()+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("github: token is invalid or expired")
	case http.StatusForbidden, http.StatusNotFound:
		return nil, fmt.Errorf("github: %s: %w (HTTP %d; for a private repository, set a GitHub token with Contents: Read-only in Settings)", path, ErrNotFound, resp.StatusCode)
	default:
		return nil, fmt.Errorf("github: %s: HTTP %d", path, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return nil, fmt.Errorf("github: %s: %w", path, err)
		}
	}
	return resp.Header, nil
}

// Validate checks the token and returns the account it belongs to.
func (a *API) Validate(ctx context.Context) (TokenInfo, error) {
	var u struct {
		Login string `json:"login"`
	}
	h, err := a.do(ctx, "/user", &u)
	if err != nil {
		return TokenInfo{}, err
	}
	return TokenInfo{Login: u.Login, Scopes: h.Get("X-OAuth-Scopes")}, nil
}

// Repos lists repositories the token can access (first 300).
func (a *API) Repos(ctx context.Context) ([]RepoInfo, error) {
	var all []RepoInfo
	for page := 1; page <= 3; page++ {
		var rs []RepoInfo
		if _, err := a.do(ctx, fmt.Sprintf("/user/repos?per_page=100&sort=pushed&page=%d", page), &rs); err != nil {
			return nil, err
		}
		all = append(all, rs...)
		if len(rs) < 100 {
			break
		}
	}
	return all, nil
}

func repoPath(r Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

// Repository returns the repository's information, including its default
// branch.
func (a *API) Repository(ctx context.Context, r Repo) (RepoInfo, error) {
	var info RepoInfo
	_, err := a.do(ctx, repoPath(r), &info)
	return info, err
}

// Branches returns the names of up to 100 branches of r.
func (a *API) Branches(ctx context.Context, r Repo) ([]string, error) {
	var bs []struct {
		Name string `json:"name"`
	}
	if _, err := a.do(ctx, repoPath(r)+"/branches?per_page=100", &bs); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return out, nil
}

// BranchHead returns the SHA of the commit that branch points to. Branch
// names may contain slashes; each part is escaped on its own.
func (a *API) BranchHead(ctx context.Context, r Repo, branch string) (string, error) {
	parts := strings.Split(branch, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	var b struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if _, err := a.do(ctx, repoPath(r)+"/branches/"+strings.Join(parts, "/"), &b); err != nil {
		return "", err
	}
	return b.Commit.SHA, nil
}

// Commit is one commit of a repository.
type Commit struct {
	SHA       string    `json:"sha"`
	Subject   string    `json:"subject"` // first line of the message
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"date"`
}

type rawCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
}

func (c rawCommit) commit() Commit {
	subject, _, _ := strings.Cut(c.Commit.Message, "\n")
	return Commit{SHA: c.SHA, Subject: strings.TrimSpace(subject), Author: c.Commit.Author.Name, CreatedAt: c.Commit.Author.Date}
}

// Commits returns up to n commits of branch, newest first.
func (a *API) Commits(ctx context.Context, r Repo, branch string, n int) ([]Commit, error) {
	var raw []rawCommit
	if _, err := a.do(ctx, fmt.Sprintf("%s/commits?sha=%s&per_page=%d", repoPath(r), url.QueryEscape(branch), min(max(n, 1), 100)), &raw); err != nil {
		return nil, err
	}
	out := make([]Commit, 0, len(raw))
	for _, c := range raw {
		out = append(out, c.commit())
	}
	return out, nil
}

// Commit returns the commit that ref (a full SHA or a branch name) points to.
func (a *API) Commit(ctx context.Context, r Repo, ref string) (Commit, error) {
	var raw rawCommit
	if _, err := a.do(ctx, repoPath(r)+"/commits/"+url.PathEscape(ref), &raw); err != nil {
		return Commit{}, err
	}
	return raw.commit(), nil
}

// Tarball writes the source archive of commit sha of r to path (at most max
// bytes). GitHub answers with a redirect to a signed URL on another host;
// the token is never sent there.
func (a *API) Tarball(ctx context.Context, r Repo, sha, path string, max int64) error {
	return a.fetch(ctx, a.base()+repoPath(r)+"/tarball/"+url.PathEscape(sha), "", path, max, "source of "+sha)
}

// fetch downloads u into path (at most max bytes), refusing to leave a
// partial file behind.
func (a *API) fetch(ctx context.Context, u, accept, path string, max int64, what string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	c := http.Client{} // no overall timeout: ctx bounds the download
	if a.HTTP != nil {
		c = *a.HTTP
	}
	// Never send the token anywhere but the API host (not even to another
	// port of it); Go's own rule compares host names only.
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if r.URL.Host != via[0].URL.Host {
			r.Header.Del("Authorization")
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("github: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github: download %s: HTTP %d", what, resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = fmt.Errorf("the source is larger than %d MB", max>>20)
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("github: download: %w", err)
	}
	return nil
}
