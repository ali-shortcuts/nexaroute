package config

import (
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func videoValidationConfig() Config {
	cfg := Default()
	cfg.Video.Enabled = true
	cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
	return cfg
}

func validComfyUIVideoConfig() Config {
	cfg := videoValidationConfig()
	cfg.Video.ComfyUIEnabled = true
	cfg.Video.ComfyUIBaseURL = "http://127.0.0.1:8188"
	cfg.Video.ComfyUIWorkflowPath = "comfyui-api-workflow.json"
	cfg.Video.ComfyUIPromptNodeID = "6"
	cfg.Video.ComfyUIWidthNodeID = "8"
	cfg.Video.ComfyUIHeightNodeID = "8"
	cfg.Video.ComfyUIFramesNodeID = "12"
	return cfg
}

func validCodeRenderVideoConfig() Config {
	cfg := videoValidationConfig()
	cfg.Video.CodeRenderEnabled = true
	cfg.Video.CodeRenderPlannerModel = "planner-model"
	cfg.Video.CodeRenderPlannerBaseURL = "http://127.0.0.1:9999/v1"
	cfg.Video.CodeRenderMaxDurationSeconds = 60
	cfg.Video.CodeRenderRetentionHours = 24
	return cfg
}

func TestValidateVideoDomainCharacterization(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "disabled with legacy zero values is optional",
			mutate: func(cfg *Config) {
				cfg.Video = VideoConfig{MaxAssetBytes: -1, CodeRenderMaxDurationSeconds: -1}
			},
		},
		{
			name: "enabled requires auth token env",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
			},
			wantErr: "video.auth_token_env is required when video.enabled is true",
		},
		{
			name: "enabled with auth token env is valid",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
			},
		},
		{
			name: "file job store backend is valid",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
				cfg.Video.JobStoreBackend = "file"
			},
		},
		{
			name: "unknown job store backend is rejected",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
				cfg.Video.JobStoreBackend = "s3"
			},
			wantErr: "video.job_store_backend must be file or postgres",
		},
		{
			name: "postgres job store requires a url",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
				cfg.Video.JobStoreBackend = "postgres"
			},
			wantErr: "video.job_store_url is required for the postgres job store backend",
		},
		{
			name: "postgres job store accepts a url",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
				cfg.Video.JobStoreBackend = "postgres"
				cfg.Video.JobStoreURL = "env:NEXAROUTE_TEST_PG_DSN"
			},
		},
		{
			name: "file job store rejects a url",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
				cfg.Video.JobStoreBackend = "file"
				cfg.Video.JobStoreURL = "postgres://localhost/db"
			},
			wantErr: "video.job_store_url must be empty for the file job store backend",
		},
		{
			name: "comfyui requires the video gateway",
			mutate: func(cfg *Config) {
				cfg.Video.ComfyUIEnabled = true
			},
			wantErr: "video.comfyui_enabled requires video.enabled",
		},
		{
			name: "code render requires the video gateway",
			mutate: func(cfg *Config) {
				cfg.Video.CodeRenderEnabled = true
			},
			wantErr: "video.code_render_enabled requires video.enabled",
		},
		{
			name: "comfyui gate error precedes code render gate error",
			mutate: func(cfg *Config) {
				cfg.Video.ComfyUIEnabled = true
				cfg.Video.CodeRenderEnabled = true
			},
			wantErr: "video.comfyui_enabled requires video.enabled",
		},
		{
			name: "auth env error precedes asset bound error",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.MaxAssetBytes = -1
			},
			wantErr: "video.auth_token_env is required when video.enabled is true",
		},
		{
			name: "asset cap minimum boundary",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
				cfg.Video.MaxAssetBytes = 0
			},
		},
		{
			name: "asset cap negative is rejected",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
				cfg.Video.MaxAssetBytes = -1
			},
			wantErr: "video.max_asset_bytes must be between 0 and 8589934592",
		},
		{
			name: "asset cap maximum boundary",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
				cfg.Video.MaxAssetBytes = 8 << 30
			},
		},
		{
			name: "asset cap above maximum is rejected",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
				cfg.Video.MaxAssetBytes = (8 << 30) + 1
			},
			wantErr: "video.max_asset_bytes must be between 0 and 8589934592",
		},
		{
			name: "code render requires a planner model or model list",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.AuthTokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
				cfg.Video.CodeRenderEnabled = true
			},
			wantErr: "video.code_render_planner_model or video.code_render_planner_models is required when code-render is enabled",
		},
		{
			name: "code render accepts a planner model",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
			},
		},
		{
			name: "code render accepts a planner model list",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderPlannerModel = ""
				cfg.Video.CodeRenderPlannerModels = []string{"planner-a", "planner-b"}
			},
		},
		{
			name: "code render duration minimum boundary",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderMaxDurationSeconds = 0
			},
		},
		{
			name: "code render duration maximum boundary",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderMaxDurationSeconds = 3600
			},
		},
		{
			name: "code render duration negative is rejected",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderMaxDurationSeconds = -1
			},
			wantErr: "video.code_render_max_duration_seconds must be between 0 and 3600",
		},
		{
			name: "code render duration above maximum is rejected",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderMaxDurationSeconds = 3600.5
			},
			wantErr: "video.code_render_max_duration_seconds must be between 0 and 3600",
		},
		{
			name: "code render duration NaN is rejected",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderMaxDurationSeconds = math.NaN()
			},
			wantErr: "video.code_render_max_duration_seconds must be between 0 and 3600",
		},
		{
			name: "code render duration infinity is rejected",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderMaxDurationSeconds = math.Inf(1)
			},
			wantErr: "video.code_render_max_duration_seconds must be between 0 and 3600",
		},
		{
			name: "code render planner base url rejects control characters",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderPlannerBaseURL = "http://127.0.0.1\r\n/evil"
			},
			wantErr: "video.code_render_planner_base_url is invalid or too long",
		},
		{
			name: "code render planner base url rejects oversized values",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderPlannerBaseURL = "http://127.0.0.1/" + strings.Repeat("a", maxURLBytes)
			},
			wantErr: "video.code_render_planner_base_url is invalid or too long",
		},
		{
			name: "code render retention maximum boundary",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderRetentionHours = 24 * 365
			},
		},
		{
			name: "code render retention negative keeps work forever",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderRetentionHours = -1
			},
		},
		{
			name: "code render retention above maximum is rejected",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderRetentionHours = 24*365 + 1
			},
			wantErr: "video.code_render_retention_hours must be at most 8760 (one year) or negative to keep work forever",
		},
		{
			name: "valid comfyui configuration",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
			},
		},
		{
			name: "comfyui requires base url and workflow path",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIBaseURL = ""
			},
			wantErr: "video.comfyui_base_url and video.comfyui_workflow_path are required when ComfyUI is enabled",
		},
		{
			name: "comfyui requires the workflow path",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIWorkflowPath = ""
			},
			wantErr: "video.comfyui_base_url and video.comfyui_workflow_path are required when ComfyUI is enabled",
		},
		{
			name: "comfyui requires prompt node id",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIPromptNodeID = ""
			},
			wantErr: "video.comfyui_prompt_node_id is required when ComfyUI is enabled",
		},
		{
			name: "comfyui requires width node id",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIWidthNodeID = ""
			},
			wantErr: "video.comfyui_width_node_id is required when ComfyUI is enabled",
		},
		{
			name: "comfyui requires height node id",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIHeightNodeID = ""
			},
			wantErr: "video.comfyui_height_node_id is required when ComfyUI is enabled",
		},
		{
			name: "comfyui requires frames node id",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIFramesNodeID = ""
			},
			wantErr: "video.comfyui_frames_node_id is required when ComfyUI is enabled",
		},
		{
			name: "comfyui duration minimum boundary",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIMaxDurationSeconds = 0
			},
		},
		{
			name: "comfyui duration maximum boundary",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIMaxDurationSeconds = 3600
			},
		},
		{
			name: "comfyui duration negative is rejected",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIMaxDurationSeconds = -1
			},
			wantErr: "video.comfyui_max_duration_seconds must be between 0 and 3600",
		},
		{
			name: "comfyui duration above maximum is rejected",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIMaxDurationSeconds = 3600.5
			},
			wantErr: "video.comfyui_max_duration_seconds must be between 0 and 3600",
		},
		{
			name: "comfyui duration NaN is rejected",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIMaxDurationSeconds = math.NaN()
			},
			wantErr: "video.comfyui_max_duration_seconds must be between 0 and 3600",
		},
		{
			name: "comfyui duration infinity is rejected",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIMaxDurationSeconds = math.Inf(1)
			},
			wantErr: "video.comfyui_max_duration_seconds must be between 0 and 3600",
		},
		{
			name: "code render error precedes auth and comfyui errors",
			mutate: func(cfg *Config) {
				cfg.Video.Enabled = true
				cfg.Video.CodeRenderEnabled = true
				cfg.Video.ComfyUIEnabled = true
			},
			wantErr: "video.code_render_planner_model or video.code_render_planner_models is required when code-render is enabled",
		},
		{
			name: "auth error precedes comfyui error once code render is valid",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.AuthTokenEnv = ""
				cfg.Video.ComfyUIEnabled = true
			},
			wantErr: "video.auth_token_env is required when video.enabled is true",
		},
		{
			name: "asset bound error precedes comfyui error",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.MaxAssetBytes = -1
				cfg.Video.ComfyUIEnabled = true
			},
			wantErr: "video.max_asset_bytes must be between 0 and 8589934592",
		},
		{
			name: "comfyui required fields error precedes duration bound",
			mutate: func(cfg *Config) {
				cfg.Video = validComfyUIVideoConfig().Video
				cfg.Video.ComfyUIBaseURL = ""
				cfg.Video.ComfyUIMaxDurationSeconds = -1
			},
			wantErr: "video.comfyui_base_url and video.comfyui_workflow_path are required when ComfyUI is enabled",
		},
		{
			name: "code render duration error precedes planner url error",
			mutate: func(cfg *Config) {
				cfg.Video = validCodeRenderVideoConfig().Video
				cfg.Video.CodeRenderMaxDurationSeconds = -1
				cfg.Video.CodeRenderPlannerBaseURL = "http://127.0.0.1\r\n/evil"
			},
			wantErr: "video.code_render_max_duration_seconds must be between 0 and 3600",
		},
		{
			name: "disabled comfyui and code render ignore their numeric fields",
			mutate: func(cfg *Config) {
				cfg.Video = VideoConfig{
					Enabled:                      true,
					AuthTokenEnv:                 "NEXAROUTE_TEST_VIDEO_TOKEN",
					ComfyUIMaxDurationSeconds:    -1,
					CodeRenderMaxDurationSeconds: -1,
					CodeRenderRetentionHours:     24*365 + 1,
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want %q", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("Validate() error = %q, want %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateVideoConfigRoundTrip(t *testing.T) {
	want := validCodeRenderVideoConfig()
	want.Video.ComfyUIEnabled = true
	want.Video.ComfyUIBaseURL = "http://127.0.0.1:8188"
	want.Video.ComfyUIWorkflowPath = "comfyui-api-workflow.json"
	want.Video.ComfyUIPromptNodeID = "6"
	want.Video.ComfyUIWidthNodeID = "8"
	want.Video.ComfyUIHeightNodeID = "8"
	want.Video.ComfyUIFramesNodeID = "12"
	want.Video.ComfyUIMaxDurationSeconds = 30
	want.Video.MaxAssetBytes = 1 << 30

	if err := want.Validate(); err != nil {
		t.Fatalf("valid video config rejected: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveAtomic(path, want); err != nil {
		t.Fatalf("SaveAtomic() error = %v", err)
	}
	got, err := LoadBase(path)
	if err != nil {
		t.Fatalf("LoadBase() error = %v", err)
	}
	if !reflect.DeepEqual(got.Video, want.Video) {
		t.Fatalf("video config changed across round trip:\n got: %#v\nwant: %#v", got.Video, want.Video)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("round-tripped video config rejected: %v", err)
	}
}
