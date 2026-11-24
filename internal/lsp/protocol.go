package lsp

import "encoding/json"

// Only the protocol structures the server uses.

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
	kindFile     = 1
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

// Diagnostic is a pushed problem report. Tags holds DiagnosticTag values
// (1 = Unnecessary).
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"`
	Code     string `json:"code"`
	Source   string `json:"source"`
	Message  string `json:"message"`
	Tags     []int  `json:"tags,omitempty"`
}

// showMessageParams are the parameters of window/showMessage. Type is an
// LSP MessageType (1 Error, 2 Warning, 3 Info).
type showMessageParams struct {
	Type    int    `json:"type"`
	Message string `json:"message"`
}

// publishDiagnosticsParams is PublishDiagnosticsParams: version is the
// document version the findings were computed for (omitted when the document
// is closed and its findings cleared).
type publishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Version     *int         `json:"version,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// logMessageParams are the parameters of window/logMessage (MessageType as in
// showMessageParams).
type logMessageParams struct {
	Type    int    `json:"type"`
	Message string `json:"message"`
}

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type initializeParams struct {
	RootURI          *string           `json:"rootUri"`
	RootPath         *string           `json:"rootPath"`
	WorkspaceFolders []workspaceFolder `json:"workspaceFolders"`
	// InitializationOptions is read leniently (see resolvedOnlyOption): a
	// malformed value must not fail initialize.
	InitializationOptions json.RawMessage `json:"initializationOptions"`
	Capabilities          struct {
		TextDocument struct {
			DocumentSymbol struct {
				Hierarchical bool `json:"hierarchicalDocumentSymbolSupport"`
			} `json:"documentSymbol"`
			Rename struct {
				PrepareSupport bool `json:"prepareSupport"`
			} `json:"rename"`
			SignatureHelp struct {
				SignatureInformation struct {
					ParameterInformation struct {
						LabelOffsetSupport bool `json:"labelOffsetSupport"`
					} `json:"parameterInformation"`
				} `json:"signatureInformation"`
			} `json:"signatureHelp"`
			FoldingRange struct {
				RangeLimit      int  `json:"rangeLimit"`
				LineFoldingOnly bool `json:"lineFoldingOnly"`
			} `json:"foldingRange"`
		} `json:"textDocument"`
		// ClientCapabilities.workspace (WorkspaceClientCapabilities)
		// .didChangeWatchedFiles (DidChangeWatchedFilesClientCapabilities)
		// .dynamicRegistration; see the watched-files section below.
		Workspace struct {
			DidChangeWatchedFiles struct {
				DynamicRegistration bool `json:"dynamicRegistration"`
			} `json:"didChangeWatchedFiles"`
			WorkspaceEdit struct {
				DocumentChanges         bool            `json:"documentChanges"`
				ChangeAnnotationSupport json.RawMessage `json:"changeAnnotationSupport"`
			} `json:"workspaceEdit"`
			SemanticTokens struct {
				RefreshSupport bool `json:"refreshSupport"`
			} `json:"semanticTokens"`
			InlayHint struct {
				RefreshSupport bool `json:"refreshSupport"`
			} `json:"inlayHint"`
		} `json:"workspace"`
	} `json:"capabilities"`
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
		"positionEncoding": "utf-16",
		// TextDocumentSyncOptions.save (SaveOptions): without it a client
		// sends no didSave.
		"textDocumentSync": map[string]any{"openClose": true, "change": 2, "save": map[string]any{"includeText": false}},
		// ServerCapabilities.workspace (WorkspaceOptions).workspaceFolders
		// (WorkspaceFoldersServerCapabilities), in the form gopls sends.
		"workspace": map[string]any{"workspaceFolders": map[string]any{
			"supported": true, "changeNotifications": "workspace/didChangeWorkspaceFolders"}},
		"definitionProvider":              true,
		"referencesProvider":              true,
		"implementationProvider":          true,
		"hoverProvider":                   true,
		"documentSymbolProvider":          true,
		"workspaceSymbolProvider":         true,
		"callHierarchyProvider":           true,
		"diagnosticProvider":              map[string]any{"interFileDependencies": true, "workspaceDiagnostics": false},
		"codeActionProvider":              map[string]any{"codeActionKinds": []string{"quickfix"}},
		"documentFormattingProvider":      true,
		"documentRangeFormattingProvider": true,
		"foldingRangeProvider":            true,
		"selectionRangeProvider":          true,
		"documentHighlightProvider":       true,
		"documentLinkProvider":            map[string]any{"resolveProvider": false},
	}
}

// ---- call hierarchy ----
//
// Field names as in LSP 3.17 (all @since 3.16.0), copied from the generated
// protocol of gopls v0.24.0 (golang.org/x/tools/gopls/internal/protocol/
// tsprotocol.go, generated from the vscode-languageserver-node metaModel at
// release/protocol/3.18.2, in which these types are unchanged since 3.16):
// CallHierarchyItem, CallHierarchyIncomingCall, CallHierarchyOutgoingCall,
// CallHierarchyPrepareParams (= TextDocumentPositionParams plus
// workDoneToken), CallHierarchyIncomingCallsParams and
// CallHierarchyOutgoingCallsParams (item plus work-done and partial-result
// tokens, which this server ignores), and the capability
// `callHierarchyProvider`. SymbolTag has one value (Deprecated = 1), so tags
// cannot carry a resolution label: it goes into detail.

// callHierarchyItem is CallHierarchyItem. Data is preserved by the client
// between prepare and the incoming/outgoing requests; this server puts an
// itemData in it.
type callHierarchyItem struct {
	Name           string          `json:"name"`
	Kind           int             `json:"kind"`
	Tags           []int           `json:"tags,omitempty"`
	Detail         string          `json:"detail,omitempty"`
	URI            string          `json:"uri"`
	Range          Range           `json:"range"`
	SelectionRange Range           `json:"selectionRange"`
	Data           json.RawMessage `json:"data,omitempty"`
}

// callHierarchyIncomingCall is CallHierarchyIncomingCall: fromRanges are
// the call sites, in the document of From.
type callHierarchyIncomingCall struct {
	From       callHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

// callHierarchyOutgoingCall is CallHierarchyOutgoingCall: fromRanges are
// the call sites in the document of the item the request was made for.
type callHierarchyOutgoingCall struct {
	To         callHierarchyItem `json:"to"`
	FromRanges []Range           `json:"fromRanges"`
}

// callHierarchyCallsParams is CallHierarchyIncomingCallsParams and
// CallHierarchyOutgoingCallsParams (same shape).
type callHierarchyCallsParams struct {
	Item callHierarchyItem `json:"item"`
}

// ---- watched files, save, workspace folders ----
//
// Field names as in LSP 3.17, copied from the generated protocol of gopls
// v0.24.0 (golang.org/x/tools/gopls/internal/protocol/tsprotocol.go, from
// the vscode-languageserver-node metaModel; these types are unchanged in
// 3.18): DidChangeWatchedFilesClientCapabilities, DidChangeWatchedFilesParams,
// FileEvent, FileChangeType (Created 1, Changed 2, Deleted 3),
// DidSaveTextDocumentParams, DidChangeWorkspaceFoldersParams,
// WorkspaceFoldersChangeEvent, WorkspaceFolder, RegistrationParams,
// Registration, DidChangeWatchedFilesRegistrationOptions and
// FileSystemWatcher (globPattern; kind omitted means Create|Change|Delete).
// The request is client/registerCapability and the notifications are
// workspace/didChangeWatchedFiles, textDocument/didSave and
// workspace/didChangeWorkspaceFolders (gopls tsclient.go, tsserver.go).

// fileEvent is FileEvent. Type is not used: the state on disk decides.
type fileEvent struct {
	URI  string `json:"uri"`
	Type int    `json:"type"`
}

type didChangeWatchedFilesParams struct {
	Changes []fileEvent `json:"changes"`
}

// didSaveTextDocumentParams is DidSaveTextDocumentParams; text is not
// requested (includeText false) and not used.
type didSaveTextDocumentParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type workspaceFoldersChangeEvent struct {
	Added   []workspaceFolder `json:"added"`
	Removed []workspaceFolder `json:"removed"`
}

type didChangeWorkspaceFoldersParams struct {
	Event workspaceFoldersChangeEvent `json:"event"`
}

// fileSystemWatcher is FileSystemWatcher with a pattern string (a
// GlobPattern is a Pattern string or a RelativePattern).
type fileSystemWatcher struct {
	GlobPattern string `json:"globPattern"`
}

type didChangeWatchedFilesRegistrationOptions struct {
	Watchers []fileSystemWatcher `json:"watchers"`
}

type registration struct {
	ID              string `json:"id"`
	Method          string `json:"method"`
	RegisterOptions any    `json:"registerOptions,omitempty"`
}

type registrationParams struct {
	Registrations []registration `json:"registrations"`
}

// ---- pull diagnostics, code actions, formatting ----
//
// Field names as in LSP 3.17 (textDocument/diagnostic: DocumentDiagnosticParams,
// RelatedFullDocumentDiagnosticReport, RelatedUnchangedDocumentDiagnosticReport,
// DiagnosticOptions; textDocument/codeAction: CodeActionParams, CodeActionContext,
// CodeAction, WorkspaceEdit, CodeActionOptions; textDocument/formatting and
// rangeFormatting: DocumentFormattingParams, DocumentRangeFormattingParams,
// FormattingOptions, TextEdit). Work-done and partial-result tokens, the
// identifier of a pull and the client's `triggerKind` are not used.

type documentDiagnosticParams struct {
	TextDocument     textDocumentIdentifier `json:"textDocument"`
	PreviousResultID string                 `json:"previousResultId"`
}

// fullDiagnosticReport is the `full` kind of a document diagnostic report.
type fullDiagnosticReport struct {
	Kind     string       `json:"kind"`
	ResultID string       `json:"resultId"`
	Items    []Diagnostic `json:"items"`
}

// unchangedDiagnosticReport is the `unchanged` kind.
type unchangedDiagnosticReport struct {
	Kind     string `json:"kind"`
	ResultID string `json:"resultId"`
}

// TextEdit replaces Range with NewText.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

type codeActionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Range        Range                  `json:"range"`
	Context      struct {
		Diagnostics []Diagnostic `json:"diagnostics"`
		Only        []string     `json:"only"`
	} `json:"context"`
}

type workspaceEdit struct {
	Changes map[string][]TextEdit `json:"changes"`
}

// CodeAction is a quick fix. IsPreferred is always sent.
type CodeAction struct {
	Title       string        `json:"title"`
	Kind        string        `json:"kind"`
	Diagnostics []Diagnostic  `json:"diagnostics,omitempty"`
	IsPreferred bool          `json:"isPreferred"`
	Edit        workspaceEdit `json:"edit"`
}

type formattingOptions struct {
	TabSize      int  `json:"tabSize"`
	InsertSpaces bool `json:"insertSpaces"`
}

type documentFormattingParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Options      formattingOptions      `json:"options"`
}

type documentRangeFormattingParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Range        Range                  `json:"range"`
	Options      formattingOptions      `json:"options"`
}
