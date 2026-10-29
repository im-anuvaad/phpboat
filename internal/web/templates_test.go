package web

import (
	"bytes"
	"testing"
	"time"

	"github.com/im-anuvaad/phpboat/internal/apps"
	"github.com/im-anuvaad/phpboat/internal/deployer"
	"github.com/im-anuvaad/phpboat/internal/edge"
	"github.com/im-anuvaad/phpboat/internal/github"
	"github.com/im-anuvaad/phpboat/internal/hostinfo"
)

// Templates are only checked when executed, so render the main pages with
// representative data: a field that no longer exists fails here, not in
// front of the user.
func TestTemplatesRender(t *testing.T) {
	s := &Server{}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	row := AppRow{App: apps.App{Name: "web", Repo: "https://github.com/me/web", Branch: "main", Domain: "web.example.com", Concurrency: 4}}
	const sha = "0123456789abcdef0123456789abcdef01234567"
	current := deployer.Build{App: "web", ID: sha[:12], CommitSHA: sha, Subject: "One", Current: true}
	commits := []commitView{
		{Commit: github.Commit{SHA: "fedcba9876543210fedcba9876543210fedcba98", Subject: "Two", Author: "Ann", CreatedAt: time.Now()}},
		{Commit: github.Commit{SHA: sha, Subject: "One", Author: "Ann", CreatedAt: time.Now()}, Deployed: true},
	}
	pages := map[string]any{
		"home": map[string]any{"Apps": []AppRow{row}, "Host": hostinfo.Read(t.TempDir()), "Warnings": []string{"w"}},
		"app": map[string]any{"App": &row, "Range": "1h", "Ranges": []string{"1h"},
			"Current": current, "Builds": []deployer.Build{current},
			"Commits": commits, "Behind": commitStatus(nil, sha),
			"Edit": apps.Input{Repo: row.Repo, Branch: "main"}, "EdgeHost": edge.HostStatus{Host: "web.example.com"}},
		"app_new":  map[string]any{"In": apps.Input{RestoreFrom: apps.AutoFolder}, "BucketSet": true, "Folders": []string{"web"}},
		"settings": map[string]any{"Edge": edge.Status{Dashboard: "d.example.com", TokenSet: true}, "Policy": "every 3h"},
	}
	for name, data := range pages {
		var b bytes.Buffer
		if err := s.pages[name].ExecuteTemplate(&b, "layout", Page{Title: name, CSRF: "x", Data: data}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// No commits yet: the page says so.
	var b bytes.Buffer
	if err := s.pages["app"].ExecuteTemplate(&b, "layout", Page{Data: map[string]any{"App": &row}}); err != nil || !bytes.Contains(b.Bytes(), []byte("The branch has no commits yet")) {
		t.Errorf("app without commits: %v", err)
	}
	// PHP files found in DATA_DIR by the last backup: the backups section warns.
	php := row
	php.PHPInData, php.PHPInDataList = 7, []string{"uploads/shell.php"}
	b.Reset()
	if err := s.pages["app"].ExecuteTemplate(&b, "layout", Page{Data: map[string]any{"App": &php}}); err != nil ||
		!bytes.Contains(b.Bytes(), []byte("7 PHP file(s) found in the data folder at the last backup (uploads/shell.php, …)")) {
		t.Errorf("app with PHP files in data: %v", err)
	}
}
