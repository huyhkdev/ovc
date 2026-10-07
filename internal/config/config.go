// Package config loads ovc-server.yaml (spec §13.1) and the CLI config (§13.2).
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"ovc/internal/oracle/export"
)

type Server struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"public_url"`
	Git       struct {
		Host      string `yaml:"host"` // github | gitlab
		Remote    string `yaml:"remote"`
		APIURL    string `yaml:"api_url"`
		Repo      string `yaml:"repo"`
		TokenEnv  string `yaml:"token_env"`
		Committer struct {
			Name  string `yaml:"name"`
			Email string `yaml:"email"`
		} `yaml:"committer"`
	} `yaml:"git"`
	// Envs are the environments a schema gets branches for (suffix of <schema>_<env>).
	Envs    []string `yaml:"envs"`
	Exclude []string `yaml:"exclude"`
	CI      struct {
		GitHubTemplate string `yaml:"github_template"`
		GitLabProject  string `yaml:"gitlab_project"`
		GitLabFile     string `yaml:"gitlab_file"`
	} `yaml:"ci"`
	Storage struct {
		MirrorDir string `yaml:"mirror_dir"`
		StateFile string `yaml:"state_file"`
	} `yaml:"storage"`
	MetadataDB struct {
		DSNEnv string `yaml:"dsn_env"`
	} `yaml:"metadata_db"`
	Databases map[string]Database `yaml:"databases"`
	Export    struct {
		Workers int `yaml:"workers"`
	} `yaml:"export"`
	Drift struct {
		Schedule string `yaml:"schedule"`
	} `yaml:"drift"`
	CLI struct {
		MinVersion  string `yaml:"min_version"`
		DownloadDir string `yaml:"download_dir"`
	} `yaml:"cli"`
}

// Database is a read-only connection (OVC_READER). The password is never in the
// file: it is read from the environment variable named by PasswordEnv.
type Database struct {
	DSN         string `yaml:"dsn"` // host:port/service
	User        string `yaml:"user"`
	PasswordEnv string `yaml:"password_env"`
}

func (d Database) Password() string { return os.Getenv(d.PasswordEnv) }

// ExportConfig converts the DSN "host:port/service" into a connection config.
func (d Database) ExportConfig() (export.Config, error) {
	hostport, service, ok := strings.Cut(strings.TrimPrefix(d.DSN, "//"), "/")
	if !ok || service == "" {
		return export.Config{}, fmt.Errorf("db dsn %q: want host:port/service", d.DSN)
	}
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		host, portStr = hostport, "1521"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || host == "" {
		return export.Config{}, fmt.Errorf("db dsn %q: bad host or port", d.DSN)
	}
	if d.PasswordEnv != "" && d.Password() == "" {
		return export.Config{}, fmt.Errorf("environment variable %s is empty", d.PasswordEnv)
	}
	return export.Config{Host: host, Port: port, Service: service, User: d.User, Password: d.Password()}, nil
}

func LoadServer(path string) (*Server, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Server
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Export.Workers <= 0 {
		c.Export.Workers = 8
	}
	if len(c.Envs) == 0 {
		c.Envs = []string{"dev", "prd"}
	}
	if c.Git.Committer.Name == "" {
		c.Git.Committer.Name = "OVC"
	}
	if c.Git.Committer.Email == "" {
		c.Git.Committer.Email = "ovc@localhost"
	}
	if c.Storage.MirrorDir == "" {
		c.Storage.MirrorDir = "/var/lib/ovc/repos"
	}
	if c.Storage.StateFile == "" {
		c.Storage.StateFile = c.Storage.MirrorDir + "/../ovc-state.json"
	}
	if c.CLI.MinVersion == "" {
		c.CLI.MinVersion = "0.0.0"
	}
	return &c, nil
}
