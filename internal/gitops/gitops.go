// Package gitops does every Git operation of the OVC Server with the git CLI
// (spec §4.3, §9): mirror a remote, create the per-schema branches, commit with
// a declared author, three-way merge for pull/push, and push. It is host
// agnostic: GitHub or GitLab only differ by the remote URL.
//
// The mirror is a bare repository used as a cache of the remote. Local heads
// are overwritten by Fetch, so never rely on unpushed local commits.
// Commits are built with plumbing (fast-import, merge-tree) and never touch a
// working tree, which keeps concurrent requests on different branches safe.
package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
)

var (
	ErrNoBranch    = errors.New("gitops: branch not found")
	ErrExists      = errors.New("gitops: branch already exists")
	ErrStale       = errors.New("gitops: branch moved (expected head does not match)")
	ErrRejected    = errors.New("gitops: push rejected by remote")
	ErrInvalidPath = errors.New("gitops: invalid path")
	ErrNotFound    = errors.New("gitops: file not found")
)

// Config describes the remote. Token is used for HTTPS remotes through a
// credential helper fed by an environment variable; it never appears in the
// remote URL, in git config or in command lines.
type Config struct {
	Remote string
	Token  string
}

type Repo struct {
	dir string
	cfg Config

	mu    sync.Mutex
	locks map[string]chan struct{}
}

// Open returns the mirror in dir, creating a bare repository with origin set
// to cfg.Remote when it does not exist yet.
func Open(ctx context.Context, dir string, cfg Config) (*Repo, error) {
	r := &Repo{dir: dir, cfg: cfg, locks: map[string]chan struct{}{}}
	if _, err := os.Stat(path.Join(dir, "HEAD")); err != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if _, err := r.git(ctx, nil, nil, "init", "--bare", "--quiet", "--initial-branch=ovc-unused"); err != nil {
			return nil, err
		}
	}
	// origin may be absent (fresh) or stale (host switch GitHub -> GitLab).
	if _, err := r.git(ctx, nil, nil, "remote", "set-url", "origin", cfg.Remote); err != nil {
		if _, err := r.git(ctx, nil, nil, "remote", "add", "origin", cfg.Remote); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// env isolates git from the host's configuration (signing, hooks, aliases).
func (r *Repo) env() []string {
	env := append(os.Environ(),
		"GIT_DIR="+r.dir,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	if r.cfg.Token != "" {
		env = append(env, "OVC_GIT_TOKEN="+r.cfg.Token)
	}
	return env
}

func (r *Repo) credArgs() []string {
	if r.cfg.Token == "" {
		return nil
	}
	return []string{"-c", "credential.helper=", "-c",
		`credential.helper=!f() { echo username=ovc; echo "password=$OVC_GIT_TOKEN"; }; f`}
}

// git runs one command and returns stdout. The error carries stderr (with the
// token scrubbed) so callers can log it.
func (r *Repo) git(ctx context.Context, stdin io.Reader, extra []string, args ...string) ([]byte, error) {
	return r.run(ctx, stdin, extra, nil, args...)
}

// run is git with extra -c arguments and extra environment variables.
func (r *Repo) run(ctx context.Context, stdin io.Reader, extra, env []string, args ...string) ([]byte, error) {
	full := append(append([]string{}, extra...), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(r.env(), env...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if r.cfg.Token != "" {
			msg = strings.ReplaceAll(msg, r.cfg.Token, "***")
		}
		return out.Bytes(), &gitError{args: args, stderr: msg, err: err}
	}
	return out.Bytes(), nil
}

type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}
func (e *gitError) Unwrap() error { return e.err }

// Fetch makes the given branches (all when none) match the remote.
func (r *Repo) Fetch(ctx context.Context, branches ...string) error {
	specs := []string{"+refs/heads/*:refs/heads/*"}
	if len(branches) > 0 {
		specs = specs[:0]
		for _, b := range branches {
			specs = append(specs, fmt.Sprintf("+refs/heads/%s:refs/heads/%s", b, b))
		}
	}
	args := append([]string{"fetch", "--quiet", "--no-tags", "origin"}, specs...)
	_, err := r.git(ctx, nil, r.credArgs(), args...)
	return err
}

// Push sends refs/heads/<branch> to the remote without force. If the remote
// moved it returns ErrRejected; callers should Fetch and retry or re-merge.
func (r *Repo) Push(ctx context.Context, branch string) error {
	_, err := r.git(ctx, nil, r.credArgs(), "push", "--quiet", "origin",
		fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch))
	if err != nil {
		var ge *gitError
		// Only "the remote moved" is ErrRejected; a hook or branch protection
		// declining the push ("[remote rejected]") is a plain error, since
		// retrying cannot help.
		if errors.As(err, &ge) && (strings.Contains(ge.stderr, "non-fast-forward") || strings.Contains(ge.stderr, "fetch first") ||
			strings.Contains(ge.stderr, "! [rejected]")) {
			return fmt.Errorf("%w: %v", ErrRejected, err)
		}
		return err
	}
	return nil
}

// DeleteBranch removes refs/heads/<branch> locally and, when remote is true,
// on the remote too. Meant for undoing a half-finished init; it never force
// overwrites anything else.
func (r *Repo) DeleteBranch(ctx context.Context, branch string, remote bool) error {
	var firstErr error
	if remote {
		if _, err := r.git(ctx, nil, r.credArgs(), "push", "--quiet", "origin", ":refs/heads/"+branch); err != nil {
			firstErr = err
		}
	}
	if _, err := r.git(ctx, nil, nil, "update-ref", "-d", "refs/heads/"+branch); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Head returns the commit a branch points to, or ErrNoBranch.
func (r *Repo) Head(ctx context.Context, branch string) (string, error) {
	out, err := r.git(ctx, nil, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoBranch, branch)
	}
	return strings.TrimSpace(string(out)), nil
}

// Lock serialises writers of one branch (spec §9.4 step 3). It honours ctx.
func (r *Repo) Lock(ctx context.Context, branch string) (unlock func(), err error) {
	r.mu.Lock()
	ch, ok := r.locks[branch]
	if !ok {
		ch = make(chan struct{}, 1)
		r.locks[branch] = ch
	}
	r.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ValidatePath accepts only plain relative paths inside the repo: no "..",
// no ".git", no empty parts, no absolute or backslash paths, no control chars.
func ValidatePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || len(p) > 1024 {
		return fmt.Errorf("%w: %q", ErrInvalidPath, p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return fmt.Errorf("%w: %q", ErrInvalidPath, p)
		}
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %q has control characters", ErrInvalidPath, p)
		}
	}
	return nil
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// RemoteBranches lists the branches on the remote whose name starts with
// prefix (asked to the remote itself, so branches deleted there are gone).
func (r *Repo) RemoteBranches(ctx context.Context, prefix string) ([]string, error) {
	out, err := r.git(ctx, nil, r.credArgs(), "ls-remote", "--heads", "origin", "refs/heads/"+prefix+"*")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	return names, nil
}

// Authors returns "Name <email>" of the commits in from..to that touch p,
// newest first, without duplicates.
func (r *Repo) Authors(ctx context.Context, from, to, p string) ([]string, error) {
	out, err := r.git(ctx, nil, nil, "log", "--no-merges", "--format=%an <%ae>", from+".."+to, "--", p)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var authors []string
	for _, a := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if a != "" && !seen[a] {
			seen[a] = true
			authors = append(authors, a)
		}
	}
	return authors, nil
}
