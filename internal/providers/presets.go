package providers

// Preset catalog data table (issue #394). A preset is the (a) provider-brand
// layer: it pairs a vendor or deployment style with the (b) protocol dialect
// and auth mode implemented by a real adapter in this package. A preset is
// NOT a deployment: (c) individual providers and models are configured
// separately through the Admin API, so presets stay inventorial claims only.
// Derived catalog claims (protocol identity, capabilities, status, discovery
// support) are computed by finalizePreset in catalog.go so they cannot drift
// from the implemented dialects.
//
// Honesty rules enforced by tests in catalog_test.go:
//   - only dialects with implemented adapters/types are advertised;
//   - models-list endpoints are advertised only where the dialect exposes a
//     real /models resource (Bedrock, Azure, Vertex therefore do not);
//   - every entry carries an explicit Status ("verified" | "partial").
type Preset struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Category        string `json:"category"`
	Type            string `json:"type"`
	BaseURL         string `json:"base_url"`
	AuthMode        string `json:"auth_mode"`
	ChatPath        string `json:"chat_path,omitempty"`
	MessagesPath    string `json:"messages_path,omitempty"`
	ModelsPath      string `json:"models_path,omitempty"`
	CountTokensPath string `json:"count_tokens_path,omitempty"`
	ResponsesPath   string `json:"responses_path,omitempty"`
	Local           bool   `json:"local,omitempty"`
	// Protocol is the explicit wire-dialect identity (equal to Type) so the
	// catalog API can name preset-vs-protocol-vs-deployment distinctly.
	Protocol string `json:"protocol"`
	// Capabilities lists only implemented wire features for this dialect.
	Capabilities []string `json:"capabilities,omitempty"`
	// Status is "verified" when the dialect+auth pairing is implemented and
	// covered by in-tree tests, otherwise "partial". Never empty.
	Status string `json:"status"`
	// SupportsDiscovery advertises a real models-listing endpoint; discovery
	// itself is always best-effort and never required to save a model.
	SupportsDiscovery bool `json:"supports_discovery"`
	// Custom marks the generic template preset (no fixed endpoint).
	Custom bool `json:"custom,omitempty"`
}

func Presets() []Preset {
	out := make([]Preset, 0, len(presetList))
	for _, p := range presetList {
		out = append(out, finalizePreset(p))
	}
	return out
}

var presetList = []Preset{
	{ID: "custom", Name: "Custom Provider", Category: "Custom", Type: "openai_compatible", AuthMode: "bearer", Custom: true},
	{ID: "anthropic", Name: "Anthropic", Category: "Direct", Type: "anthropic_compatible", BaseURL: "https://api.anthropic.com", AuthMode: "x-api-key", MessagesPath: "/v1/messages", ModelsPath: "/v1/models", CountTokensPath: "/v1/messages/count_tokens"},
	{ID: "openai", Name: "OpenAI", Category: "Direct", Type: "openai_compatible", BaseURL: "https://api.openai.com/v1", AuthMode: "bearer"},
	{ID: "openai-responses", Name: "OpenAI Responses", Category: "Direct", Type: "openai_responses", BaseURL: "https://api.openai.com/v1", AuthMode: "bearer", ResponsesPath: "/v1/responses", ModelsPath: "/v1/models"},
	// Bedrock's runtime hosts Converse APIs, not a plain /models listing on
	// this host, so no inventory endpoint is advertised (no fabricated
	// discovery). Chat + SigV4 auth are implemented; discovery is not.
	{ID: "amazon-bedrock", Name: "Amazon Bedrock", Category: "Direct", Type: "bedrock", BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com"},
	{ID: "azure-openai", Name: "Azure OpenAI", Category: "Cloud", Type: "azure_openai", BaseURL: "https://YOUR-RESOURCE.openai.azure.com", AuthMode: "api-key"},
	{ID: "google-vertex", Name: "Google Vertex AI", Category: "Cloud", Type: "vertex", BaseURL: "https://us-central1-aiplatform.googleapis.com", AuthMode: "bearer"},
	{ID: "google-gemini", Name: "Google Gemini", Category: "Direct", Type: "gemini", BaseURL: "https://generativelanguage.googleapis.com", AuthMode: "x-goog-api-key", ModelsPath: "/v1beta/models"},
	{ID: "openrouter", Name: "OpenRouter", Category: "Gateway", Type: "openai_compatible", BaseURL: "https://openrouter.ai/api/v1", AuthMode: "bearer"},
	{ID: "deepseek", Name: "DeepSeek", Category: "Direct", Type: "openai_compatible", BaseURL: "https://api.deepseek.com/v1", AuthMode: "bearer"},
	{ID: "groq", Name: "Groq", Category: "Fast inference", Type: "openai_compatible", BaseURL: "https://api.groq.com/openai/v1", AuthMode: "bearer"},
	{ID: "together", Name: "Together AI", Category: "Inference", Type: "openai_compatible", BaseURL: "https://api.together.xyz/v1", AuthMode: "bearer"},
	{ID: "mistral", Name: "Mistral AI", Category: "Direct", Type: "openai_compatible", BaseURL: "https://api.mistral.ai/v1", AuthMode: "bearer"},
	{ID: "xai", Name: "xAI", Category: "Direct", Type: "openai_compatible", BaseURL: "https://api.x.ai/v1", AuthMode: "bearer"},
	{ID: "fireworks", Name: "Fireworks AI", Category: "Inference", Type: "openai_compatible", BaseURL: "https://api.fireworks.ai/inference/v1", AuthMode: "bearer"},
	{ID: "cerebras", Name: "Cerebras", Category: "Fast inference", Type: "openai_compatible", BaseURL: "https://api.cerebras.ai/v1", AuthMode: "bearer"},
	{ID: "nvidia-nim", Name: "NVIDIA NIM", Category: "Inference", Type: "openai_compatible", BaseURL: "https://integrate.api.nvidia.com/v1", AuthMode: "bearer"},
	{ID: "sambanova", Name: "SambaNova Cloud", Category: "Fast inference", Type: "openai_compatible", BaseURL: "https://api.sambanova.ai/v1", AuthMode: "bearer"},
	{ID: "siliconflow", Name: "SiliconFlow", Category: "Inference", Type: "openai_compatible", BaseURL: "https://api.siliconflow.com/v1", AuthMode: "bearer"},
	{ID: "siliconflow-cn", Name: "SiliconFlow China", Category: "Inference", Type: "openai_compatible", BaseURL: "https://api.siliconflow.cn/v1", AuthMode: "bearer"},
	{ID: "moonshot", Name: "Moonshot AI", Category: "Direct", Type: "openai_compatible", BaseURL: "https://api.moonshot.ai/v1", AuthMode: "bearer"},
	{ID: "minimax", Name: "MiniMax", Category: "Direct", Type: "openai_compatible", BaseURL: "https://api.minimax.io/v1", AuthMode: "bearer"},
	{ID: "qwen-intl", Name: "Qwen / Model Studio International", Category: "Direct", Type: "openai_compatible", BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", AuthMode: "bearer"},
	{ID: "qwen-us", Name: "Qwen / Model Studio US", Category: "Direct", Type: "openai_compatible", BaseURL: "https://dashscope-us.aliyuncs.com/compatible-mode/v1", AuthMode: "bearer"},
	{ID: "perplexity", Name: "Perplexity", Category: "Search", Type: "openai_compatible", BaseURL: "https://api.perplexity.ai", AuthMode: "bearer"},
	{ID: "huggingface", Name: "Hugging Face Inference Providers", Category: "Gateway", Type: "openai_compatible", BaseURL: "https://router.huggingface.co/v1", AuthMode: "bearer"},
	{ID: "github-models", Name: "GitHub Models", Category: "Gateway", Type: "openai_compatible", BaseURL: "https://models.github.ai/inference", AuthMode: "bearer", ChatPath: "/chat/completions", ModelsPath: "/models"},
	{ID: "chat2api", Name: "Chat2API", Category: "Local bridge", Type: "openai_compatible", BaseURL: "http://127.0.0.1:5000/v1", AuthMode: "bearer", Local: true},
	{ID: "ollama", Name: "Ollama", Category: "Local", Type: "openai_compatible", BaseURL: "http://127.0.0.1:11434/v1", AuthMode: "none", Local: true},
	{ID: "lmstudio", Name: "LM Studio", Category: "Local", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1234/v1", AuthMode: "none", Local: true},
	{ID: "vllm", Name: "vLLM", Category: "Local", Type: "openai_compatible", BaseURL: "http://127.0.0.1:8000/v1", AuthMode: "none", Local: true},
}
