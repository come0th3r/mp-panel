package model

import "encoding/json"

type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	TOTPEnabled bool   `json:"totp_enabled"`
}
type Node struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Address         string `json:"address"`
	SSHPort         int    `json:"ssh_port"`
	SSHUser         string `json:"ssh_user"`
	GameBindAddress string `json:"game_bind_address"`
	Status          string `json:"status"`
	OS              string `json:"os,omitempty"`
	Arch            string `json:"arch,omitempty"`
	AgentVersion    string `json:"agent_version,omitempty"`
	CPUCount        int    `json:"cpu_count,omitempty"`
	MemoryTotal     uint64 `json:"memory_total,omitempty"`
	DiskTotal       uint64 `json:"disk_total,omitempty"`
	DiskFree        uint64 `json:"disk_free,omitempty"`
	LastSeen        string `json:"last_seen,omitempty"`
	Error           string `json:"error,omitempty"`
	CreatedAt       string `json:"created_at"`
}
type Server struct {
	ID               string  `json:"id"`
	NodeID           string  `json:"node_id"`
	Name             string  `json:"name"`
	Core             string  `json:"core"`
	MinecraftVersion string  `json:"minecraft_version"`
	LoaderVersion    string  `json:"loader_version"`
	Build            string  `json:"build"`
	JavaVersion      int     `json:"java_version"`
	MemoryMB         int     `json:"memory_mb"`
	CPULimit         float64 `json:"cpu_limit"`
	Port             int     `json:"port"`
	Status           string  `json:"status"`
	DesiredState     string  `json:"desired_state"`
	EULAAccepted     bool    `json:"eula_accepted"`
	CreatedAt        string  `json:"created_at"`
	Error            string  `json:"error,omitempty"`
}
type Job struct {
	ID        string   `json:"id"`
	NodeID    string   `json:"node_id"`
	ServerID  string   `json:"server_id,omitempty"`
	Type      string   `json:"type"`
	Status    string   `json:"status"`
	Step      string   `json:"step"`
	Error     string   `json:"error,omitempty"`
	Logs      []string `json:"logs"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}
type Metric struct {
	Timestamp     string  `json:"timestamp"`
	NodeID        string  `json:"node_id"`
	ServerID      string  `json:"server_id,omitempty"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryUsed    uint64  `json:"memory_used"`
	MemoryLimit   uint64  `json:"memory_limit"`
	DiskUsed      uint64  `json:"disk_used"`
	DiskFree      uint64  `json:"disk_free"`
	IORead        uint64  `json:"io_read"`
	IOWrite       uint64  `json:"io_write"`
	UptimeSeconds uint64  `json:"uptime_seconds,omitempty"`
}
type Backup struct {
	ID        string `json:"id"`
	ServerID  string `json:"server_id"`
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	CreatedAt string `json:"created_at"`
	Path      string `json:"path"`
}
type FileEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}
type FileContent struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Revision string `json:"revision"`
}
type Artifact struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
	SHA1   string `json:"sha1,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Target string `json:"target,omitempty"`
}
type InstallSpec struct {
	Core             string     `json:"core"`
	MinecraftVersion string     `json:"minecraft_version"`
	LoaderVersion    string     `json:"loader_version"`
	Build            string     `json:"build"`
	JavaVersion      int        `json:"java_version"`
	InstallerVersion string     `json:"installer_version,omitempty"`
	Artifacts        []Artifact `json:"artifacts"`
	ExtraJars        []Artifact `json:"extra_jars,omitempty"`
	LaunchJar        string     `json:"launch_jar,omitempty"`
	InstallMode      string     `json:"install_mode"`
}
type Command struct {
	ID       string          `json:"id"`
	JobID    string          `json:"job_id,omitempty"`
	NodeID   string          `json:"node_id"`
	ServerID string          `json:"server_id,omitempty"`
	Type     string          `json:"type"`
	Server   *Server         `json:"server,omitempty"`
	Install  *InstallSpec    `json:"install,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Deadline string          `json:"deadline"`
}
type CommandResult struct {
	CommandID string          `json:"command_id"`
	NodeID    string          `json:"node_id"`
	Success   bool            `json:"success"`
	Error     string          `json:"error,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Logs      []string        `json:"logs,omitempty"`
}
type ObservedServer struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Error  string   `json:"error,omitempty"`
	Logs   []string `json:"logs,omitempty"`
}
type Heartbeat struct {
	Node    Node             `json:"node"`
	Servers []ObservedServer `json:"servers"`
	Metrics []Metric         `json:"metrics"`
}
type AgentConfig struct {
	DownloadUserAgent string `json:"download_user_agent"`
	NodeID            string `json:"node_id"`
	PanelURL          string `json:"panel_url"`
	DataDir           string `json:"data_dir"`
	Certificate       string `json:"certificate"`
	PrivateKey        string `json:"private_key"`
	CA                string `json:"ca"`
	PollSeconds       int    `json:"poll_seconds,omitempty"`
	BindAddress       string `json:"bind_address,omitempty"`
}
