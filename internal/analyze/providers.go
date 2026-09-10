package analyze

const (
	ProviderCodexCLI   = "codex-cli"
	ProviderClaudeCLI  = "claude-cli"
	DefaultCodexModel  = "gpt-5.6-luna"
	DefaultClaudeModel = "haiku"
)

// Provider is the public metadata for one supported analysis adapter.
type Provider struct {
	ID           string
	Name         string
	DefaultModel string
	Available    bool
}

// DiscoverCLIProviders reports the fixed provider catalog and constructs
// adapters only for executables visible to the current process.
func DiscoverCLIProviders(lookPath func(string) (string, error)) ([]Provider, map[string]Analyzer) {
	providers := []Provider{
		{ID: ProviderCodexCLI, Name: "Codex", DefaultModel: DefaultCodexModel},
		{ID: ProviderClaudeCLI, Name: "Claude Code", DefaultModel: DefaultClaudeModel},
	}
	analyzers := make(map[string]Analyzer)
	if path, err := lookPath("codex"); err == nil {
		providers[0].Available = true
		analyzers[ProviderCodexCLI] = NewCodexCLI(path)
	}
	if path, err := lookPath("claude"); err == nil {
		providers[1].Available = true
		analyzers[ProviderClaudeCLI] = NewClaudeCLI(path)
	}
	return providers, analyzers
}

// FindProvider finds supported provider metadata by stable ID.
func FindProvider(providers []Provider, id string) (Provider, bool) {
	for _, provider := range providers {
		if provider.ID == id {
			return provider, true
		}
	}
	return Provider{}, false
}
