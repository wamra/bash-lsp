package config

import "testing"

func TestEditorKeyDefaults(t *testing.T) {
	s := Merge(nil, nil, nil)
	if !s.InlayHints.SourceTargets || s.InlayHints.ParameterNames {
		t.Errorf("inlay hints = %+v, want source targets on, parameter names off", s.InlayHints)
	}
	if !s.Completion.PathCommands {
		t.Error("completion.path_commands should default to true")
	}
	if !s.Help.Enabled || s.Help.BashPath != "" || s.Help.ManPath != "" {
		t.Errorf("help = %+v, want enabled with no paths", s.Help)
	}
}

func TestEditorKeysFromAnyLayer(t *testing.T) {
	s := Merge(nil, []byte("[inlay_hints]\nsource_targets = false\nparameter_names = true\n"), nil)
	if s.InlayHints.SourceTargets || !s.InlayHints.ParameterNames {
		t.Errorf("repo inlay hints = %+v", s.InlayHints)
	}
	if len(s.Warnings) != 0 || len(s.Refused) != 0 {
		t.Errorf("warnings %v refused %v", s.Warnings, s.Refused)
	}
}

func TestRepoCannotNameHelpPrograms(t *testing.T) {
	s := Merge(nil, []byte("[help]\nbash_path = \"/tmp/b\"\nman_path = \"/tmp/m\"\n"), nil)
	got := map[string]bool{}
	for _, r := range s.Refused {
		got[r.Key] = true
		if r.Reason != refusalReason {
			t.Errorf("reason %q", r.Reason)
		}
	}
	if !got["help.bash_path"] || !got["help.man_path"] {
		t.Errorf("Refused = %+v, want help.bash_path and help.man_path", s.Refused)
	}
	if s.Help.BashPath != "" || s.Help.ManPath != "" {
		t.Errorf("Help = %+v, want no paths", s.Help)
	}
}

func TestRepoCannotEnableHelpOrPath(t *testing.T) {
	for _, tc := range []struct{ key, section, name string }{
		{"help.enabled", "help", "enabled"},
		{"completion.path_commands", "completion", "path_commands"},
	} {
		user := []byte("[" + tc.section + "]\n" + tc.name + " = false\n")
		repo := []byte("# repo\n[" + tc.section + "]\n" + tc.name + " = true\n")
		s := Merge(user, repo, nil)
		if s.Help.Enabled && tc.key == "help.enabled" || s.Completion.PathCommands && tc.key == "completion.path_commands" {
			t.Errorf("%s: a repo file turned it back on", tc.key)
		}
		if !hasWarning(s, tc.key, ".bash-lsp.toml:3") {
			t.Errorf("%s: warnings %v do not name the key and line", tc.key, s.Warnings)
		}
		// A repo file may turn it off.
		off := Merge(nil, []byte("["+tc.section+"]\n"+tc.name+" = false\n"), nil)
		if off.Help.Enabled && tc.key == "help.enabled" || off.Completion.PathCommands && tc.key == "completion.path_commands" {
			t.Errorf("%s: a repo file could not turn it off", tc.key)
		}
	}
}

func TestInitHelpPaths(t *testing.T) {
	s := Merge(nil, nil, []byte(`{"help":{"bash_path":"/bin/bash","man_path":"/usr/bin/man","enabled":false}}`))
	if s.Help.BashPath != "/bin/bash" || s.Help.ManPath != "/usr/bin/man" || s.Help.Enabled {
		t.Errorf("Help = %+v", s.Help)
	}
	if len(s.Refused) != 0 || len(s.Warnings) != 0 {
		t.Errorf("refused %v warnings %v", s.Refused, s.Warnings)
	}
}
