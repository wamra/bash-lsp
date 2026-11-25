package bashlsp_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The release carries one version number in three places.
func TestVersionsAgree(t *testing.T) {
	raw, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(raw))
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(version) {
		t.Fatalf("VERSION holds %q, want MAJOR.MINOR.PATCH", version)
	}

	pj, err := os.ReadFile("plugin/.claude-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(pj, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != version {
		t.Errorf("plugin.json version is %q, VERSION is %q", manifest.Version, version)
	}

	cl, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	heading := regexp.MustCompile(`(?m)^## \[?(\d+\.\d+\.\d+)\]?`).FindStringSubmatch(string(cl))
	if heading == nil {
		t.Fatal("CHANGELOG.md has no version heading")
	}
	if heading[1] != version {
		t.Errorf("top CHANGELOG.md heading is %q, VERSION is %q", heading[1], version)
	}
}

// trackedTextFiles returns the path and content of every tracked file that
// holds text. It skips when the tree is not a git checkout.
func trackedTextFiles(t *testing.T) map[string][]byte {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	files := map[string][]byte{}
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) == 0 {
			continue
		}
		b, err := os.ReadFile(string(p))
		if err != nil {
			continue // a tracked file deleted in the working tree
		}
		if bytes.IndexByte(b, 0) >= 0 {
			continue // binary
		}
		files[string(p)] = b
	}
	return files
}

// Nothing tracked may carry an attribution trailer, a generated-by line, or
// wording that belongs to an automated workflow. The patterns are assembled
// from pieces so that this file does not match them.
func TestNoAttributionLines(t *testing.T) {
	co := "Co-" + "Authored-" + "By:"
	gen := "Generated " + "with \\["
	res := []*regexp.Regexp{
		regexp.MustCompile(`(?im)^\s*` + co),
		regexp.MustCompile(`(?m)^\s*.{0,4}` + gen),
		regexp.MustCompile(`(?i)` + "agen" + "tic"),
		regexp.MustCompile(`(?i)` + "sub" + "agent"),
		regexp.MustCompile(`(?i)` + "super" + "powers:"),
	}
	for name, b := range trackedTextFiles(t) {
		for n, line := range strings.Split(string(b), "\n") {
			for _, re := range res {
				if re.MatchString(line) {
					t.Errorf("%s:%d matches %s", name, n+1, re)
					break
				}
			}
		}
	}
}
