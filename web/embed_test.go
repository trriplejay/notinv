package webassets

import (
	"io/fs"
	"regexp"
	"testing"
)

func TestEmbeddedAssetsStayLocal(t *testing.T) {
	external := regexp.MustCompile(`(?i)https?://`)
	err := fs.WalkDir(assets, "static", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		if external.Match(data) {
			t.Errorf("external URL literal in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"static/vendor/chart.umd.min.js", "static/vendor/chartjs-adapter-date-fns.bundle.min.js"} {
		data, err := assets.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) < 40000 {
			t.Errorf("%s is not a full library distribution (%d bytes)", path, len(data))
		}
	}
}
