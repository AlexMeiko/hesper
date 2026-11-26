package dto

// EngineType identifies a supported database engine.
type EngineType string

const (
	EngineMySQL    EngineType = "mysql"
	EnginePostgres EngineType = "postgres"
	EngineMariaDB  EngineType = "mariadb"
	EngineSQLite   EngineType = "sqlite"
	EngineFake     EngineType = "fake"
)

// TLSConfig holds optional TLS/SSL connection parameters.
type TLSConfig struct {
	Mode       string `json:"mode"` // disable | prefer | require | verify-ca | verify-full
	CACertPath string `json:"caCertPath,omitempty"`
	CertPath   string `json:"certPath,omitempty"`
	KeyPath    string `json:"keyPath,omitempty"`
}

// SSHConfig holds non-secret SSH bastion tunnel parameters stored in app.db config_json.
// Plaintext SSH passwords and passphrases are NEVER stored here or returned in ConnectionDTO.
type SSHConfig struct {
	Enabled        bool   `json:"enabled"`
	Host           string `json:"host,omitempty"`
	Port           int    `json:"port,omitempty"`
	Username       string `json:"username,omitempty"`
	AuthMethod     string `json:"authMethod,omitempty"` // password | key | agent
	PrivateKeyPath string `json:"privateKeyPath,omitempty"`
	SaveCredential bool   `json:"saveCredential"`
}

// SSHHostKeyChallenge describes an unverified or changed SSH host key presented during handshake.
type SSHHostKeyChallenge struct {
	HostPort            string `json:"hostPort"`
	Algorithm           string `json:"algorithm"`
	Fingerprint         string `json:"fingerprint"`
	Mismatch            bool   `json:"mismatch"`
	PreviousFingerprint string `json:"previousFingerprint,omitempty"`
}

// SSHAuthPrompt requests a runtime SSH password or private-key passphrase from the user.
type SSHAuthPrompt struct {
	ConnectionID   string `json:"connectionId,omitempty"`
	HostPort       string `json:"hostPort"`
	Username       string `json:"username"`
	AuthMethod     string `json:"authMethod"` // password | key
	NeedPassphrase bool   `json:"needPassphrase,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// ConnectionParams holds non-secret connection configuration serialized into app.db config_json.
type ConnectionParams struct {
	Host            string     `json:"host"`
	Port            int        `json:"port"`
	Username        string     `json:"username"`
	DefaultDatabase string     `json:"defaultDatabase"`
	FilePath        string     `json:"filePath,omitempty"` // SQLite only
	TLS             TLSConfig  `json:"tls"`
	SSH             *SSHConfig `json:"ssh,omitempty"`
	ConnectTimeoutS int        `json:"connectTimeoutS,omitempty"`
	ErrorCharset    string     `json:"errorCharset,omitempty"`
}

// ConnectionDTO is the safe frontend representation of a saved connection.
// Plaintext passwords are NEVER sent back to the frontend.
type ConnectionDTO struct {
	ID               string           `json:"id"`
	GroupID          string           `json:"groupId,omitempty"`
	Name             string           `json:"name"`
	Driver           EngineType       `json:"driver"`
	EnvColor         string           `json:"envColor"` // red | amber | green | default
	ReadOnly         bool             `json:"readOnly"`
	Params           ConnectionParams `json:"params"`
	HasPassword      bool             `json:"hasPassword"`
	HasSSHPassword   bool             `json:"hasSshPassword,omitempty"`
	HasSSHPassphrase bool             `json:"hasSshPassphrase,omitempty"`
	SortOrder        int              `json:"sortOrder"`
	CreatedAt        int64            `json:"createdAt"`
	UpdatedAt        int64            `json:"updatedAt"`
}

// SaveConnectionRequest is sent from frontend to create or update a connection.
// When UpdatePassword is false on edit, existing stored password is kept untouched.
type SaveConnectionRequest struct {
	ID                  string           `json:"id,omitempty"`
	GroupID             string           `json:"groupId,omitempty"`
	Name                string           `json:"name"`
	Driver              EngineType       `json:"driver"`
	EnvColor            string           `json:"envColor"`
	ReadOnly            bool             `json:"readOnly"`
	Params              ConnectionParams `json:"params"`
	Password            string           `json:"password,omitempty"`
	UpdatePassword      bool             `json:"updatePassword"`
	SSHPassword         string           `json:"sshPassword,omitempty"`
	UpdateSSHPassword   bool             `json:"updateSshPassword,omitempty"`
	SSHPassphrase       string           `json:"sshPassphrase,omitempty"`
	UpdateSSHPassphrase bool             `json:"updateSshPassphrase,omitempty"`
}

// TestConnectionResult reports real server detection and latency from a connection test.
type TestConnectionResult struct {
	Success       bool         `json:"success"`
	DetectedType  EngineType   `json:"detectedType"`
	ServerVersion string       `json:"serverVersion"`
	LatencyMs     int64        `json:"latencyMs"`
	Capabilities  Capabilities `json:"capabilities"`
	Error         *AppError    `json:"error,omitempty"`
}

// ConnectResult is returned when opening/activating a connection in the workspace.
type ConnectResult struct {
	ConnectionID    string       `json:"connectionId"`
	DetectedType    EngineType   `json:"detectedType"`
	ServerVersion   string       `json:"serverVersion"`
	CurrentDB       string       `json:"currentDb"`
	Capabilities    Capabilities `json:"capabilities"`
	CredentialSaved bool         `json:"credentialSaved,omitempty"`
	SaveWarning     string       `json:"saveWarning,omitempty"`
}

// Capabilities declares what objects and behaviors a connected database supports.
// Frontend renders tree nodes and controls strictly from this struct.
type Capabilities struct {
	HasDatabases               bool `json:"hasDatabases"`
	HasSchemas                 bool `json:"hasSchemas"`
	SwitchDBReconnects         bool `json:"switchDbReconnects"`
	SupportsCancel             bool `json:"supportsCancel"`
	SupportsViews              bool `json:"supportsViews"`
	SupportsMatViews           bool `json:"supportsMatViews"`
	SupportsRoutines           bool `json:"supportsRoutines"`
	SupportsTriggers           bool `json:"supportsTriggers"`
	SupportsSequences          bool `json:"supportsSequences"`
	SupportsSavepoint          bool `json:"supportsSavepoint"`
	SupportsExplain            bool `json:"supportsExplain"`
	DDLTransactional           bool `json:"ddlTransactional"`
	SupportsSingleStmtAlterAll bool `json:"supportsSingleStmtAlterAll"`
}
