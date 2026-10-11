package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
)

func (a *Agent) gameEndpoint(port int) string {
	address := a.cfg.BindAddress
	if address == "0.0.0.0" {
		address = "127.0.0.1"
	} else if address == "::" {
		address = "::1"
	}
	return net.JoinHostPort(address, strconv.Itoa(port))
}

func (a *Agent) waitReady(ctx context.Context, id string, s model.Server, logs *[]string) error {
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	*logs = append(*logs, "Waiting for Minecraft startup and a responding game endpoint")
	for {
		inspect, exists, err := a.inspect(readyCtx, id)
		if err != nil {
			return err
		}
		if !exists || !inspect.State.Running {
			tail, _ := docker(readyCtx, nil, "logs", "--tail", "100", containerName(id))
			*logs = append(*logs, splitLines(tail)...)
			return fmt.Errorf("server exited before readiness: exit=%d, OOM=%t, %s", inspect.State.ExitCode, inspect.State.OOMKilled, inspect.State.Error)
		}
		tail, tailErr := docker(readyCtx, nil, "logs", "--since", inspect.State.StartedAt, "--tail", "250", containerName(id))
		if statusPing(readyCtx, a.gameEndpoint(s.Port)) == nil || (tailErr == nil && strings.Contains(tail, "Done (") && tcpReachable(readyCtx, a.gameEndpoint(s.Port))) {
			if err = atomicWrite(filepath.Join(a.cfg.DataDir, "specs", id+".ready"), []byte(inspect.State.StartedAt), 0600); err != nil {
				return err
			}
			*logs = append(*logs, "Minecraft game endpoint is ready")
			return nil
		}
		if !pause(readyCtx, 2*time.Second) {
			*logs = append(*logs, splitLines(tail)...)
			return fmt.Errorf("Minecraft did not become ready before startup deadline: %w", readyCtx.Err())
		}
	}
}

func tcpReachable(ctx context.Context, address string) bool {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// A status handshake checks the Minecraft listener, including custom cores
// that do not print Vanilla's Done marker. It does not depend on login/auth.
func statusPing(ctx context.Context, address string) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	if t, ok := ctx.Deadline(); ok && t.Before(deadline) {
		deadline = t
	}
	_ = conn.SetDeadline(deadline)
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return err
	}
	packet := appendVarInt(nil, 0)
	packet = appendVarInt(packet, -1)
	packet = appendVarInt(packet, int32(len(host)))
	packet = append(packet, []byte(host)...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	packet = append(packet, portBytes[:]...)
	packet = appendVarInt(packet, 1)
	frame := appendVarInt(nil, int32(len(packet)))
	frame = append(frame, packet...)
	frame = append(frame, 1, 0)
	if _, err = conn.Write(frame); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	length, err := readVarInt(reader)
	if err != nil {
		return err
	}
	if length < 1 || length > 1<<20 {
		return errors.New("invalid Minecraft status response length")
	}
	limited := io.LimitReader(reader, int64(length))
	body, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(body) != int(length) {
		return io.ErrUnexpectedEOF
	}
	status := bufio.NewReader(strings.NewReader(string(body)))
	id, err := readVarInt(status)
	if err != nil || id != 0 {
		return errors.New("unexpected Minecraft status packet")
	}
	jsonLength, err := readVarInt(status)
	if err != nil || jsonLength < 1 || jsonLength > length {
		return errors.New("invalid Minecraft status JSON length")
	}
	payload := make([]byte, jsonLength)
	if _, err = io.ReadFull(status, payload); err != nil {
		return err
	}
	var response struct {
		Version *struct {
			Name     string `json:"name"`
			Protocol int    `json:"protocol"`
		} `json:"version"`
		Players *struct {
			Max    int `json:"max"`
			Online int `json:"online"`
		} `json:"players"`
	}
	if err = json.Unmarshal(payload, &response); err != nil {
		return err
	}
	if response.Version == nil || response.Players == nil {
		return errors.New("Minecraft status is incomplete")
	}
	return nil
}

func appendVarInt(dst []byte, n int32) []byte {
	value := uint32(n)
	for {
		b := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			b |= 0x80
		}
		dst = append(dst, b)
		if value == 0 {
			return dst
		}
	}
}
func readVarInt(r io.ByteReader) (int32, error) {
	var value uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		value |= uint32(b&0x7f) << uint(i*7)
		if b&0x80 == 0 {
			return int32(value), nil
		}
	}
	return 0, errors.New("Minecraft VarInt exceeds five bytes")
}
