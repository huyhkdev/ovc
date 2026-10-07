package githost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	h, err := New("github", "", "", "https://github.com/example-org/oracle-src.git", "tok")
	if err != nil || h.(*GitHub).Repo != "example-org/oracle-src" || h.(*GitHub).APIURL != "https://api.github.com" {
		t.Errorf("New = %+v, %v", h, err)
	}
	if h, err := New("github", "", "", "/tmp/remote.git", ""); err != nil || h != nil {
		t.Errorf("a bare repo on disk has no host API: %v %v", h, err)
	}
	if h, _ := New("github", "https://ghe.local/api/v3/", "org/r", "https://ghe.local/org/r.git", ""); h.(*GitHub).APIURL != "https://ghe.local/api/v3" {
		t.Errorf("explicit repo/api: %+v", h)
	}
	if _, err := New("gitlab", "", "", "", ""); err == nil {
		t.Error("gitlab is not implemented yet")
	}
}

func TestGitHubCreatePullRequest(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/repos/o/r/pulls" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&got)
		if got["head"] == "sync/dev/denied" {
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
			return
		}
		w.WriteHeader(201)
		w.Write([]byte(`{"html_url":"https://github.com/o/r/pull/7"}`))
	}))
	defer srv.Close()
	g := &GitHub{APIURL: srv.URL, Repo: "o/r", Token: "tok"}
	u, err := g.CreateChangeRequest(context.Background(), "sync/dev/HR/x", "dev", "title", "body")
	if err != nil || u != "https://github.com/o/r/pull/7" {
		t.Fatalf("create = %q, %v", u, err)
	}
	if got["head"] != "sync/dev/HR/x" || got["base"] != "dev" || got["title"] != "title" || got["body"] != "body" {
		t.Errorf("payload = %v", got)
	}
	_, err = g.CreateChangeRequest(context.Background(), "sync/dev/denied", "dev", "t", "b")
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "Pull requests: Read and write") {
		t.Errorf("denied = %v", err)
	}
}
