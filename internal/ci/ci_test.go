package ci

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFiles(t *testing.T) {
	fs, err := Files(Settings{GitHubTemplate: "org/t/.github/workflows/x.yml@v1", GitLabProject: "db/t"}, []string{"dev", "prd"})
	if err != nil || len(fs) != 2 {
		t.Fatalf("%v %v", fs, err)
	}
	for p, c := range fs {
		var v any
		if err := yaml.Unmarshal(c, &v); err != nil {
			t.Errorf("%s is not valid YAML: %v\n%s", p, err, c)
		}
	}
	if !strings.Contains(string(fs[".github/workflows/ovc-deploy.yml"]), `branches: ["dev", "prd"]`) {
		t.Errorf("branches not rendered: %s", fs[".github/workflows/ovc-deploy.yml"])
	}
	// nothing configured: no CI files at all (a repo used only to try OVC out)
	if fs, err := Files(Settings{}, []string{"dev"}); err != nil || len(fs) != 0 {
		t.Errorf("empty settings: %v %v", fs, err)
	}
	// half configured is a mistake
	if _, err := Files(Settings{GitHubTemplate: "x"}, []string{"dev"}); err == nil {
		t.Error("half configured must fail")
	}
}

func TestOvcYAML(t *testing.T) {
	b, err := OvcYAML("QLSC", DefaultExclude, []string{"dev", "prd"})
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Schema string `yaml:"schema"`
		Deploy struct {
			AllowDrop bool `yaml:"allow_drop"`
		} `yaml:"deploy"`
		Exclude []string `yaml:"exclude"`
	}
	if err := yaml.Unmarshal(b, &v); err != nil || v.Schema != "QLSC" || v.Deploy.AllowDrop || len(v.Exclude) != len(DefaultExclude) {
		t.Errorf("%+v %v\n%s", v, err, b)
	}
}
