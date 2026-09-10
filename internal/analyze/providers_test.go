package analyze

import (
	"os/exec"
	"testing"
)

func TestDefaultCodexModelUsesCurrentFastSubscriptionModel(t *testing.T) {
	if DefaultCodexModel != "gpt-5.6-luna" {
		t.Fatalf("DefaultCodexModel = %q", DefaultCodexModel)
	}
}

func TestDiscoverCLIProviders(t *testing.T) {
	for _, test := range []struct {
		name      string
		installed map[string]string
		available []bool
		adapters  int
	}{
		{name: "both", installed: map[string]string{"codex": "/bin/codex", "claude": "/bin/claude"}, available: []bool{true, true}, adapters: 2},
		{name: "codex", installed: map[string]string{"codex": "/bin/codex"}, available: []bool{true, false}, adapters: 1},
		{name: "claude", installed: map[string]string{"claude": "/bin/claude"}, available: []bool{false, true}, adapters: 1},
		{name: "neither", installed: map[string]string{}, available: []bool{false, false}, adapters: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(name string) (string, error) {
				if path, ok := test.installed[name]; ok {
					return path, nil
				}
				return "", exec.ErrNotFound
			}
			providers, adapters := DiscoverCLIProviders(lookup)
			if len(providers) != 2 || len(adapters) != test.adapters {
				t.Fatalf("providers = %#v, adapters = %d", providers, len(adapters))
			}
			if providers[0].ID != ProviderCodexCLI || providers[0].DefaultModel != DefaultCodexModel ||
				providers[0].Available != test.available[0] {
				t.Fatalf("Codex provider = %#v", providers[0])
			}
			if providers[1].ID != ProviderClaudeCLI || providers[1].DefaultModel != DefaultClaudeModel ||
				providers[1].Available != test.available[1] {
				t.Fatalf("Claude provider = %#v", providers[1])
			}
		})
	}
}

func TestFindProvider(t *testing.T) {
	providers, _ := DiscoverCLIProviders(func(string) (string, error) { return "", exec.ErrNotFound })
	if provider, ok := FindProvider(providers, ProviderClaudeCLI); !ok || provider.Name != "Claude Code" {
		t.Fatalf("Claude provider = %#v, %v", provider, ok)
	}
	if _, ok := FindProvider(providers, "shell"); ok {
		t.Fatal("unknown provider was accepted")
	}
}
