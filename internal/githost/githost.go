// Package githost is the small part of OVC that depends on the Git host
// (spec §4.3): opening a pull/merge request. Everything else is plain git.
package githost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Host opens change requests (GitHub pull requests, GitLab merge requests).
type Host interface {
	// CreateChangeRequest opens a request to merge source into target and
	// returns its web URL.
	CreateChangeRequest(ctx context.Context, source, target, title, body string) (string, error)
}

// New returns the Host for the server config, or nil when the remote is not
// on a Git host OVC can call (a bare repository on disk in tests and the
// local playground).
func New(host, apiURL, repo, remote, token string) (Host, error) {
	switch host {
	case "", "github":
		if repo == "" {
			repo = repoFromRemote(remote, "github.com")
		}
		if repo == "" {
			return nil, nil
		}
		if apiURL == "" {
			apiURL = "https://api.github.com"
		}
		return &GitHub{APIURL: strings.TrimRight(apiURL, "/"), Repo: repo, Token: token}, nil
	case "gitlab":
		return nil, fmt.Errorf("githost: gitlab is not implemented yet")
	}
	return nil, fmt.Errorf("githost: unknown host %q", host)
}

// repoFromRemote extracts "owner/repo" from https://<domain>/owner/repo(.git).
func repoFromRemote(remote, domain string) string {
	u, err := url.Parse(remote)
	if err != nil || u.Host != domain {
		return ""
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if strings.Count(p, "/") != 1 {
		return ""
	}
	return p
}

// GitHub opens pull requests with the REST API.
type GitHub struct {
	APIURL string // https://api.github.com
	Repo   string // owner/repo
	Token  string // service account token: "Pull requests: read and write"
	HTTP   *http.Client
}

func (g *GitHub) CreateChangeRequest(ctx context.Context, source, target, title, body string) (string, error) {
	in, _ := json.Marshal(map[string]string{"title": title, "head": source, "base": target, "body": body})
	req, err := http.NewRequestWithContext(ctx, "POST", g.APIURL+"/repos/"+g.Repo+"/pulls", bytes.NewReader(in))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	h := g.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: create pull request: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		var e struct {
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Message
		for _, x := range e.Errors {
			msg += "; " + x.Message
		}
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		hint := ""
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			hint = " (the token needs \"Pull requests: Read and write\" on " + g.Repo + ")"
		}
		return "", fmt.Errorf("github: create pull request %s -> %s: HTTP %d: %s%s", source, target, resp.StatusCode, msg, hint)
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.HTMLURL == "" {
		return "", fmt.Errorf("github: unexpected create pull request response: %s", data)
	}
	return out.HTMLURL, nil
}
