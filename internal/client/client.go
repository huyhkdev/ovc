// Package client is the HTTP client the ovc CLI uses to talk to OVC Server.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	Server  string
	Name    string
	Email   string
	Version string
	HTTP    *http.Client
}

// APIError is an error response of the server ({"error":{code,message,details}}).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.Status)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Server, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OVC-Client-Version", c.Version)
	req.Header.Set("X-OVC-User-Name", c.Name)
	req.Header.Set("X-OVC-User-Email", c.Email)
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach OVC Server at %s: %w", c.Server, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Code != "" {
			return &APIError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
		}
		return &APIError{Status: resp.StatusCode, Code: "HTTP_ERROR", Message: strings.TrimSpace(string(data))}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type Job struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Status   string          `json:"status"`
	Progress string          `json:"progress"`
	Result   json.RawMessage `json:"result"`
	Error    string          `json:"error"`
}

func (c *Client) StartInit(ctx context.Context, schema, dbAlias string) (string, error) {
	var out struct {
		JobID string `json:"job_id"`
	}
	err := c.do(ctx, "POST", "/api/v1/schemas", map[string]any{"schema": schema, "db_alias": dbAlias}, &out)
	return out.JobID, err
}

// HydrateRequest asks the server to give stubs their real content (spec §9.8).
// Paths are relative to the owner's folder.
type HydrateRequest struct {
	DB    string   `json:"db"`
	Paths []string `json:"paths,omitempty"`
	All   bool     `json:"all,omitempty"`
	Types []string `json:"types,omitempty"`
}

type PathReason struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type HydrateResult struct {
	Schema    string         `json:"schema"`
	DB        string         `json:"db"`
	Branch    string         `json:"branch"`
	Commit    string         `json:"commit"`
	Hydrated  []string       `json:"hydrated"`
	Synced    []string       `json:"synced"`
	UpToDate  []string       `json:"up_to_date"`
	Pending   []string       `json:"pending"`
	Conflicts []SyncConflict `json:"conflicts"`
	Skipped   []PathReason   `json:"skipped"`
	Failed    []PathReason   `json:"failed"`
	Warnings  []string       `json:"warnings"`
	ByType    map[string]int `json:"by_type"`
}

type SyncConflict struct {
	Path     string   `json:"path"`
	Reason   string   `json:"reason"`
	Branch   string   `json:"branch"`
	URL      string   `json:"url"`
	Existing bool     `json:"existing"`
	Authors  []string `json:"authors"`
	Error    string   `json:"error"`
}

func (c *Client) StartHydrate(ctx context.Context, schema string, req HydrateRequest) (string, error) {
	var out struct {
		JobID string `json:"job_id"`
	}
	err := c.do(ctx, "POST", "/api/v1/schemas/"+url.PathEscape(schema)+"/hydrate", req, &out)
	return out.JobID, err
}

func (c *Client) Job(ctx context.Context, id string) (Job, error) {
	var j Job
	return j, c.do(ctx, "GET", "/api/v1/jobs/"+id, nil, &j)
}

// WaitJob polls until the job leaves "running", calling onProgress when the
// progress text changes.
func (c *Client) WaitJob(ctx context.Context, id string, every time.Duration, onProgress func(string)) (Job, error) {
	last := ""
	for {
		j, err := c.Job(ctx, id)
		if err != nil {
			return j, err
		}
		if j.Progress != last && onProgress != nil {
			last = j.Progress
			onProgress(j.Progress)
		}
		if j.Status != "running" {
			return j, nil
		}
		select {
		case <-ctx.Done():
			return j, errors.Join(ctx.Err(), errors.New("the job keeps running on the server: ovc job "+id))
		case <-time.After(every):
		}
	}
}

type Schema struct {
	Name      string        `json:"name"`
	Dir       string        `json:"dir"`
	Baseline  string        `json:"baseline"`
	DBs       map[string]DB `json:"dbs"`
	CreatedBy string        `json:"created_by"`
	CreatedAt time.Time     `json:"created_at"`
}

type DB struct {
	Branch    string    `json:"branch"`
	Commit    string    `json:"commit"`
	Objects   int       `json:"objects"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

func (c *Client) Schemas(ctx context.Context) ([]Schema, error) {
	var out struct {
		Schemas []Schema `json:"schemas"`
	}
	err := c.do(ctx, "GET", "/api/v1/schemas", nil, &out)
	return out.Schemas, err
}
