package video

type Config struct {
	Enabled                   bool    `json:"enabled,omitempty"`
	StorePath                 string  `json:"store_path,omitempty"`
	StorageRoot               string  `json:"storage_root,omitempty"`
	QueueSize                 int     `json:"queue_size,omitempty"`
	Workers                   int     `json:"workers,omitempty"`
	AuthTokenEnv              string  `json:"auth_token_env,omitempty"`
	MaxAssetBytes             int64   `json:"max_asset_bytes,omitempty"`
	DevelopmentFakeProvider   bool    `json:"development_fake_provider,omitempty"`
	ComfyUIEnabled            bool    `json:"comfyui_enabled,omitempty"`
	ComfyUIBaseURL            string  `json:"comfyui_base_url,omitempty"`
	ComfyUIWorkflowPath       string  `json:"comfyui_workflow_path,omitempty"`
	ComfyUIModel              string  `json:"comfyui_model,omitempty"`
	ComfyUIMaxDurationSeconds float64 `json:"comfyui_max_duration_seconds,omitempty"`
	ComfyUIPromptNodeID       string  `json:"comfyui_prompt_node_id,omitempty"`
	ComfyUIPromptInput        string  `json:"comfyui_prompt_input,omitempty"`
	ComfyUINegativeNodeID     string  `json:"comfyui_negative_node_id,omitempty"`
	ComfyUINegativeInput      string  `json:"comfyui_negative_input,omitempty"`
	ComfyUIWidthNodeID        string  `json:"comfyui_width_node_id,omitempty"`
	ComfyUIWidthInput         string  `json:"comfyui_width_input,omitempty"`
	ComfyUIHeightNodeID       string  `json:"comfyui_height_node_id,omitempty"`
	ComfyUIHeightInput        string  `json:"comfyui_height_input,omitempty"`
	ComfyUIFramesNodeID       string  `json:"comfyui_frames_node_id,omitempty"`
	ComfyUIFramesInput        string  `json:"comfyui_frames_input,omitempty"`
	ComfyUIFPSNodeID          string  `json:"comfyui_fps_node_id,omitempty"`
	ComfyUIFPSInput           string  `json:"comfyui_fps_input,omitempty"`
	ComfyUISeedNodeID         string  `json:"comfyui_seed_node_id,omitempty"`
	ComfyUISeedInput          string  `json:"comfyui_seed_input,omitempty"`
	PiperEnabled              bool    `json:"piper_enabled,omitempty"`
	PiperBinary               string  `json:"piper_binary,omitempty"`
	PiperModelPath            string  `json:"piper_model_path,omitempty"`
	// Universal AI-to-Video (code-render) provider, issue #461 Tier A.
	// The planner endpoint is an OpenAI-compatible chat-completions base
	// URL; in production it normally points back at this gateway's own
	// data plane so any configured text/coding model can plan videos
	// without a second credential store.
	CodeRenderEnabled            bool     `json:"code_render_enabled,omitempty"`
	CodeRenderPlannerBaseURL     string   `json:"code_render_planner_base_url,omitempty"`
	CodeRenderPlannerAPIKeyEnv   string   `json:"code_render_planner_api_key_env,omitempty"`
	CodeRenderPlannerModel       string   `json:"code_render_planner_model,omitempty"`
	CodeRenderPlannerModels      []string `json:"code_render_planner_models,omitempty"`
	CodeRenderPlannerJSONMode    bool     `json:"code_render_planner_json_mode,omitempty"`
	CodeRenderMaxDurationSeconds float64  `json:"code_render_max_duration_seconds,omitempty"`
	CodeRenderWorkRoot           string   `json:"code_render_work_root,omitempty"`
}
