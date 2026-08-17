package domain

// ConnectEnvVar is one environment variable the Dagger CLI reads.
type ConnectEnvVar struct {
	Name        string `json:"name"`
	Value       string `json:"value"` // full value, including plaintext token when reveal=true
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret"`
	Description string `json:"description"`
}

// ConnectTokenMeta is the masked view of the caller's API token.
type ConnectTokenMeta struct {
	Exists      bool   `json:"exists"`
	Prefix      string `json:"prefix"`      // e.g. "dct_ab12cd34"; "" when !Exists
	Recoverable bool   `json:"recoverable"` // true when ciphertext is present + key available
}

// ConnectEnvSnapshot is the payload of GET /api/v1/connect/env.
type ConnectEnvSnapshot struct {
	ServerURL       string           `json:"server_url"`
	DataHostname    string           `json:"data_hostname"`
	CacheBackend    string           `json:"cache_backend"`
	VersionFloor    string           `json:"version_floor"`
	AllowedVersions []string         `json:"allowed_versions"`
	SelectedVersion string           `json:"selected_version,omitempty"`
	Token           ConnectTokenMeta `json:"token"`
	EnvVars         []ConnectEnvVar  `json:"env_vars"`
}
