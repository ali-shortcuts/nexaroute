package config

import "testing"

// M6-1 MCP registry validation.
func TestMCPConfigValidation(t *testing.T) {
	base := func() Config {
		cfg := Default()
		cfg.ApplyDefaults()
		return cfg
	}
	cfg := base()
	cfg.MCP.Enabled = true
	cfg.MCP.Servers = []MCPServerConfig{{ID: "tools", URL: "https://mcp.example.com/rpc", Enabled: true, AuthEnv: "MCP_TOOLS_AUTH"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid MCP config rejected: %v", err)
	}
	if cfg.MCP.MaxRequestBytes != 1<<20 || cfg.MCP.MaxResponseBytes != 1<<20 || cfg.MCP.CallTimeoutSeconds != 30 {
		t.Fatalf("MCP defaults not applied: %+v", cfg.MCP)
	}
	cfg.MCP.Servers[0].URL = "https://user:secret@mcp.example.com/rpc"
	if err := cfg.Validate(); err == nil {
		t.Fatal("URL with embedded credentials accepted")
	}
	cfg.MCP.Servers[0].URL = "ftp://mcp.example.com"
	if err := cfg.Validate(); err == nil {
		t.Fatal("non-http scheme accepted")
	}
	cfg.MCP.Servers[0].URL = "https://mcp.example.com/rpc"
	cfg.MCP.Servers = append(cfg.MCP.Servers, MCPServerConfig{ID: "tools", URL: "https://other.example.com"})
	if err := cfg.Validate(); err == nil {
		t.Fatal("duplicate server id accepted")
	}
	cfg2 := base()
	cfg2.MCP.Enabled = true
	if err := cfg2.Validate(); err == nil {
		t.Fatal("enabled with zero servers accepted")
	}
	cfg2.MCP.Servers = []MCPServerConfig{{ID: "s", URL: "https://x.example.com"}}
	cfg2.MCP.CallTimeoutSeconds = 0
	cfg2.ApplyDefaults()
	cfg2.MCP.CallTimeoutSeconds = 9999
	if err := cfg2.Validate(); err == nil {
		t.Fatal("absurd call timeout accepted")
	}
	cfg2.MCP.CallTimeoutSeconds = 30
	cfg2.MCP.MaxRequestBytes = 1023
	if err := cfg2.Validate(); err == nil {
		t.Fatal("MCP request limit below minimum accepted")
	}
	cfg2.MCP.MaxRequestBytes = 64<<20 + 1
	if err := cfg2.Validate(); err == nil {
		t.Fatal("MCP request limit above maximum accepted")
	}
}
