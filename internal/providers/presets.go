package providers

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
}

func Presets() []Preset {
	return []Preset{
		{ID: "custom", Name: "Custom Provider", Category: "Custom", Type: "openai_compatible", AuthMode: "bearer"},
		{ID: "anthropic", Name: "Anthropic", Category: "Direct", Type: "anthropic_compatible", BaseURL: "https://api.anthropic.com", AuthMode: "x-api-key", MessagesPath: "/v1/messages", ModelsPath: "/v1/models", CountTokensPath: "/v1/messages/count_tokens"},
		{ID: "openai", Name: "OpenAI", Category: "Direct", Type: "openai_compatible", BaseURL: "https://api.openai.com/v1", AuthMode: "bearer"},
		{ID: "openai-responses", Name: "OpenAI Responses", Category: "Direct", Type: "openai_responses", BaseURL: "https://api.openai.com/v1", AuthMode: "bearer", ResponsesPath: "/v1/responses", ModelsPath: "/v1/models"},
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
}
