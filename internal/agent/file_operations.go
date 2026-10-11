package agent

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/nwaples/rardecode/v2"
)

const (
	fileBatchLimit             = 100
	archiveEntryLimit          = 10000
	archiveExpandedLimit int64 = 2 << 30
)

type fileTransferPayload struct {
	Paths       []string `json:"paths"`
	Destination string   `json:"destination"`
	Mode        string   `json:"mode"`
}

type fileExtractPayload struct {
	Path        string `json:"path"`
	Destination string `json:"destination"`
}

type fileUploadItem struct {
	Path     string         `json:"path"`
	Artifact model.Artifact `json:"artifact"`
}

type fileUploadPayload struct {
	Files []fileUploadItem `json:"files"`
}

type fileDeletePayload struct {
	Paths []string `json:"paths"`
}

// New operations reject absolute names instead of normalizing them to relative
// names. os.Root is the second boundary, including during commit and rollback.
func operationPath(name string, allowRoot bool) (string, error) {
	if !utf8.ValidString(name) || len(name) > 4096 || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00:") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", errors.New("invalid relative file path")
	}
	if name == "" || name == "." {
		if allowRoot {
			return ".", nil
		}
		return "", errors.New("server root cannot be selected")
	}
	name = strings.TrimSuffix(name, "/")
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("invalid relative file path")
		}
	}
	return name, nil
}

func cleanSelection(paths []string) ([]string, error) {
	if len(paths) < 1 || len(paths) > fileBatchLimit {
		return nil, fmt.Errorf("select between 1 and %d paths", fileBatchLimit)
	}
	clean := make([]string, len(paths))
	for i, p := range paths {
		var err error
		clean[i], err = operationPath(p, false)
		if err != nil {
			return nil, err
		}
	}
	sorted := append([]string(nil), clean...)
	sort.Strings(sorted)
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] || strings.HasPrefix(sorted[i], sorted[i-1]+"/") {
			return nil, errors.New("selection contains duplicate or overlapping paths")
		}
	}
	// Adjacent lexical ordering alone does not catch a/b, a-b, a/b/c.
	for i, a := range clean {
		for j, b := range clean {
			if i != j && strings.HasPrefix(b, a+"/") {
				return nil, errors.New("selection contains overlapping paths")
			}
		}
	}
	return clean, nil
}

func operationRoot(dir string, name string, mustExist bool) (*os.Root, string, error) {
	r, p, err := openJail(dir, name)
	if err != nil {
		return nil, "", err
	}
	if mustExist {
		if _, err = r.Lstat(p); err != nil {
			r.Close()
			return nil, "", err
		}
	}
	return r, p, nil
}

func requireDirectory(r *os.Root, name string) error {
	info, err := r.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("destination %q is not a directory", name)
	}
	return nil
}

func newFileStage(r *os.Root) (string, *os.Root, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", nil, err
	}
	name := ".mp-files-" + hex.EncodeToString(token[:])
	if err := r.Mkdir(name, 0700); err != nil {
		return "", nil, err
	}
	stage, err := r.OpenRoot(name)
	if err != nil {
		_ = r.Remove(name)
		return "", nil, err
	}
	return name, stage, nil
}

func readDirectory(r *os.Root, name string) ([]os.DirEntry, error) {
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return entries, err
}

// validateTree checks descendants before any move or deletion starts. Files
// created by a game may include links and devices, which these operations deny.
func validateTree(ctx context.Context, r *os.Root, name string, count *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	(*count)++
	if *count > 100000 {
		return errors.New("selected tree exceeds 100000 entries")
	}
	info, err := r.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return fmt.Errorf("links and special files are forbidden: %s", name)
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := readDirectory(r, name)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err = validateTree(ctx, r, path.Join(name, e.Name()), count); err != nil {
			return err
		}
	}
	return nil
}

func copyRootTree(ctx context.Context, src, dst *os.Root, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := src.Lstat(from)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return errors.New("links and special files cannot be copied")
	}
	if info.IsDir() {
		if err = dst.Mkdir(to, 0750); err != nil {
			return err
		}
		if err = ownManagedPath(dst, to); err != nil {
			return err
		}
		entries, err := readDirectory(src, from)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err = copyRootTree(ctx, src, dst, path.Join(from, e.Name()), path.Join(to, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := src.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := dst.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, contextReader{ctx: ctx, r: in})
	if err == nil {
		err = ownManagedFile(out)
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

func destinationMissing(r *os.Root, name string) error {
	_, err := r.Lstat(name)
	if err == nil {
		return fmt.Errorf("destination already exists: %s", name)
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func copiedName(r *os.Root, destination, source string, reserved map[string]bool) (string, error) {
	info, err := r.Lstat(source)
	if err != nil {
		return "", err
	}
	base := path.Base(source)
	ext := ""
	if !info.IsDir() {
		ext = path.Ext(base)
		base = strings.TrimSuffix(base, ext)
	}
	for n := 1; n <= 10000; n++ {
		suffix := " (копия)"
		if n > 1 {
			suffix = fmt.Sprintf(" (копия %d)", n)
		}
		target := path.Join(destination, base+suffix+ext)
		if reserved[target] {
			continue
		}
		_, err := r.Lstat(target)
		if os.IsNotExist(err) {
			return target, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("cannot find an available copy name")
}

func transferFiles(ctx context.Context, dir string, p fileTransferPayload, logs *[]string) (any, error) {
	if p.Mode != "copy" && p.Mode != "move" {
		return nil, errors.New("mode must be copy or move")
	}
	selection, err := cleanSelection(p.Paths)
	if err != nil {
		return nil, err
	}
	destination, err := operationPath(p.Destination, true)
	if err != nil {
		return nil, err
	}
	r, _, err := operationRoot(dir, destination, true)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err = requireDirectory(r, destination); err != nil {
		return nil, err
	}
	targets := make([]string, len(selection))
	reserved := map[string]bool{}
	count := 0
	for i, source := range selection {
		guard, _, e := operationRoot(dir, source, true)
		if e != nil {
			return nil, e
		}
		guard.Close()
		if e = validateTree(ctx, r, source, &count); e != nil {
			return nil, e
		}
		if destination == source || strings.HasPrefix(destination, source+"/") {
			return nil, errors.New("cannot copy or move a directory into itself")
		}
		target := path.Join(destination, path.Base(source))
		if path.Dir(source) == destination {
			if p.Mode == "move" {
				return nil, errors.New("selected path is already in the destination")
			}
			target, e = copiedName(r, destination, source, reserved)
			if e != nil {
				return nil, e
			}
		}
		if reserved[target] {
			return nil, errors.New("selected paths have conflicting destination names")
		}
		if e = destinationMissing(r, target); e != nil {
			return nil, e
		}
		reserved[target] = true
		targets[i] = target
	}
	stageName, stage, err := newFileStage(r)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	defer r.RemoveAll(stageName)
	if p.Mode == "copy" {
		for i, source := range selection {
			if err = copyRootTree(ctx, r, stage, source, fmt.Sprint(i)); err != nil {
				return nil, err
			}
		}
	}
	committed := 0
	for i, source := range selection {
		if err = ctx.Err(); err == nil {
			err = destinationMissing(r, targets[i])
		}
		if err == nil {
			from := source
			if p.Mode == "copy" {
				from = path.Join(stageName, fmt.Sprint(i))
			}
			err = r.Rename(from, targets[i])
		}
		if err != nil {
			for j := committed - 1; j >= 0; j-- {
				to := selection[j]
				if p.Mode == "copy" {
					to = path.Join(stageName, fmt.Sprint(j))
				}
				if e := r.Rename(targets[j], to); e != nil {
					*logs = append(*logs, "Rollback failed for "+targets[j]+": "+e.Error())
					err = errors.Join(err, e)
				}
			}
			return map[string]any{"paths": targets[:committed]}, err
		}
		committed++
	}
	*logs = append(*logs, fmt.Sprintf("%s completed for %d paths", p.Mode, len(targets)))
	return map[string]any{"paths": targets}, nil
}

func deleteFiles(ctx context.Context, dir string, p fileDeletePayload, logs *[]string) (any, error) {
	selection, err := cleanSelection(p.Paths)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	count := 0
	for _, name := range selection {
		guard, _, e := operationRoot(dir, name, true)
		if e != nil {
			return nil, e
		}
		guard.Close()
		if e = validateTree(ctx, r, name, &count); e != nil {
			return nil, e
		}
	}
	stageName, stage, err := newFileStage(r)
	if err != nil {
		return nil, err
	}
	stage.Close()
	committed := 0
	for i, name := range selection {
		if err = ctx.Err(); err == nil {
			err = r.Rename(name, path.Join(stageName, fmt.Sprint(i)))
		}
		if err != nil {
			for j := committed - 1; j >= 0; j-- {
				if e := r.Rename(path.Join(stageName, fmt.Sprint(j)), selection[j]); e != nil {
					*logs = append(*logs, "Delete rollback failed for "+selection[j]+": "+e.Error())
					err = errors.Join(err, e)
				}
			}
			// Retain the stage if rollback failed, so no remaining selected data
			// is lost while reporting an unsuccessful operation.
			entries, e := readDirectory(r, stageName)
			if e == nil && len(entries) == 0 {
				_ = r.Remove(stageName)
			} else {
				*logs = append(*logs, "Retained recovery directory: "+stageName)
			}
			return nil, err
		}
		committed++
	}
	if err = r.RemoveAll(stageName); err != nil {
		*logs = append(*logs, "Deleted paths are retained in "+stageName)
		return map[string]any{"paths": selection}, fmt.Errorf("paths removed from directory, cleanup failed: %w", err)
	}
	*logs = append(*logs, fmt.Sprintf("Deleted %d paths", len(selection)))
	return map[string]any{"paths": selection}, nil
}

type stagedEntry struct {
	name      string
	directory bool
}

// zip.NewReader parses the directory before we can inspect its entry count.
// Bound that phase as well, so forged counts cannot allocate an unbounded index.
type archiveIndexReaderAt struct {
	ctx       context.Context
	f         *os.File
	indexing  bool
	remaining int64
}

func (r *archiveIndexReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.indexing {
		if int64(len(p)) > r.remaining {
			return 0, errors.New("ZIP metadata exceeds 64 MiB")
		}
		r.remaining -= int64(len(p))
	}
	return r.f.ReadAt(p, offset)
}

type extractionStage struct {
	root     *os.Root
	entries  []stagedEntry
	known    map[string]bool
	expanded int64
	headers  int
}

func archivePath(name string) (string, error) {
	if strings.Contains(name, "\\") {
		return "", errors.New("archive paths must use forward slashes")
	}
	return operationPath(strings.TrimSuffix(name, "/"), false)
}

func (s *extractionStage) directory(name string) error {
	parts := strings.Split(name, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		if isDir, ok := s.known[p]; ok {
			if !isDir {
				return fmt.Errorf("archive file conflicts with directory: %s", p)
			}
			continue
		}
		if len(s.entries) >= archiveEntryLimit {
			return errors.New("archive exceeds 10000 extracted entries")
		}
		if err := s.root.Mkdir(p, 0750); err != nil {
			return err
		}
		if err := ownManagedPath(s.root, p); err != nil {
			return err
		}
		s.known[p] = true
		s.entries = append(s.entries, stagedEntry{p, true})
	}
	return nil
}

func (s *extractionStage) add(ctx context.Context, name string, isDir bool, size int64, src io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.headers++
	if s.headers > archiveEntryLimit {
		return errors.New("archive exceeds 10000 entries")
	}
	clean, err := archivePath(name)
	if err != nil {
		return err
	}
	if isDir {
		return s.directory(clean)
	}
	if _, ok := s.known[clean]; ok {
		return fmt.Errorf("duplicate archive path: %s", clean)
	}
	if size < 0 || size > archiveExpandedLimit-s.expanded {
		return errors.New("archive expanded size exceeds 2 GiB")
	}
	if parent := path.Dir(clean); parent != "." {
		if err = s.directory(parent); err != nil {
			return err
		}
	}
	if len(s.entries) >= archiveEntryLimit {
		return errors.New("archive exceeds 10000 extracted entries")
	}
	f, err := s.root.OpenFile(clean, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(contextReader{ctx: ctx, r: src}, size+1))
	if err == nil && n > archiveExpandedLimit-s.expanded {
		err = errors.New("archive expanded size exceeds 2 GiB")
	}
	if err == nil && n != size {
		err = errors.New("archive entry size mismatch")
	}
	if err == nil {
		err = ownManagedFile(f)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	s.expanded += n
	s.known[clean] = false
	s.entries = append(s.entries, stagedEntry{clean, false})
	return nil
}

func unpackArchive(ctx context.Context, r *os.Root, archive string, s *extractionStage) error {
	f, err := r.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("archive must be a regular file")
	}
	switch strings.ToLower(path.Ext(archive)) {
	case ".zip":
		index := &archiveIndexReaderAt{ctx: ctx, f: f, indexing: true, remaining: 64 << 20}
		zr, err := zip.NewReader(index, info.Size())
		if err != nil {
			return fmt.Errorf("read ZIP: %w", err)
		}
		index.indexing = false
		if len(zr.File) > archiveEntryLimit {
			return errors.New("archive exceeds 10000 entries")
		}
		for _, entry := range zr.File {
			if entry.Flags&1 != 0 {
				return errors.New("encrypted ZIP archives are not supported")
			}
			mode := entry.Mode()
			if mode&os.ModeSymlink != 0 || (!mode.IsDir() && !mode.IsRegular()) {
				return errors.New("archive links and special files are forbidden")
			}
			if entry.UncompressedSize64 > uint64(archiveExpandedLimit) {
				return errors.New("archive expanded size exceeds 2 GiB")
			}
			if mode.IsDir() {
				if err = s.add(ctx, entry.Name, true, 0, nil); err != nil {
					return err
				}
				continue
			}
			reader, err := entry.Open()
			if err != nil {
				return err
			}
			err = s.add(ctx, entry.Name, false, int64(entry.UncompressedSize64), reader)
			if e := reader.Close(); err == nil {
				err = e
			}
			if err != nil {
				return err
			}
		}
	case ".rar":
		rr, err := rardecode.NewReader(contextReader{ctx: ctx, r: f}, rardecode.MaxDictionarySize(64<<20))
		if err != nil {
			return fmt.Errorf("read RAR (unencrypted, single-volume archives only): %w", err)
		}
		for {
			header, err := rr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("read RAR (unencrypted, single-volume archives only): %w", err)
			}
			if header.Encrypted || header.HeaderEncrypted {
				return errors.New("encrypted RAR archives are not supported")
			}
			mode := header.Mode()
			if header.LinkType != rardecode.LinkTypeNone || mode&os.ModeSymlink != 0 || (!header.IsDir && !mode.IsRegular()) {
				return errors.New("archive links and special files are forbidden")
			}
			// rardecode.Mode reports symlinks but does not translate Unix FIFO,
			// socket or device type bits. Check the original mode as well.
			if header.HostOS == rardecode.HostOSUnix {
				kind := header.Attributes & 0xF000
				if kind != 0 && kind != 0x8000 && kind != 0x4000 {
					return errors.New("archive links and special files are forbidden")
				}
				if (kind == 0x4000 && !header.IsDir) || (kind == 0x8000 && header.IsDir) {
					return errors.New("inconsistent RAR entry type")
				}
			}
			if header.UnKnownSize {
				return errors.New("RAR entries with unknown expanded size are not supported")
			}
			if err = s.add(ctx, header.Name, header.IsDir, header.UnPackedSize, rr); err != nil {
				return err
			}
		}
	default:
		return errors.New("only ZIP and RAR archives can be extracted")
	}
	if len(s.entries) == 0 {
		return errors.New("archive contains no entries")
	}
	return nil
}

// Commit merges existing directories but never existing files. Only paths this
// command created are rolled back, through the same jailed root handle.
func commitEntries(ctx context.Context, dir string, r *os.Root, stageName, destination string, entries []stagedEntry, logs *[]string) error {
	for _, entry := range entries {
		target := path.Join(destination, entry.name)
		guard, _, err := openJail(dir, target)
		if err != nil {
			return err
		}
		guard.Close()
		info, err := r.Lstat(target)
		if err == nil {
			if !entry.directory || !info.IsDir() {
				return fmt.Errorf("destination already exists: %s", target)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	created := []stagedEntry{}
	rollback := func(cause error) error {
		for i := len(created) - 1; i >= 0; i-- {
			entry := created[i]
			var err error
			if entry.directory {
				err = r.Remove(entry.name)
			} else {
				original := entry.name
				if destination != "." {
					original = strings.TrimPrefix(original, destination+"/")
				}
				err = r.Rename(entry.name, path.Join(stageName, original))
			}
			if err != nil {
				*logs = append(*logs, "Rollback failed for "+entry.name+": "+err.Error())
				cause = errors.Join(cause, err)
			}
		}
		return cause
	}
	// Destination ancestors are part of the transaction as well.
	if destination != "." {
		parts := strings.Split(destination, "/")
		for i := range parts {
			p := strings.Join(parts[:i+1], "/")
			info, err := r.Lstat(p)
			if err == nil {
				if !info.IsDir() {
					return rollback(errors.New("destination is not a directory"))
				}
				continue
			}
			if !os.IsNotExist(err) {
				return rollback(err)
			}
			if err = r.Mkdir(p, 0750); err != nil {
				return rollback(err)
			}
			created = append(created, stagedEntry{p, true})
			if err = ownManagedPath(r, p); err != nil {
				return rollback(err)
			}
		}
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return rollback(err)
		}
		target := path.Join(destination, entry.name)
		if entry.directory {
			if _, err := r.Lstat(target); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				return rollback(err)
			}
			if err := r.Mkdir(target, 0750); err != nil {
				return rollback(err)
			}
			created = append(created, stagedEntry{target, true})
			if err := ownManagedPath(r, target); err != nil {
				return rollback(err)
			}
			continue
		} else {
			if err := destinationMissing(r, target); err != nil {
				return rollback(err)
			}
			if err := r.Rename(path.Join(stageName, entry.name), target); err != nil {
				return rollback(err)
			}
		}
		created = append(created, stagedEntry{target, entry.directory})
	}
	return nil
}

func extractFiles(ctx context.Context, dir string, p fileExtractPayload, logs *[]string) (any, error) {
	archive, err := operationPath(p.Path, false)
	if err != nil {
		return nil, err
	}
	destination, err := operationPath(p.Destination, true)
	if err != nil {
		return nil, err
	}
	r, _, err := operationRoot(dir, archive, true)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	guard, _, err := openJail(dir, destination)
	if err != nil {
		return nil, err
	}
	guard.Close()
	stageName, stage, err := newFileStage(r)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	defer r.RemoveAll(stageName)
	s := &extractionStage{root: stage, known: map[string]bool{}}
	if err = unpackArchive(ctx, r, archive, s); err != nil {
		return nil, err
	}
	if err = commitEntries(ctx, dir, r, stageName, destination, s.entries, logs); err != nil {
		return nil, err
	}
	*logs = append(*logs, fmt.Sprintf("Extracted %d entries (%d bytes) into %s", len(s.entries), s.expanded, destination))
	return map[string]any{"destination": destination, "entries": len(s.entries), "bytes": s.expanded}, nil
}

func (a *Agent) downloadEmptyFile(ctx context.Context, dir, target string, artifact model.Artifact) error {
	u, err := url.Parse(artifact.URL)
	panel, _ := url.Parse(a.cfg.PanelURL)
	if err != nil || u.Scheme != "https" || u.User != nil || !identifier.MatchString(artifact.ID) || u.Host != panel.Host || u.Path != "/agent/v1/artifacts/"+artifact.ID || u.RawQuery != "" {
		return errors.New("invalid upload artifact endpoint")
	}
	client := &http.Client{Transport: a.client.Transport, Timeout: 30 * time.Minute, CheckRedirect: a.client.CheckRedirect}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", a.cfg.DownloadUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("artifact download HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1))
	if err != nil {
		return err
	}
	h := sha256.Sum256(nil)
	if len(b) != 0 || !strings.EqualFold(artifact.SHA256, hex.EncodeToString(h[:])) {
		return errors.New("empty artifact size or SHA256 mismatch")
	}
	return jailedWrite(dir, target, strings.NewReader(""), 0, true)
}

func (a *Agent) uploadFiles(ctx context.Context, dir string, p fileUploadPayload, logs *[]string) (any, error) {
	paths := make([]string, len(p.Files))
	for i, f := range p.Files {
		paths[i] = f.Path
	}
	selection, err := cleanSelection(paths)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for i, f := range p.Files {
		if !identifier.MatchString(f.Artifact.ID) || len(f.Artifact.SHA256) != 64 || f.Artifact.Size < 0 || f.Artifact.Size > 256<<20 {
			return nil, errors.New("upload needs a verified panel artifact, at most 256 MiB per file")
		}
		if _, err = hex.DecodeString(f.Artifact.SHA256); err != nil {
			return nil, errors.New("invalid artifact SHA256")
		}
		guard, _, err := openJail(dir, selection[i])
		if err != nil {
			return nil, err
		}
		guard.Close()
		if err = destinationMissing(r, selection[i]); err != nil {
			return nil, err
		}
	}
	stageName, stage, err := newFileStage(r)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	defer r.RemoveAll(stageName)
	s := &extractionStage{root: stage, known: map[string]bool{}}
	for i, f := range p.Files {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		name := selection[i]
		if parent := path.Dir(name); parent != "." {
			if err = s.directory(parent); err != nil {
				return nil, err
			}
		}
		stageDir := filepath.Join(dir, stageName)
		if f.Artifact.Size == 0 {
			err = a.downloadEmptyFile(ctx, stageDir, name, f.Artifact)
		} else {
			err = a.download(ctx, stageDir, name, f.Artifact, true)
		}
		if err != nil {
			return nil, fmt.Errorf("upload %s: %w", name, err)
		}
		if err = ownManagedPath(stage, name); err != nil {
			return nil, err
		}
		s.known[name] = false
		s.entries = append(s.entries, stagedEntry{name, false})
	}
	if err = commitEntries(ctx, dir, r, stageName, ".", s.entries, logs); err != nil {
		return nil, err
	}
	*logs = append(*logs, fmt.Sprintf("Uploaded %d files", len(selection)))
	return map[string]any{"paths": selection}, nil
}
