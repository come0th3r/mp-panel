package agent

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/come0th3r/mp-panel/internal/model"
)

const textLimit = 2 << 20

func cleanPath(name string) (string, error) {
	if strings.ContainsAny(name, "\\\x00") {
		return "", errors.New("invalid file path")
	}
	name = strings.TrimPrefix(name, "/")
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", errors.New("parent path traversal is forbidden")
		}
	}
	name = path.Clean(name)
	if name == "/" || strings.HasPrefix(name, "../") {
		return "", errors.New("invalid file path")
	}
	return name, nil
}

func (a *Agent) serverDir(id string) (string, error) {
	if !identifier.MatchString(id) {
		return "", errors.New("invalid server ID")
	}
	return filepath.Join(a.cfg.DataDir, "servers", id), nil
}

func openJail(dir, name string) (*os.Root, string, error) {
	clean, err := cleanPath(name)
	if err != nil {
		return nil, "", err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	// Reject all links, including internal ones. Root provides a second traversal
	// boundary for reads even if a running game replaces a path concurrently.
	current := ""
	if clean != "." {
		for _, part := range strings.Split(clean, "/") {
			current = path.Join(current, part)
			info, err := r.Lstat(current)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				r.Close()
				return nil, "", err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				r.Close()
				return nil, "", errors.New("symbolic links are not supported")
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				r.Close()
				return nil, "", errors.New("special files are forbidden")
			}
		}
	}
	return r, clean, nil
}

func readText(dir, name string) (model.FileContent, error) {
	r, p, err := openJail(dir, name)
	if err != nil {
		return model.FileContent{}, err
	}
	defer r.Close()
	f, err := r.Open(p)
	if err != nil {
		return model.FileContent{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return model.FileContent{}, err
	}
	if !info.Mode().IsRegular() {
		return model.FileContent{}, errors.New("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, textLimit+1))
	if err != nil {
		return model.FileContent{}, err
	}
	if len(b) > textLimit {
		return model.FileContent{}, errors.New("text file exceeds 2 MiB")
	}
	if !utf8.Valid(b) {
		return model.FileContent{}, errors.New("file is not UTF-8 text")
	}
	h := sha256.Sum256(b)
	return model.FileContent{Path: p, Content: string(b), Revision: hex.EncodeToString(h[:])}, nil
}

func writeText(dir, name, content, revision string) error {
	if len(content) > textLimit || !utf8.ValidString(content) {
		return errors.New("text must be UTF-8 and at most 2 MiB")
	}
	if revision != "" {
		old, err := readText(dir, name)
		if err != nil {
			return err
		}
		if old.Revision != revision {
			return errors.New("revision conflict: file changed since it was read")
		}
	}
	return jailedWrite(dir, name, strings.NewReader(content), int64(len(content)), false)
}

func jailedWrite(dir, name string, src io.Reader, limit int64, exclusive bool) error {
	r, p, err := openJail(dir, name)
	if err != nil {
		return err
	}
	defer r.Close()
	if p == "." {
		return errors.New("cannot write root directory")
	}
	if exclusive {
		if _, err = r.Lstat(p); err == nil {
			return errors.New("destination already exists")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return err
	}
	tmp := path.Join(path.Dir(p), ".mp-write-"+hex.EncodeToString(token[:]))
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return err
	}
	defer r.Remove(tmp)
	n, err := io.Copy(f, io.LimitReader(src, limit+1))
	if err == nil && n > limit {
		err = errors.New("file exceeds size limit")
	}
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// Mutations run only while the container is stopped and under its command
	// lock; neither Minecraft nor another MP command can swap path components.
	return r.Rename(tmp, p)
}

func listFiles(dir, name string) ([]model.FileEntry, error) {
	r, p, err := openJail(dir, name)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]model.FileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			continue
		}
		out = append(out, model.FileEntry{Name: entry.Name(), Path: path.Join(p, entry.Name()), IsDir: info.IsDir(), Size: info.Size(), Modified: info.ModTime().UTC().Format(time.RFC3339)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func makeDir(dir, name string) error {
	r, p, err := openJail(dir, name)
	if err != nil {
		return err
	}
	defer r.Close()
	current := ""
	for _, part := range strings.Split(p, "/") {
		current = path.Join(current, part)
		if err = r.Mkdir(current, 0750); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func removePath(dir, name string) error {
	r, p, err := openJail(dir, name)
	if err != nil {
		return err
	}
	defer r.Close()
	if p == "." {
		return errors.New("cannot delete server root")
	}
	return removeRoot(r, p)
}

func removeRoot(r *os.Root, p string) error {
	info, err := r.Lstat(p)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to remove symbolic link")
	}
	if info.IsDir() {
		f, err := r.Open(p)
		if err != nil {
			return err
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err = removeRoot(r, path.Join(p, e.Name())); err != nil {
				return err
			}
		}
	}
	return r.Remove(p)
}

func renamePath(dir, old, new string) error {
	r, p, err := openJail(dir, old)
	if err != nil {
		return err
	}
	defer r.Close()
	r2, q, err := openJail(dir, new)
	if err != nil {
		return err
	}
	defer r2.Close()
	if p == "." || q == "." {
		return errors.New("cannot rename server root")
	}
	if _, err = r2.Lstat(q); err == nil {
		return errors.New("destination already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	return r.Rename(p, q)
}

func parseProperties(content string) (map[string]string, error) {
	properties := map[string]string{}
	scan := bufio.NewScanner(strings.NewReader(content))
	scan.Buffer(make([]byte, 4096), textLimit)
	pending := ""
	for scan.Scan() {
		line := strings.TrimLeft(scan.Text(), " \t\f")
		if pending != "" {
			line = pending + line
			pending = ""
		}
		count := 0
		for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
			count++
		}
		if count%2 == 1 {
			pending = line[:len(line)-1]
			continue
		}
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		split := len(line)
		escaped := false
		for i, c := range line {
			if !escaped && (c == '=' || c == ':' || c == ' ' || c == '\t' || c == '\f') {
				split = i
				break
			}
			if c == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
		}
		key, err := unescapeProperty(line[:split])
		if err != nil {
			return nil, err
		}
		rest := strings.TrimLeft(line[split:], " \t\f")
		if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ":") {
			rest = rest[1:]
		}
		rest = strings.TrimLeft(rest, " \t\f")
		value, err := unescapeProperty(rest)
		if err != nil {
			return nil, err
		}
		properties[key] = value
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	if pending != "" {
		return nil, errors.New("unfinished property continuation")
	}
	return properties, nil
}

func unescapeProperty(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			return "", errors.New("invalid property escape")
		}
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		case 'f':
			b.WriteByte('\f')
		case 'u':
			if i+4 >= len(s) {
				return "", errors.New("invalid Unicode property escape")
			}
			n, err := strconv.ParseUint(s[i+1:i+5], 16, 16)
			if err != nil {
				return "", err
			}
			b.WriteRune(rune(n))
			i += 4
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), nil
}

func encodeProperties(properties map[string]string) string {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(escapeProperty(key))
		b.WriteByte('=')
		b.WriteString(escapeProperty(properties[key]))
		b.WriteByte('\n')
	}
	return b.String()
}

func escapeProperty(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch c {
		case '\\', '=', ':', ' ', '#', '!':
			b.WriteByte('\\')
			b.WriteRune(c)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 32 {
				fmt.Fprintf(&b, `\u%04x`, c)
			} else {
				b.WriteRune(c)
			}
		}
	}
	return b.String()
}
