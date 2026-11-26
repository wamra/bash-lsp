package bashlsp_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The documents name files of the repository in backquotes. A name that
// points at a file that is not there is a broken reference.
var docRefRe = regexp.MustCompile("`((?:docs|bench|scripts|plugin|cmd|internal|analysis)/[A-Za-z0-9_./-]+)`")

func TestDocumentsNameExistingFiles(t *testing.T) {
	for name, b := range trackedTextFiles(t) {
		if !strings.HasSuffix(name, ".md") || strings.Contains(name, "testdata/") {
			continue
		}
		for _, m := range docRefRe.FindAllStringSubmatch(string(b), -1) {
			ref := strings.TrimRight(m[1], ".")
			if _, err := os.Stat(filepath.FromSlash(ref)); err != nil {
				t.Errorf("%s names %s, which does not exist", name, ref)
			}
		}
	}
}

// A result file that a document or a script cites by name must be committed.
var resultRefRe = regexp.MustCompile(`bench/results/([A-Za-z0-9_.-]+\.(?:json|txt|md))`)

func TestCitedResultFilesExist(t *testing.T) {
	for name, b := range trackedTextFiles(t) {
		if strings.HasPrefix(name, "bench/results/") {
			continue
		}
		for _, m := range resultRefRe.FindAllStringSubmatch(string(b), -1) {
			if _, err := os.Stat(filepath.Join("bench", "results", m[1])); err != nil {
				t.Errorf("%s cites bench/results/%s, which is not committed", name, m[1])
			}
		}
	}
}
