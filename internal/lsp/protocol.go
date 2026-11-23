package lsp

// Only the protocol structures milestone 1 uses.

// Position is a 0-based line and a column in UTF-16 code units.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open range of Positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range in a document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type textDocumentItem struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
	Text    string `json:"text"`
}

type versionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// contentChange is one edit; a nil Range replaces the whole text.
type contentChange struct {
	Range *Range `json:"range"`
	Text  string `json:"text"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type didChangeParams struct {
	TextDocument   versionedTextDocumentIdentifier `json:"textDocument"`
	ContentChanges []contentChange                 `json:"contentChanges"`
}

type didCloseParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type textDocumentPositionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

type referenceParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	Context      struct {
		IncludeDeclaration bool `json:"includeDeclaration"`
	} `json:"context"`
}

type documentSymbolParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type workspaceSymbolParams struct {
	Query string `json:"query"`
}

// DocumentSymbol is the hierarchical outline entry.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// SymbolInformation is the flat outline and workspace-symbol entry.
type SymbolInformation struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
}

// Symbol kinds (LSP SymbolKind).
const (
	kindFunction = 12
	kindVariable = 13
)

type markupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type hover struct {
	Contents markupContent `json:"contents"`
	Range    Range         `json:"range"`
}

// Diagnostic is a pushed problem report.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"`
	Code     string `json:"code"`
	Source   string `json:"source"`
	Message  string `json:"message"`
}

type publishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type initializeParams struct {
	RootURI          *string           `json:"rootUri"`
	RootPath         *string           `json:"rootPath"`
	WorkspaceFolders []workspaceFolder `json:"workspaceFolders"`
	Capabilities     struct {
		TextDocument struct {
			DocumentSymbol struct {
				Hierarchical bool `json:"hierarchicalDocumentSymbolSupport"`
			} `json:"documentSymbol"`
		} `json:"textDocument"`
	} `json:"capabilities"`
}

type didChangeWatchedFilesParams struct {
	Changes []struct {
		URI string `json:"uri"`
	} `json:"changes"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	Capabilities map[string]any `json:"capabilities"`
	ServerInfo   serverInfo     `json:"serverInfo"`
}

func capabilities() map[string]any {
	return map[string]any{
		"positionEncoding":        "utf-16",
		"textDocumentSync":        map[string]any{"openClose": true, "change": 2},
		"definitionProvider":      true,
		"referencesProvider":      true,
		"implementationProvider":  true,
		"hoverProvider":           true,
		"documentSymbolProvider":  true,
		"workspaceSymbolProvider": true,
	}
}
