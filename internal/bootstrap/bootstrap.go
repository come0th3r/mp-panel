package bootstrap

import (
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	Address                                                   string
	Port                                                      int
	User, Password, PrivateKey, HostFingerprint, SudoPassword string
	NodeID, AgentURL, AgentBinaryPath                         string
	Certificate, PrivateKeyPEM, CA                            []byte
	BindAddress                                               string
	DownloadUserAgent                                         string
}

func endpoint(address string, port int) (string, error) {
	if address == "" || strings.ContainsAny(address, " \t\r\n/\x00") {
		return "", errors.New("invalid SSH address")
	}
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return "", errors.New("invalid SSH port")
	}
	return net.JoinHostPort(strings.Trim(address, "[]"), strconv.Itoa(port)), nil
}

func Fingerprint(ctx context.Context, address string, port int) (string, error) {
	addr, err := endpoint(address, port)
	if err != nil {
		return "", err
	}
	dialer := net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	var fingerprint string
	cfg := &ssh.ClientConfig{User: "mp-fingerprint", Timeout: 15 * time.Second, HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fingerprint = ssh.FingerprintSHA256(key)
		return nil
	}}
	client, _, _, err := ssh.NewClientConn(conn, addr, cfg)
	if client != nil {
		client.Close()
	}
	if fingerprint != "" {
		return fingerprint, nil
	}
	return "", fmt.Errorf("read SSH host key: %w", err)
}

func Install(ctx context.Context, c Config, log func(string)) error {
	if c.BindAddress == "" {
		c.BindAddress = "0.0.0.0"
	}
	bindIP := net.ParseIP(c.BindAddress)
	if bindIP == nil {
		return errors.New("game bind address must be an IPv4 or IPv6 address")
	}
	c.BindAddress = bindIP.String()
	if strings.TrimSpace(c.DownloadUserAgent) == "" || strings.ContainsAny(c.DownloadUserAgent, "\r\n\x00") {
		return errors.New("download user agent must include a real administrator contact")
	}
	if log == nil {
		log = func(string) {}
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`).MatchString(c.NodeID) {
		return errors.New("invalid node ID")
	}
	if c.User == "" || c.HostFingerprint == "" {
		return errors.New("SSH user and verified host fingerprint required")
	}
	if strings.ContainsAny(c.SudoPassword, "\r\n\x00") {
		return errors.New("sudo password cannot contain newline or NUL")
	}
	u, err := url.Parse(c.AgentURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("agent panel URL must use HTTPS")
	}
	if len(c.Certificate) == 0 || len(c.PrivateKeyPEM) == 0 || len(c.CA) == 0 {
		return errors.New("agent certificate, key and panel CA required")
	}
	binary, err := os.Open(c.AgentBinaryPath)
	if err != nil {
		return fmt.Errorf("open Linux agent binary: %w", err)
	}
	defer binary.Close()
	info, err := binary.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 {
		return errors.New("agent binary must be a nonempty regular file")
	}
	// Reject accidentally supplied Windows binaries before touching the host.
	magic := make([]byte, 4)
	if _, err = io.ReadFull(binary, magic); err != nil {
		return err
	}
	if string(magic) != "\x7fELF" {
		return errors.New("agent binary must be a Linux ELF executable")
	}
	if _, err = binary.Seek(0, io.SeekStart); err != nil {
		return err
	}
	elfFile, err := elf.NewFile(binary)
	if err != nil {
		return fmt.Errorf("read Linux executable: %w", err)
	}
	if elfFile.Machine != elf.EM_X86_64 || elfFile.Class != elf.ELFCLASS64 {
		return errors.New("agent binary must target Linux x86_64")
	}
	addr, err := endpoint(c.Address, c.Port)
	if err != nil {
		return err
	}
	auth := []ssh.AuthMethod{}
	if c.PrivateKey != "" {
		signer, e := ssh.ParsePrivateKey([]byte(c.PrivateKey))
		if e != nil {
			return fmt.Errorf("parse SSH private key (encrypted keys are not accepted): %w", e)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if c.Password != "" {
		auth = append(auth, ssh.Password(c.Password))
	}
	if len(auth) == 0 {
		return errors.New("SSH key or password required")
	}
	sshCfg := &ssh.ClientConfig{User: c.User, Auth: auth, Timeout: 20 * time.Second, HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		actual := ssh.FingerprintSHA256(key)
		if actual != c.HostFingerprint {
			return fmt.Errorf("SSH host fingerprint changed: expected %s, got %s", c.HostFingerprint, actual)
		}
		return nil
	}}
	log("Connecting to host and verifying pinned SSH fingerprint")
	dialer := net.Dialer{Timeout: 20 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, addr, sshCfg)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConn, chans, reqs)
	defer client.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			client.Close()
		case <-done:
		}
	}()
	arch, err := run(client, "uname -m", "")
	if err != nil {
		return fmt.Errorf("read host architecture: %w", err)
	}
	if strings.TrimSpace(arch) != "x86_64" {
		return errors.New("bootstrap currently supports Linux x86_64 hosts only")
	}
	osRelease, err := run(client, "cat /etc/os-release", "")
	if err != nil {
		return err
	}
	osID := ""
	for _, line := range strings.Split(osRelease, "\n") {
		if strings.HasPrefix(line, "ID=") {
			osID = strings.Trim(strings.TrimPrefix(line, "ID="), `"`)
		}
	}
	if osID != "ubuntu" && osID != "debian" {
		return fmt.Errorf("unsupported OS %q; use Debian or Ubuntu with systemd", osID)
	}
	uid, err := run(client, "id -u", "")
	if err != nil {
		return err
	}
	isRoot := strings.TrimSpace(uid) == "0"
	if !isRoot {
		cmd := "sudo -n true"
		input := ""
		if c.SudoPassword != "" {
			cmd = "sudo -S -p '' true"
			input = c.SudoPassword + "\n"
		}
		if _, err = run(client, cmd, input); err != nil {
			return errors.New("SSH user needs root or sudo privileges; provide sudo_password if needed")
		}
	}
	stage, err := run(client, "umask 077; mktemp -d /tmp/mp-bootstrap.XXXXXXXX", "")
	if err != nil {
		return err
	}
	stage = strings.TrimSpace(stage)
	if !regexp.MustCompile(`^/tmp/mp-bootstrap\.[A-Za-z0-9]{8}$`).MatchString(stage) {
		return errors.New("host returned unexpected staging path")
	}
	defer func() { _, _ = run(client, "rm -rf -- '"+stage+"'", "") }()
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("start SFTP (enable SSH SFTP subsystem): %w", err)
	}
	defer sftpClient.Close()
	log("Uploading agent binary and node identity over SFTP")
	if err = upload(sftpClient, stage+"/mp-agent", binary, 0700); err != nil {
		return err
	}
	cfg := model.AgentConfig{NodeID: c.NodeID, PanelURL: strings.TrimRight(c.AgentURL, "/"), DataDir: "/var/lib/mp-agent", Certificate: "/etc/mp-agent/agent.crt", PrivateKey: "/etc/mp-agent/agent.key", CA: "/etc/mp-agent/ca.crt", PollSeconds: 5, BindAddress: c.BindAddress, DownloadUserAgent: c.DownloadUserAgent}
	if cfg.BindAddress == "" {
		cfg.BindAddress = "0.0.0.0"
	}
	configJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	for name, b := range map[string][]byte{"config.json": configJSON, "agent.crt": c.Certificate, "agent.key": c.PrivateKeyPEM, "ca.crt": c.CA, "mp-agent.service": []byte(serviceUnit)} {
		if err = upload(sftpClient, stage+"/"+name, strings.NewReader(string(b)), 0600); err != nil {
			return err
		}
	}
	log("Installing Docker when absent and registering systemd agent service")
	// Only a mktemp-verified ASCII path enters this fixed script. Credentials
	// are transmitted on stdin and never placed in a command or log entry.
	script := strings.ReplaceAll(installScript, "STAGING_DIRECTORY", stage)
	cmd := "sh -s --"
	input := script
	if !isRoot {
		if c.SudoPassword != "" {
			cmd = "sudo -S -p '' sh -s --"
			input = c.SudoPassword + "\n" + script
		} else {
			cmd = "sudo -n sh -s --"
		}
	}
	if _, err = run(client, cmd, input); err != nil {
		return fmt.Errorf("bootstrap failed: %w", err)
	}
	log("Agent service started; waiting for its authenticated heartbeat")
	return nil
}

func run(client *ssh.Client, command, input string) (string, error) {
	s, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	if input != "" {
		s.Stdin = strings.NewReader(input)
	}
	b, err := s.CombinedOutput(command)
	if len(b) > 8192 {
		b = b[len(b)-8192:]
	}
	if err != nil {
		return string(b), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

func upload(client *sftp.Client, name string, reader io.Reader, mode os.FileMode) error {
	f, err := client.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, reader)
	if err == nil {
		err = f.Chmod(mode)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

const serviceUnit = `[Unit]
Description=MP Minecraft host agent
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/mp-agent --config /etc/mp-agent/config.json
Restart=on-failure
RestartSec=5
TimeoutStopSec=90
KillMode=process
UMask=0077
Environment=HOME=/var/lib/mp-agent
Environment=DOCKER_CONFIG=/var/lib/mp-agent/docker
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/mp-agent /run/docker.sock

[Install]
WantedBy=multi-user.target
`

const installScript = `set -eu
stage='STAGING_DIRECTORY'
test -d /run/systemd/system || { echo 'systemd is required' >&2; exit 1; }
. /etc/os-release
case "$ID" in ubuntu|debian) ;; *) echo 'Unsupported OS' >&2; exit 1 ;; esac
test "$(uname -m)" = x86_64 || { echo 'x86_64 is required' >&2; exit 1; }
if ! command -v docker >/dev/null 2>&1; then
  command -v apt-get >/dev/null 2>&1 || { echo 'apt-get required' >&2; exit 1; }
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends docker.io ca-certificates
fi
systemctl enable --now docker
docker info >/dev/null
if test -f /etc/mp-agent/config.json; then
  command -v openssl >/dev/null 2>&1 || { echo 'openssl required for identity recovery' >&2; exit 1; }
  existing_identity=$(openssl x509 -in /etc/mp-agent/agent.crt -noout -subject -nameopt RFC2253)
  next_identity=$(openssl x509 -in "$stage/agent.crt" -noout -subject -nameopt RFC2253)
  if ! cmp -s "$stage/ca.crt" /etc/mp-agent/ca.crt || test "$existing_identity" != "$next_identity"; then
    echo 'Agent identity already exists; remove or explicitly re-enroll the old node before replacing it' >&2
    exit 1
  fi
fi
install -d -m 0700 /etc/mp-agent /var/lib/mp-agent
install -m 0755 "$stage/mp-agent" /usr/local/bin/mp-agent.new
if test -f /usr/local/bin/mp-agent; then
  cp -p /usr/local/bin/mp-agent /usr/local/bin/mp-agent.previous
fi
mv /usr/local/bin/mp-agent.new /usr/local/bin/mp-agent
install -m 0600 "$stage/config.json" /etc/mp-agent/config.json
install -m 0600 "$stage/agent.crt" /etc/mp-agent/agent.crt
install -m 0600 "$stage/agent.key" /etc/mp-agent/agent.key
install -m 0600 "$stage/ca.crt" /etc/mp-agent/ca.crt
install -m 0644 "$stage/mp-agent.service" /etc/systemd/system/mp-agent.service
systemctl daemon-reload
systemctl enable mp-agent
systemctl restart mp-agent
systemctl is-active --quiet mp-agent
`
