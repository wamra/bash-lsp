package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/wamra/bash-lsp/analysis"
)

type sigHelp struct {
	Signatures []struct {
		Label         string         `json:"label"`
		Documentation *markupContent `json:"documentation"`
		Parameters    []struct {
			Label json.RawMessage `json:"label"`
		} `json:"parameters"`
	} `json:"signatures"`
	ActiveSignature int `json:"activeSignature"`
	ActiveParameter int `json:"activeParameter"`
}

// sigAtMark opens src (cursor = the ▮ mark, removed) as a.sh and asks for
// signature help there. The result is nil when the server answered null.
func (c *testClient) sigAtMark(src string) (*sigHelp, string) {
	c.t.Helper()
	before, after, ok := strings.Cut(src, "▮")
	if !ok {
		c.t.Fatal("no cursor mark")
	}
	c.openText("a.sh", before+after)
	line := strings.Count(before, "\n")
	col := before[strings.LastIndex(before, "\n")+1:]
	p := Position{Line: line, Character: len(utf16.Encode([]rune(col)))}
	raw := c.call("textDocument/signatureHelp", map[string]any{
		"textDocument": map[string]any{"uri": c.uri("a.sh")}, "position": p})
	if string(raw) == "null" {
		return nil, string(raw)
	}
	var h sigHelp
	if err := json.Unmarshal(raw, &h); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	return &h, string(raw)
}

func TestSignatureHelpCapability(t *testing.T) {
	caps := capabilities()
	got, _ := json.Marshal(caps["signatureHelpProvider"])
	if string(got) != `{"retriggerCharacters":[" "],"triggerCharacters":[" "]}` {
		t.Fatalf("signatureHelpProvider = %s", got)
	}
}

func TestSignatureHelpStringLabels(t *testing.T) {
	c := client(t, t.TempDir())
	h, raw := c.sigAtMark("# Copy a file.\ncp2() { local src=\"$1\" dst=\"$2\"; }\ncp2 a ▮")
	if h == nil || len(h.Signatures) != 1 {
		t.Fatalf("got %s", raw)
	}
	s := h.Signatures[0]
	if s.Label != "cp2 src dst" || h.ActiveParameter != 1 || h.ActiveSignature != 0 {
		t.Fatalf("got %s", raw)
	}
	if s.Documentation == nil || s.Documentation.Value != "Copy a file." {
		t.Fatalf("doc: %s", raw)
	}
	var labels []string
	for _, p := range s.Parameters {
		var l string
		if err := json.Unmarshal(p.Label, &l); err != nil {
			t.Fatalf("not a string label: %s", p.Label)
		}
		labels = append(labels, l)
	}
	if strings.Join(labels, ",") != "src,dst" {
		t.Fatalf("labels %v", labels)
	}
}

func TestSignatureHelpLabelOffsets(t *testing.T) {
	var srv *server
	c := clientWith(t, clientCfg{root: t.TempDir(), onServer: func(s *server) { srv = s }})
	srv.ccaps.LabelOffset = true
	h, raw := c.sigAtMark("f() { echo \"$1\" \"$2\" \"$@\"; }\nf a b c d ▮")
	if h == nil {
		t.Fatalf("got %s", raw)
	}
	s := h.Signatures[0]
	if s.Label != "f $1 $2 [args...]" || h.ActiveParameter != 2 {
		t.Fatalf("got %s", raw)
	}
	var got []string
	for _, p := range s.Parameters {
		var o [2]int
		if err := json.Unmarshal(p.Label, &o); err != nil {
			t.Fatalf("not an offset pair: %s", p.Label)
		}
		got = append(got, s.Label[o[0]:o[1]])
	}
	if strings.Join(got, "|") != "$1|$2|[args...]" {
		t.Fatalf("offsets pick out %v in %s", got, raw)
	}
}

func TestSignatureHelpOptionsDoc(t *testing.T) {
	c := client(t, t.TempDir())
	h, raw := c.sigAtMark("# Parse flags.\nopts() { while getopts \"ab:\" o; do :; done; }\nopts ▮")
	if h == nil || h.Signatures[0].Documentation == nil {
		t.Fatalf("got %s", raw)
	}
	if v := h.Signatures[0].Documentation.Value; v != "Parse flags.\n\noptions: -a -b ARG" {
		t.Fatalf("doc %q", v)
	}
	h, raw = c.sigAtMark("opts() { while getopts \"ab:\" o; do :; done; }\nopts ▮")
	if h == nil || h.Signatures[0].Documentation == nil || h.Signatures[0].Documentation.Value != "options: -a -b ARG" {
		t.Fatalf("doc without comment: %s", raw)
	}
}

func TestSignatureHelpDocRedactedPlaintext(t *testing.T) {
	c := client(t, t.TempDir())
	h, raw := c.sigAtMark("# Deploy with password=hunter2 and AKIAABCDEFGHIJKLMNOP.\nf() { echo \"$1\"; }\nf ▮")
	if h == nil || h.Signatures[0].Documentation == nil {
		t.Fatalf("got %s", raw)
	}
	d := h.Signatures[0].Documentation
	if d.Kind != "plaintext" || strings.Contains(d.Value, "hunter2") || strings.Contains(d.Value, "AKIAABCDEFGHIJKLMNOP") || !strings.Contains(d.Value, "[redacted]") {
		t.Fatalf("doc %+v", d)
	}
}

func TestSignatureHelpVariadicClamped(t *testing.T) {
	c := client(t, t.TempDir())
	h, raw := c.sigAtMark("f() { echo \"$1\" \"$@\"; }; f a b c ▮")
	if h == nil || h.ActiveParameter != 1 || len(h.Signatures[0].Parameters) != 2 {
		t.Fatalf("got %s", raw)
	}
}

func TestSignatureHelpNone(t *testing.T) {
	c := client(t, t.TempDir())
	for name, src := range map[string]string{
		"on the name": "f() { :; }\nf▮ a",
		"builtin":     "echo ▮",
		"no call":     "f() { :; }\n▮",
	} {
		if h, raw := c.sigAtMark(src); h != nil {
			t.Errorf("%s: got %s", name, raw)
		}
	}
}

func TestSignatureHelpCorpus(t *testing.T) {
	root := copyCorpus(t)
	c := client(t, root)
	c.open("main.sh")
	b, _ := os.ReadFile(filepath.Join(root, "main.sh"))
	text := string(b)
	i := strings.Index(text, "deploy app")
	before := text[:i+len("deploy ")]
	p := Position{Line: strings.Count(before, "\n"), Character: len("deploy ")}
	raw := c.call("textDocument/signatureHelp", map[string]any{
		"textDocument": map[string]any{"uri": c.uri("main.sh")}, "position": p})
	var h sigHelp
	if err := json.Unmarshal(raw, &h); err != nil || len(h.Signatures) != 1 {
		t.Fatalf("%v: %s", err, raw)
	}
	if h.Signatures[0].Label != "deploy name" || h.ActiveParameter != 0 {
		t.Fatalf("got %s", raw)
	}
}

func TestSignatureUTF16(t *testing.T) {
	c := client(t, t.TempDir())
	// An emoji (a surrogate pair) before the call on the same line: the
	// cursor column is in UTF-16 units.
	h, raw := c.sigAtMark("f() { echo \"$1\" \"$2\"; }\necho \"😀😀\"; f 😀 ▮")
	if h == nil || h.Signatures[0].Label != "f $1 $2" || h.ActiveParameter != 1 {
		t.Fatalf("got %s", raw)
	}
	// Offsets count UTF-16 units in the label too: parameter names with
	// non-BMP and non-ASCII letters.
	s := &server{}
	s.ccaps.LabelOffset = true
	sig := analysis.FuncSignature{Name: "fé", Label: "fé é😀x $2 [args...]", Variadic: true,
		Params: []analysis.Param{{Index: 1, Name: "é😀x"}, {Index: 2}}}
	info := s.signatureInfo(sig)
	units := utf16.Encode([]rune(sig.Label))
	var got []string
	for _, p := range info.Parameters {
		o := p.Label.([2]int)
		got = append(got, string(utf16.Decode(units[o[0]:o[1]])))
	}
	if strings.Join(got, "|") != "é😀x|$2|[args...]" {
		t.Fatalf("offsets pick out %v", got)
	}
}
