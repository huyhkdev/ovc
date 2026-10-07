package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"
)

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show your identity (from git config), the OVC Server and the version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, email := GitIdentity(cmd.Context())
			server := ServerURL()
			if server == "" {
				server = "(none built in; set OVC_SERVER)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "user:    %s <%s>  (git config user.name / user.email)\nserver:  %s\nversion: %s\n",
				name, email, server, Version)
			return nil
		},
	}
}

func newInitCmd() *cobra.Command {
	var db string
	cmd := &cobra.Command{
		Use:   "init <SCHEMA> --db <alias>",
		Short: "Add a schema's folder of stub files to the branch of a database",
		Long: "Reads only the object names of SCHEMA from database <alias> and adds the folder\n" +
			"<SCHEMA>/ to the branch of that database in the shared repository\n" +
			"(init HR --db dev -> HR/ on branch dev; the branch is created if needed).\n" +
			"Run it once per database; every database merges the schema's first init so\n" +
			"its files can be merged between branches. File contents are pulled later, per\n" +
			"object, when you need to edit them.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if db == "" {
				return coded(ExitUsage, "--db <alias> is required")
			}
			c, err := newClient(cmd.Context())
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			out := cmd.OutOrStdout()
			id, err := c.StartInit(ctx, args[0], db)
			if err != nil {
				return mapAPIError(err)
			}
			fmt.Fprintf(out, "job %s started\n", id)
			j, err := c.WaitJob(ctx, id, 500*time.Millisecond, func(p string) { fmt.Fprintf(out, "  %s\n", p) })
			if err != nil {
				return err
			}
			if j.Status != "succeeded" {
				return coded(ExitError, "init failed: %s", j.Error)
			}
			var r struct {
				Schema    string         `json:"schema"`
				DB        string         `json:"db"`
				Branch    string         `json:"branch"`
				Dir       string         `json:"dir"`
				Objects   int            `json:"objects"`
				ByType    map[string]int `json:"by_type"`
				Commit    string         `json:"commit"`
				Baseline  string         `json:"baseline"`
				FirstDB   bool           `json:"first_db"`
				NewBranch bool           `json:"new_branch"`
				Added     int            `json:"added"`
				Removed   int            `json:"removed"`
				Warnings  []string       `json:"warnings"`
			}
			if err := json.Unmarshal(j.Result, &r); err != nil {
				return err
			}
			fmt.Fprintf(out, "\nOK %s on db %s: %d objects\n", r.Schema, r.DB, r.Objects)
			types := make([]string, 0, len(r.ByType))
			for t := range r.ByType {
				types = append(types, t)
			}
			sort.Strings(types)
			for _, t := range types {
				fmt.Fprintf(out, "  %-18s %d\n", t, r.ByType[t])
			}
			newb := ""
			if r.NewBranch {
				newb = " (new branch)"
			}
			fmt.Fprintf(out, "branch   %s%s\n", r.Branch, newb)
			fmt.Fprintf(out, "folder   %s/\n", r.Dir)
			fmt.Fprintf(out, "commit   %s\n", r.Commit)
			if r.FirstDB {
				fmt.Fprintf(out, "baseline %s (first database of %s)\n", r.Baseline, r.Schema)
			} else {
				fmt.Fprintf(out, "baseline %s; this database differs by +%d -%d stubs\n", r.Baseline, r.Added, r.Removed)
			}
			for _, w := range r.Warnings {
				fmt.Fprintf(out, "warning: %s\n", w)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&db, "db", "", "database alias (as configured on the server); also the branch written to")
	return cmd
}

func newSchemasCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schemas",
		Short: "List the registered schemas",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient(cmd.Context())
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			list, err := c.Schemas(ctx)
			if err != nil {
				return mapAPIError(err)
			}
			out := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintln(out, "no schemas registered")
				return nil
			}
			for _, s := range list {
				dbs := make([]string, 0, len(s.DBs))
				for d := range s.DBs {
					dbs = append(dbs, d)
				}
				sort.Strings(dbs)
				fmt.Fprintf(out, "%s  (folder %s/)\n", s.Name, s.Dir)
				for _, d := range dbs {
					v := s.DBs[d]
					fmt.Fprintf(out, "  branch %-8s %5d objects  by %s %s\n", v.Branch, v.Objects, v.CreatedBy, v.CreatedAt.Local().Format("2006-01-02 15:04"))
				}
			}
			return nil
		},
	}
}
