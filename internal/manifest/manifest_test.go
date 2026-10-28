package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/im-anuvaad/phpboat/internal/app"
	"github.com/im-anuvaad/phpboat/internal/php"
)

func TestParse(t *testing.T) {
	m, err := Parse([]byte(`contract = 1
docroot = "web"
health_path = "/healthz"

[php]
memory_limit = "96M"
max_execution_time = 60
timezone = "Europe/Berlin"
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Contract != 1 || m.Docroot != "web" || m.HealthPath != "/healthz" || m.PHP["memory_limit"] != "96M" || m.PHP["timezone"] != "Europe/Berlin" || m.PHP["max_execution_time"] != "60" {
		t.Fatalf("%+v", m)
	}
	m, err = Parse([]byte("contract = 1\n"))
	if err != nil || m.HealthPath != app.DefaultHealthPath || m.Docroot != DefaultDocroot || m.PHP != nil {
		t.Fatalf("%+v %v", m, err)
	}
	m, err = Parse([]byte("contract = 1\ndocroot = \"./\"\n"))
	if err != nil || m.Docroot != "." {
		t.Fatalf("root docroot: %+v %v", m, err)
	}
}

func TestParseReportsEverything(t *testing.T) {
	_, err := Parse([]byte(`contract = 3
run = "app"
docroot = "../up"
health_path = "healthz"
colour = "blue"
persist = ["../escape"]

[php]
memory_limit = "lots"
display_errors = "1"

[[cron]]
schedule = "* * * * *"
run = "bin/x.php"
`))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"contract = 3", `unknown key "run"`, "docroot", "health_path", `unknown key "colour"`,
		`persist folder "../escape" must be a folder inside the repository`, `"cron" is not supported`, "memory_limit", "display_errors is not supported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	for _, body := range []string{"", "not toml ===", "contract = 1\n[php]\nmax_input_vars = 1.5\n", "contract = 1\n[php]\ntimezone = [\"UTC\"]\n", "contract = 1\ndocroot = \"/etc\"\n", "contract = 1\ndocroot = \".git\"\n", "contract = 1\ndocroot = \"\"\n"} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "no public/ folder") {
		t.Errorf("no public/: %v", err)
	}
	os.Mkdir(filepath.Join(dir, "public"), 0o755)
	if m, err := Load(dir); err != nil || m.Docroot != "public" {
		t.Errorf("defaults: %+v %v", m, err)
	}
	os.WriteFile(filepath.Join(dir, FileName), []byte("contract = 1\ndocroot = \"site\"\n"), 0o644)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), `docroot "site" is not a folder`) {
		t.Errorf("missing docroot: %v", err)
	}
	os.Symlink("/etc", filepath.Join(dir, "site"))
	if _, err := Load(dir); err == nil {
		t.Error("a symlinked docroot was accepted")
	}
	os.WriteFile(filepath.Join(dir, FileName), []byte("contract = 1\ndocroot = \".\"\n"), 0o644)
	if m, err := Load(dir); err != nil || m.Docroot != "." {
		t.Errorf("root: %+v %v", m, err)
	}
}

// FuzzParse: never panics, and anything accepted satisfies the contract.
func FuzzParse(f *testing.F) {
	f.Add([]byte("contract = 1\n"))
	f.Add([]byte("contract = 1\ndocroot = \"a/../../b\"\nhealth_path = \"/h\"\n[php]\nmemory_limit = \"1G\"\n"))
	f.Add([]byte("[php]\nx = 1\n"))
	f.Add([]byte("contract = 99999999999999999999\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Parse(b)
		if err != nil {
			return
		}
		if m.Contract != app.ContractVersion {
			t.Fatalf("accepted invalid manifest: %+v", m)
		}
		if c := filepath.Clean(m.Docroot); filepath.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
			t.Fatalf("docroot escapes the release: %q", m.Docroot)
		}
		if !strings.HasPrefix(m.HealthPath, "/") || strings.ContainsAny(m.HealthPath, " \t\r\n") {
			t.Fatalf("bad health path %q", m.HealthPath)
		}
		if err := php.ValidateSettings(m.PHP); err != nil {
			t.Fatalf("invalid [php] accepted: %v", err)
		}
	})
}
