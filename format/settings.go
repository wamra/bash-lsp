// Package format holds the formatter's options.
package format

// Settings are the formatter options a configuration source may set. A nil
// field means "not set here"; the formatter's own default applies.
type Settings struct {
	Indent           *uint // spaces per level; 0 means tabs
	BinaryNextLine   *bool
	SwitchCaseIndent *bool
	SpaceRedirects   *bool
	FunctionNextLine *bool
	Simplify         *bool
}
