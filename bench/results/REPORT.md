Server cells: plain corpus; "x / y" where the annotated corpus (`# shellcheck source=` directives) differs.
Last column is not a server: it says whether the mvdan/sh tree alone contains the facts the case needs ("facts" = yes, "MISSING" = no). Resolving them is the layer bash-lsp adds.

| Case | What is tested | bash-language-server 5.8.1 | shuck 0.2.11 | bashd 0.4.0 | bash-lsp 0.1.0 | mvdan/sh 3.14.1, parser only |
|---|---|---|---|---|---|---|
| C13 | first documentSymbol right after didOpen is non-empty | FAIL | pass | pass | pass | facts |
| C01 | definition across a static `source ./lib/util.sh` | pass | FAIL | pass | pass | facts |
| C01d | definition across `source "$HERE/lib/util.sh"` (HERE=$(dirname $0)) | pass | FAIL / pass | FAIL | pass | facts |
| C02 | definition through alias reaches the function | FAIL | FAIL | FAIL | pass | facts |
| C03 | definition of function named in trap string | FAIL | FAIL | FAIL | FAIL | facts |
| C04 | references exclude same-named function in unrelated orphan.sh | FAIL | FAIL | pass | pass | facts |
| C04b | references include the definition and the direct call | pass | FAIL | pass | pass | facts |
| C05 | references include the call through alias br | FAIL | FAIL | FAIL | pass | facts |
| C06 | references include cmd=build_release / "$cmd" | FAIL | FAIL | FAIL | FAIL | facts |
| C07 | references include eval "build_release y" | FAIL | FAIL | FAIL | FAIL | facts |
| C08 | references of a local variable stay inside its function | FAIL | pass | FAIL / pass | pass | facts |
| C09 | prepareCallHierarchy returns an item | FAIL | pass | FAIL | FAIL | facts |
| C10 | incoming calls of build_release include deploy | FAIL | pass | FAIL | FAIL | facts |
| C11 | outgoing calls of deploy include build_release | FAIL | pass | FAIL | FAIL | facts |
| C12 | document symbols are hierarchical (locals nested in function) | FAIL | pass | pass | pass | facts |
| C14 | hover at a function definition is non-empty | FAIL | pass | pass | pass | facts |
| C14b | hover shows the doc comment above the function | FAIL | FAIL | FAIL | pass | facts |
| C24 | rename of a local variable edits only its function | pass | pass | FAIL / pass | FAIL | facts |
| C28 | diagnostic on unquoted `rm $f` | pass | FAIL | pass | FAIL | facts |
| C29 | source path resolves to lib/util.sh (definition or document link) | pass | pass | pass | pass | facts |
| C25 | call inside orphan.sh resolves to its own definition only | pass | pass | pass | pass | facts |
| C15 | library call resolves to a function defined by the sourcing script | FAIL | FAIL | FAIL | pass | facts |
| C16 | references from the script find the call inside the library | pass | FAIL | pass | pass | facts |
| C17 | definition through a glob loader (for f in plugins/*.sh; . "$f") | FAIL / pass | FAIL | FAIL | pass | facts |
| C22 | workspace symbol finds a function in an unopened file | pass | pass | pass | pass | facts |
| C18 | missing `fi`: functions before and after are still in symbols | pass | FAIL | FAIL | pass | facts |
| C19 | unterminated `${x`: functions before and after are still in symbols | pass | FAIL | FAIL | pass | MISSING |
| C20 | syntax errors are reported as diagnostics in both broken files | pass | pass | pass | pass | facts |
| **Score, plain** | of 28 | 12 | 12 | 12 | 20 | 27 have the facts |
| **Score, annotated** | of 28 | 13 | 13 | 14 | 20 | |

| Timing, small corpus | bash-language-server 5.8.1 | shuck 0.2.11 | bashd 0.4.0 | bash-lsp 0.1.0 |
|---|---|---|---|---|
| Initialise, ms | 157 | 5 | 14 | 3 |
| Definition, warm median, ms | 0.6 | 0.2 | 0.2 | 0.2 |
| Memory, MB | 196 | 16 | 12 | 8 |

| One 561 KB script, 2,000 functions | bash-language-server 5.8.1 | shuck 0.2.11 | bashd 0.4.0 | bash-lsp 0.1.0 | mvdan/sh parse only |
|---|---|---|---|---|---|
| Open to first symbols, ms | 727 | 394 | exited (message too long for its reader) | 116 | 16 |
| Edit to refreshed symbols, ms | 596 | 366 | - | 104 | 16 |
| Definition after the edit, ms | 319.8 | 6.1 | - | 1.9 | - |
| Memory, MB | 592 | 255 | - | 155 | - |

| Capability advertised | bash-language-server 5.8.1 | shuck 0.2.11 | bashd 0.4.0 | bash-lsp 0.1.0 |
|---|---|---|---|---|
| definitionProvider | yes | yes | yes | yes |
| referencesProvider | yes | yes | yes | yes |
| hoverProvider | yes | yes | yes | yes |
| documentSymbolProvider | yes | yes | yes | yes |
| workspaceSymbolProvider | yes | yes | yes | yes |
| implementationProvider | - | - | - | yes |
| callHierarchyProvider | - | yes | - | - |
| renameProvider | yes | yes | yes | - |
| documentFormattingProvider | yes | yes | yes | - |
| documentRangeFormattingProvider | - | yes | yes | - |
| completionProvider | yes | yes | yes | - |
| signatureHelpProvider | - | - | - | - |
| codeActionProvider | yes | yes | yes | - |
| semanticTokensProvider | - | - | - | - |
| foldingRangeProvider | - | yes | - | - |
| selectionRangeProvider | - | yes | - | - |
| inlayHintProvider | - | - | yes | - |
| documentLinkProvider | - | yes | - | - |
| documentHighlightProvider | yes | yes | - | - |
| diagnosticProvider | - | yes | yes | - |
| codeLensProvider | - | - | - | - |
| colorProvider | - | - | yes | - |
