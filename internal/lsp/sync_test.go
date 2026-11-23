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
