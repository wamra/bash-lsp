package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/query"
)

// The fake code-index: a copy of the test binary named code-index. It reads
// what to do from <its directory>/behavior.json and writes what it saw to
// the file that names.
type fakeBehavior struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
	Record string `json:"record"`
}

type fakeSaw struct {
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
	Env  []string `json:"env"`
}

func runFakeCodeIndex() {
	exe, _ := os.Executable()
	var b fakeBehavior
	data, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "behavior.json"))
	if err != nil || json.Unmarshal(data, &b) != nil {
		os.Exit(64)
	}
	cwd, _ := os.Getwd()
	saw, _ := json.Marshal(fakeSaw{os.Args[1:], cwd, os.Environ()})
	if b.Record != "" {
		os.WriteFile(b.Record, saw, 0o644)
	}
	fmt.Fprint(os.Stdout, b.Stdout)
	fmt.Fprint(os.Stderr, b.Stderr)
	os.Exit(b.Exit)
}

// installFake puts the fake code-index in a new directory outside any
// workspace, makes it the only entry of PATH and returns the file it
// records into.
func installFake(t *testing.T, b fakeBehavior) (record string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake program is a copy of the test binary named code-index")
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	record = filepath.Join(t.TempDir(), "saw.json")
	b.Record = record
	data, _ := json.Marshal(b)
	if err := os.WriteFile(filepath.Join(dir, "behavior.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, "code-index"), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return record
}

func readSaw(t *testing.T, record string) fakeSaw {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the fake code-index did not run: %v", err)
	}
	var s fakeSaw
	mustJSON(t, string(data), &s)
	return s
}

type searchOut struct {
	Generation uint64         `json:"generation"`
	Native     []query.Symbol `json:"native"`
	CodeIndex  struct {
		Used   bool              `json:"used"`
		Reason string            `json:"reason"`
		Hits   []json.RawMessage `json:"hits"`
	} `json:"codeindex"`
}

func searchJSON(t *testing.T, root string, text ...string) searchOut {
	t.Helper()
	args := append([]string{"search", "--json", "--root", root, "--"}, text...)
	var out searchOut
	mustJSON(t, runOK(t, args...), &out)
	return out
}

func TestSearchWithoutCodeIndex(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := writeRoot(t, map[string]string{"a.sh": "deploy_all() { :; }\n"})
	out := searchJSON(t, root, "deploy")
	if out.CodeIndex.Used || out.CodeIndex.Reason == "" || out.CodeIndex.Hits == nil || len(out.CodeIndex.Hits) != 0 {
		t.Errorf("codeindex %+v: want used false with a reason and an empty hits list", out.CodeIndex)
	}
	if len(out.Native) != 1 || out.Native[0].Name != "deploy_all" || out.Native[0].Path != "a.sh" || out.Generation != 1 {
		t.Errorf("native %+v generation %d", out.Native, out.Generation)
	}
	text := runOK(t, "search", "--root", root, "deploy")
	wantLines(t, text, "a.sh:1:1  function  deploy_all\n", "code-index: not used: ")
}

func TestSearchPassesExactArgv(t *testing.T) {
	hits := `{"hits":[{"target_kind":"symbol","name":"deploy","path":"a.sh","line":1,"kind":"function","score":1.5}],"route":"fts"}`
	root := writeRoot(t, map[string]string{"a.sh": "deploy() { :; }\n"})
	for _, text := range []string{
		"deploy",
		"--mode=auto",
		"--mode auto",
		"-x --json",
		"two words and 'quotes' \"too\"",
		"$(touch pwned) `touch pwned2` ; touch pwned3",
		"--",
		"é世界",
	} {
		record := installFake(t, fakeBehavior{Stdout: hits})
		out := searchJSON(t, root, text)
		saw := readSaw(t, record)
		want := []string{"search", "--mode", "fts", "--lang", "bash", "--k", "20", "--json", "--", text}
		if strings.Join(saw.Argv, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("text %q: argv %q, want %q", text, saw.Argv, want)
		}
		if !out.CodeIndex.Used || out.CodeIndex.Reason != "" || len(out.CodeIndex.Hits) != 1 {
			t.Errorf("text %q: codeindex %+v", text, out.CodeIndex)
		}
		for _, f := range []string{"pwned", "pwned2", "pwned3"} {
			if _, err := os.Stat(filepath.Join(root, f)); err == nil {
				t.Errorf("text %q: reached a shell (%s exists)", text, f)
			}
		}
		// the same text given without --separator (several words are one text)
	}
	// words without -- are joined by one space and passed as one element
	record := installFake(t, fakeBehavior{Stdout: hits})
	runOK(t, "search", "--root", root, "plain", "words")
	if saw := readSaw(t, record); saw.Argv[len(saw.Argv)-1] != "plain words" || saw.Argv[len(saw.Argv)-2] != "--" {
		t.Errorf("argv %q", saw.Argv)
	}
}

// The program starts in the neutral directory, not in the workspace, with the
// filtered environment and the index named by CODE_INDEX_DB.
func TestSearchRunsNeutrallyWithFilteredEnvironment(t *testing.T) {
	t.Setenv("CODE_INDEX_DB", "")
	t.Setenv("GHCRTS", "-M1m")
	t.Setenv("WB_SECRET", "s3cret")
	root := writeRoot(t, map[string]string{"a.sh": "deploy() { :; }\n"})
	record := installFake(t, fakeBehavior{Stdout: `{"hits":[],"route":"fts"}`})
	if out := searchJSON(t, root, "deploy"); !out.CodeIndex.Used {
		t.Fatalf("%+v", out.CodeIndex)
	}
	saw := readSaw(t, record)
	if saw.Cwd != "/" {
		t.Errorf("working directory %q, want the neutral directory", saw.Cwd)
	}
	canon, _ := filepath.EvalSymlinks(root)
	env := strings.Join(saw.Env, "\n")
	if !strings.Contains(env, "CODE_INDEX_DB="+filepath.Join(canon, ".Wammy", "code.sqlite")) {
		t.Errorf("no CODE_INDEX_DB naming the workspace's index:\n%s", env)
	}
	for _, bad := range []string{"GHCRTS=", "WB_SECRET=", "BASH_LSP_CONFIG="} {
		if strings.Contains(env, bad) {
			t.Errorf("the program saw %s", bad)
		}
	}
	if !strings.Contains(env, "PATH=") {
		t.Errorf("no PATH:\n%s", env)
	}
	// the user's own CODE_INDEX_DB wins
	t.Setenv("CODE_INDEX_DB", filepath.Join(t.TempDir(), "mine.sqlite"))
	record = installFake(t, fakeBehavior{Stdout: `{"hits":[]}`})
	searchJSON(t, root, "deploy")
	if env := strings.Join(readSaw(t, record).Env, "\n"); !strings.Contains(env, "CODE_INDEX_DB="+os.Getenv("CODE_INDEX_DB")) {
		t.Errorf("the user's CODE_INDEX_DB was not passed on:\n%s", env)
	}
}

func TestSearchFailureIsAReason(t *testing.T) {
	root := writeRoot(t, map[string]string{"a.sh": "deploy() { :; }\n"})
	token := "ghp_" + strings.Repeat("Qz", 18)
	for _, c := range []struct {
		name string
		b    fakeBehavior
		want func(reason string) bool
	}{
		{"exit 1 with a message", fakeBehavior{Stderr: "no index for /x (looked for /x/.Wammy/code.sqlite); run `code-index index`\nsecond line\n", Exit: 1},
			func(r string) bool {
				return r == "no index for /x (looked for /x/.Wammy/code.sqlite); run `code-index index`"
			}},
		{"first non-empty line", fakeBehavior{Stderr: "\n\n  boom  \nlater\n", Exit: 3}, func(r string) bool { return r == "boom" }},
		{"silent failure", fakeBehavior{Exit: 7}, func(r string) bool { return r == "exit status 7" }},
		{"long line is cut", fakeBehavior{Stderr: strings.Repeat("é", 400) + "\n", Exit: 1}, func(r string) bool { return len(r) <= 200 && len(r) >= 190 && !strings.ContainsRune(r, '�') }},
		{"secret is redacted", fakeBehavior{Stderr: "bad TOKEN=" + token + " here\n", Exit: 1}, func(r string) bool {
			return strings.Contains(r, "[redacted]") && !strings.Contains(r, token)
		}},
		{"not JSON", fakeBehavior{Stdout: "hello"}, func(r string) bool { return strings.Contains(r, "not the expected JSON") }},
	} {
		installFake(t, c.b)
		out := searchJSON(t, root, "deploy")
		if out.CodeIndex.Used || !c.want(out.CodeIndex.Reason) || len(out.CodeIndex.Hits) != 0 {
			t.Errorf("%s: codeindex %+v", c.name, out.CodeIndex)
		}
		if len(out.Native) != 1 {
			t.Errorf("%s: the native answer went missing: %+v", c.name, out.Native)
		}
	}
}

// A code-index inside the workspace is never run, whether found through PATH
// or configured.
func TestSearchRefusesCodeIndexInsideTheRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in")
	}
	marker := filepath.Join(t.TempDir(), "ran")
	root := writeRoot(t, map[string]string{"a.sh": "deploy() { :; }\n"})
	prog := filepath.Join(root, "code-index")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\necho ran > "+marker+"\necho '{\"hits\":[]}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("[codeindex]\npath = \""+prog+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]string{"path": filepath.Join(t.TempDir(), "none.toml"), "configured": cfg} {
		t.Setenv("BASH_LSP_CONFIG", env)
		out := searchJSON(t, root, "deploy")
		if out.CodeIndex.Used || !strings.Contains(out.CodeIndex.Reason, "inside the workspace folder") {
			t.Errorf("%s: %+v", name, out.CodeIndex)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: the workspace's code-index ran", name)
		}
	}
}

func TestSearchUsage(t *testing.T) {
	root := writeRoot(t, map[string]string{"a.sh": ":\n"})
	for _, args := range [][]string{
		{"search"},
		{"search", "--root", root},
		{"search", "--json", "--root", root, "--"},
		{"search", "--root", root, "--mode=auto"},
		{"search", "--root"},
		{"search", "--root", root, "   "},
		{"search", "--root", root, strings.Repeat("x", query.MaxText+1)},
	} {
		if code, _, errs := runCode(t, args...); code != 2 || !strings.Contains(errs, "usage: bash-lsp search") {
			t.Errorf("%.60v: exit %d, stderr %.200q", args, code, errs)
		}
	}
	if code, _, errs := runCode(t, "search", "--root", filepath.Join(root, "missing"), "x"); code != 1 || errs == "" {
		t.Errorf("missing root: exit %d %q", code, errs)
	}
}

// The workspace is a directory below the repository root: code-index's own
// lookup finds the index at the repository root, so the program is told that
// database.
func TestSearchFindsIndexAboveTheWorkspaceRoot(t *testing.T) {
	t.Setenv("CODE_INDEX_DB", "")
	repo := writeRoot(t, map[string]string{".git/HEAD": "ref: x\n", "scripts/a.sh": "deploy() { :; }\n"})
	canon, _ := filepath.EvalSymlinks(repo)
	record := installFake(t, fakeBehavior{Stdout: `{"hits":[],"route":"fts"}`})
	out := searchJSON(t, filepath.Join(repo, "scripts"), "deploy")
	if !out.CodeIndex.Used {
		t.Fatalf("%+v", out.CodeIndex)
	}
	want := "CODE_INDEX_DB=" + filepath.Join(canon, ".Wammy", "code.sqlite")
	if env := strings.Join(readSaw(t, record).Env, "\n"); !strings.Contains(env, want) {
		t.Errorf("want %s in\n%s", want, env)
	}
}

// What the command line prints is redacted like what MCP returns.
func TestSearchRedactsOutput(t *testing.T) {
	root := writeRoot(t, map[string]string{"a.sh": "password=hunter2\ndeploy() { :; }\n"})
	hit := `{"hits":[{"name":"deploy","path":"a.sh","line":1,"kind":"function","summary":"uses password=hunter2 now","signature":"x TOKEN=abc"},{"name":"token=sekrit1234","path":"b.sh","line":2,"kind":"function"}],"route":"fts"}`
	installFake(t, fakeBehavior{Stdout: hit})
	for _, args := range [][]string{
		{"search", "--json", "--root", root, "--", "deploy"},
		{"search", "--json", "--root", root, "--", "password"},
		{"search", "--root", root, "--", "password"},
	} {
		out := runOK(t, args...)
		if strings.Contains(out, "hunter2") || strings.Contains(out, "TOKEN=abc") || strings.Contains(out, "sekrit1234") {
			t.Errorf("%v: unredacted output:\n%s", args, out)
		}
	}
	if out := runOK(t, "search", "--json", "--root", root, "--", "deploy"); !strings.Contains(out, "password=[redacted]") {
		t.Errorf("redaction left no mark:\n%s", out)
	}
}

// A repo setting that is ignored because only user settings may carry it is
// said on stderr, by search and by mcp.
func TestRefusedRepoSettingsAreNoticed(t *testing.T) {
	root := writeRoot(t, map[string]string{"a.sh": ":\n", ".bash-lsp.toml": "[codeindex]\npath = \"/tmp/evil\"\n"})
	_, _, errs := runCode(t, "search", "--root", root, "--", "x")
	if !strings.Contains(errs, "codeindex.path") || !strings.Contains(errs, "ignored") {
		t.Errorf("search stderr %q", errs)
	}
	var out, errb bytes.Buffer
	run([]string{"mcp", "--root", root}, strings.NewReader(""), &out, &errb)
	if !strings.Contains(errb.String(), "codeindex.path") || !strings.Contains(errb.String(), "ignored") {
		t.Errorf("mcp stderr %q", errb.String())
	}
}
