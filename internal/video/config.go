package video

type Config struct {
	Enabled                 bool   `json:"enabled,omitempty"`
	StorePath               string `json:"store_path,omitempty"`
	StorageRoot             string `json:"storage_root,omitempty"`
	QueueSize               int    `json:"queue_size,omitempty"`
	Workers                 int    `json:"workers,omitempty"`
	AuthTokenEnv            string `json:"auth_token_env,omitempty"`
	MaxAssetBytes           int64  `json:"max_asset_bytes,omitempty"`
	DevelopmentFakeProvider bool   `json:"development_fake_provider,omitempty"`
}
