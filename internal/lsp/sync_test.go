package lsp

import "testing"

func TestApplyChange(t *testing.T) {
	r := func(sl, sc, el, ec int) *Range {
		return &Range{Position{sl, sc}, Position{el, ec}}
	}
	tests := []struct {
		name string
		text string
		ch   contentChange
		want string
	}{
		{"full replace", "abc\n", contentChange{Text: "x"}, "x"},
		{"replace within line", "f() { :; }\nf\n", contentChange{Range: r(0, 0, 0, 1), Text: "g"}, "g() { :; }\nf\n"},
		{"insert at EOF", "a\nb", contentChange{Range: r(1, 1, 1, 1), Text: "\nc"}, "a\nb\nc"},
		{"insert at EOF after final newline", "a\n", contentChange{Range: r(1, 0, 1, 0), Text: "b\n"}, "a\nb\n"},
		{"range past EOF clamps", "a\n", contentChange{Range: r(9, 9, 9, 9), Text: "z"}, "a\nz"},
		{"delete across CRLF", "ab\r\ncd\r\n", contentChange{Range: r(0, 2, 1, 0), Text: ""}, "abcd\r\n"},
		{"column past line end clamps before CR", "ab\r\ncd", contentChange{Range: r(0, 99, 0, 99), Text: "X"}, "abX\r\ncd"},
		{"edit after surrogate pair", "\U0001F600x\n", contentChange{Range: r(0, 2, 0, 3), Text: "y"}, "\U0001F600y\n"},
		{"edit after BMP multibyte", "éx\n", contentChange{Range: r(0, 1, 0, 2), Text: "y"}, "éy\n"},
		{"column inside a surrogate pair clamps down", "\U0001F600x", contentChange{Range: r(0, 1, 0, 1), Text: "-"}, "-\U0001F600x"},
		{"delete whole line", "a\nb\nc\n", contentChange{Range: r(1, 0, 2, 0), Text: ""}, "a\nc\n"},
		{"inverted range becomes insert", "abc", contentChange{Range: r(0, 2, 0, 1), Text: "X"}, "abXc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(applyChange([]byte(tc.text), tc.ch)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplyChangesSequential(t *testing.T) {
	// The second range is in the text produced by the first edit.
	got := applyChanges([]byte("one\ntwo\n"), []contentChange{
		{Range: &Range{Position{0, 0}, Position{0, 3}}, Text: "uno\nand"},
		{Range: &Range{Position{2, 0}, Position{2, 3}}, Text: "dos"},
	})
	if string(got) != "uno\nand\ndos\n" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyChangeLoneCR(t *testing.T) {
	r := func(sl, sc, el, ec int) *Range {
		return &Range{Position{sl, sc}, Position{el, ec}}
	}
	tests := []struct {
		name string
		text string
		ch   contentChange
		want string
	}{
		{"edit on the line after a lone CR", "ab\rcd\r", contentChange{Range: r(1, 0, 1, 1), Text: "X"}, "ab\rXd\r"},
		{"column past line end clamps before a lone CR", "ab\rcd", contentChange{Range: r(0, 99, 0, 99), Text: "X"}, "abX\rcd"},
		{"delete across a lone CR", "ab\rcd", contentChange{Range: r(0, 2, 1, 0), Text: ""}, "abcd"},
		{"insert on the empty line after a trailing CR", "a\r", contentChange{Range: r(1, 0, 1, 0), Text: "b"}, "a\rb"},
		{"mixed breaks", "a\nb\r\nc\rd", contentChange{Range: r(3, 0, 3, 1), Text: "Z"}, "a\nb\r\nc\rZ"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(applyChange([]byte(tc.text), tc.ch)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Incremental edits (sync kind 2) on a buffer with a lone CR: the LSP counts
// it as a line break, so lines after it are one further on than the tools
// count them.
func TestApplyChangeAroundLoneCR(t *testing.T) {
	text := "echo a\r# mid\rb=1\nrm $b\n"
	r := func(sl, sc, el, ec int) *Range { return &Range{Position{sl, sc}, Position{el, ec}} }
	tests := []struct {
		name string
		ch   contentChange
		want string
	}{
		{"before the CR", contentChange{Range: r(0, 5, 0, 6), Text: "Z"}, "echo Z\r# mid\rb=1\nrm $b\n"},
		{"on the line ending in CR", contentChange{Range: r(1, 2, 1, 5), Text: "MID"}, "echo a\r# MID\rb=1\nrm $b\n"},
		{"after the CR", contentChange{Range: r(2, 0, 2, 1), Text: "c"}, "echo a\r# mid\rc=1\nrm $b\n"},
		{"two lines after", contentChange{Range: r(3, 3, 3, 5), Text: "$c"}, "echo a\r# mid\rb=1\nrm $c\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(applyChange([]byte(text), tc.ch)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	got := applyChanges([]byte(text), []contentChange{
		{Range: r(0, 5, 0, 6), Text: "Z"}, {Range: r(2, 0, 2, 1), Text: "c"}, {Range: r(3, 3, 3, 5), Text: "$c"},
	})
	if string(got) != "echo Z\r# mid\rc=1\nrm $c\n" {
		t.Fatalf("sequential: %q", got)
	}
}
