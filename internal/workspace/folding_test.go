package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func forceFold(t *testing.T, fold bool) {
	t.Helper()
	old := detectFold
	detectFold = func(string) bool { return fold }
	t.Cleanup(func() { detectFold = old })
}

// countReadEntries counts the directory entries readDirN returns for the
// duration of a test.
func countReadEntries(t *testing.T) *int {
	t.Helper()
	n := new(int)
	old := readDirN
	readDirN = func(d string, max int) ([]os.DirEntry, error) {
		ents, err := old(d, max)
		*n += len(ents)
		return ents, err
	}
	t.Cleanup(func() { readDirN = old })
	return n
}

func TestGitignoreFoldsOnFoldingVolume(t *testing.T) {
	for _, fold := range []bool{true, false} {
		forceFold(t, fold)
		root := t.TempDir()
		write(t, filepath.Join(root, ".gitignore"), "Build/\n")
		write(t, filepath.Join(root, "build", "x.sh"), "x() { :; }\n")
		write(t, filepath.Join(root, "src", "a.sh"), "a() { :; }\n")
		w := open(t, root)
		want := []string{"build/x.sh", "src/a.sh"}
		if fold {
			want = []string{"src/a.sh"}
		}
		if got := rels(w); !slices.Equal(got, want) {
			t.Fatalf("fold=%v: listed %v, want %v", fold, got, want)
		}
		if fold { // without folding, BUILD/x.sh on a folding disk would be a second spelling
			w.Refresh(filepath.Join(w.Root(), "BUILD", "x.sh"))
			if got := rels(w); !slices.Equal(got, want) {
				t.Fatalf("after Refresh(BUILD/x.sh): %v, want %v", got, want)
			}
		}
	}
}

func TestIncludeExcludeFold(t *testing.T) {
	for _, fold := range []bool{true, false} {
		forceFold(t, fold)
		root := t.TempDir()
		write(t, filepath.Join(root, "vendor", "a.sh"), "v() { :; }\n")
		write(t, filepath.Join(root, "src", "b.sh"), "b() { :; }\n")
		w := openWith(t, root, Limits{}, Config{Exclude: []string{"Vendor/"}})
		want := []string{"src/b.sh", "vendor/a.sh"}
		if fold {
			want = []string{"src/b.sh"}
		}
		if got := rels(w); !slices.Equal(got, want) {
			t.Fatalf("exclude, fold=%v: %v, want %v", fold, got, want)
		}
		w = openWith(t, root, Limits{}, Config{Include: []string{"SRC/"}})
		want = []string{}
		if fold {
			want = []string{"src/b.sh"}
		}
		if got := rels(w); !slices.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Fatalf("include, fold=%v: %v, want %v", fold, got, want)
		}
	}
}

func makeBigDir(t *testing.T, root string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		write(t, filepath.Join(root, "d", fmt.Sprintf("f%d.sh", i)), ":\n")
	}
}

func foldLines(w *Workspace) []string {
	var out []string
	for _, l := range w.Report().LimitsHit {
		if strings.HasPrefix(l, "case-folded spelling") {
			out = append(out, l)
		}
	}
	return out
}

func TestFoldedHugeDirectoryNotListed(t *testing.T) {
	forceFold(t, true)
	root := t.TempDir()
	makeBigDir(t, root, maxFoldListing+1)
	w := open(t, root)
	n := countReadEntries(t)
	for i := 0; i < 100; i++ {
		write(t, filepath.Join(root, "d", fmt.Sprintf("n%d.sh", i)), ":\n") // the directory's mtime changes
		w.Refresh(filepath.Join(w.Root(), "d", fmt.Sprintf("N%d.SH", i)))
	}
	if *n > 1100 {
		t.Fatalf("%d entries listed over 100 events; want at most 1100", *n)
	}
	want := "case-folded spelling not corrected in d: more than 1024 entries"
	if got := foldLines(w); !slices.Equal(got, []string{want}) {
		t.Fatalf("fold limit lines = %q, want exactly %q", got, want)
	}
	// Kept until the next full walk, which lists again.
	w.Refresh()
	w.Refresh(filepath.Join(w.Root(), "d", "F1.SH"))
	if got := foldLines(w); len(got) != 1 {
		t.Fatalf("after a full walk and one more event: %q", got)
	}
}

func TestFoldedListingAtCapIsListed(t *testing.T) {
	forceFold(t, true)
	root := t.TempDir()
	makeBigDir(t, root, maxFoldListing)
	w := open(t, root)
	w.Refresh(filepath.Join(w.Root(), "D", "F7.SH"))
	if !slices.Contains(rels(w), "d/f7.sh") {
		t.Fatalf("d/f7.sh missing: the event spelling replaced the disk's")
	}
	for _, p := range rels(w) {
		if strings.ContainsAny(p, "DF") {
			t.Fatalf("event spelling leaked into %q", p)
		}
	}
	if got := foldLines(w); len(got) != 0 {
		t.Fatalf("limit line for a directory at the cap: %q", got)
	}
}

// A cached listing is reused only while the directory's modification time is a
// trustworthy sign of change. Where it is not (the listingByMtime switch is
// off), the directory is read again: a rename that leaves the recorded time
// as it was must still be seen.
func TestListingCacheDoesNotTrustAnUnchangedMtimeWhereItIsUnreliable(t *testing.T) {
	def := listingByMtime
	defer func() { listingByMtime = def }()
	for _, byMtime := range []bool{true, false} {
		listingByMtime = byMtime
		root := t.TempDir()
		write(t, filepath.Join(root, "d", "a.sh"), ":\n")
		w := open(t, root)
		dir := filepath.Join(w.Root(), "d")
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		w.mu.Lock()
		before := w.names(dir)
		w.mu.Unlock()
		if before["a.sh"] != "a.sh" {
			t.Fatalf("first listing: %v", before)
		}
		if err := os.Rename(filepath.Join(dir, "a.sh"), filepath.Join(dir, "B.sh")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, fi.ModTime(), fi.ModTime()); err != nil { // the time did not move
			t.Fatal(err)
		}
		w.mu.Lock()
		after := w.names(dir)
		w.mu.Unlock()
		if got := after["b.sh"]; byMtime && got != "" || !byMtime && got != "B.sh" {
			t.Errorf("listingByMtime=%v: after the rename the listing is %v", byMtime, after)
		}
	}
	if windows && def {
		t.Error("directory modification times are trusted on Windows")
	}
}
