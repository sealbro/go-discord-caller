package telemetry

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var metricNameLiteral = regexp.MustCompile(`"(gdc\.[a-z0-9_.]+)"`)

func TestEveryMetricIsDocumented(t *testing.T) {
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "METRICS.md"))
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]string{}
	err = filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return err
		}
		for _, m := range metricNameLiteral.FindAllSubmatch(src, -1) {
			names[string(m[1])] = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("found no gdc.* instrument names; the scan is broken")
	}

	for name, path := range names {
		if !strings.Contains(string(docs), "`"+name+"`") {
			t.Errorf("metric %q (%s) is not documented in docs/METRICS.md", name, path)
		}
	}
}
