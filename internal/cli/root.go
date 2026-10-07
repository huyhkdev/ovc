// Package cli holds the cobra commands of the ovc binary (spec §7).
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"

	"ovc/internal/client"
)

// Version is set at build time with -ldflags.
var Version = "0.1.0-dev"

// Exit codes (spec §7.6).
const (
	ExitError       = 1
	ExitUsage       = 2
	ExitNotLoggedIn = 3 // no declared identity, or the server cannot be reached
	ExitDenied      = 4
	ExitConflict    = 5
)

// ExitError carries the process exit code.
type CodedError struct {
	Code int
	Err  error
}

func (e *CodedError) Error() string { return e.Err.Error() }
func (e *CodedError) Unwrap() error { return e.Err }

func coded(code int, format string, args ...any) error {
	return &CodedError{Code: code, Err: fmt.Errorf(format, args...)}
}

// DefaultServer is the OVC Server this binary talks to, set when the company
// builds the CLI: go build -ldflags "-X ovc/internal/cli.DefaultServer=https://ovc.company.local".
// The server is not something a dev configures; OVC_SERVER overrides it for tests.
var DefaultServer = ""

// ServerURL is the OVC Server to use.
func ServerURL() string {
	if s := strings.TrimSpace(os.Getenv("OVC_SERVER")); s != "" {
		return strings.TrimRight(s, "/")
	}
	return strings.TrimRight(DefaultServer, "/")
}

// GitIdentity is the dev's identity as git knows it in the current directory
// (repository config first, then global): the same name and email that sign
// their own commits.
func GitIdentity(ctx context.Context) (name, email string) {
	get := func(key string) string {
		out, err := exec.CommandContext(ctx, "git", "config", "--get", key).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return get("user.name"), get("user.email")
}

// newClient builds a client from the built-in server and the git identity,
// or explains what is missing.
func newClient(ctx context.Context) (*client.Client, error) {
	server := ServerURL()
	if server == "" {
		return nil, coded(ExitNotLoggedIn, "this ovc binary has no OVC Server built in: get ovc from your OVC Server, or set OVC_SERVER=<url> to test")
	}
	name, email := GitIdentity(ctx)
	var missing []string
	if name == "" {
		missing = append(missing, `git config --global user.name "Your Name"`)
	}
	if email == "" || !strings.Contains(email, "@") {
		missing = append(missing, `git config --global user.email you@company.local`)
	}
	if len(missing) > 0 {
		problem := "is not set"
		if email != "" && !strings.Contains(email, "@") {
			problem = fmt.Sprintf("is incomplete (user.email %q is not an email address)", email)
		}
		return nil, coded(ExitNotLoggedIn, "ovc signs its commits with your git identity, which %s. Run:\n  %s", problem, strings.Join(missing, "\n  "))
	}
	return &client.Client{Server: server, Name: name, Email: email, Version: Version}, nil
}

func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "ovc",
		Short:         "OVC – Oracle Version Control",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().Bool("json", false, "machine-readable output")
	root.PersistentFlags().Bool("verbose", false, "verbose output")
	root.AddCommand(
		&cobra.Command{
			Use:   "version",
			Short: "Print the CLI version",
			Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), Version) },
		},
		newWhoamiCmd(), newInitCmd(), newSchemasCmd(), newGetCmd(),
	)
	return root
}

// signalContext is cancelled by Ctrl-C.
func signalContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(cmd.Context(), os.Interrupt)
}

// mapAPIError turns server errors into the CLI's exit codes.
func mapAPIError(err error) error {
	var ae *client.APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 409:
			return &CodedError{Code: ExitConflict, Err: err}
		case ae.Code == "IDENTITY_REQUIRED":
			return &CodedError{Code: ExitNotLoggedIn, Err: err}
		case ae.Status == 400:
			return &CodedError{Code: ExitUsage, Err: err}
		case ae.Status == 403:
			return &CodedError{Code: ExitDenied, Err: err}
		}
	}
	return err
}
