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
}
