package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
)

type backupRecord struct {
	Backup model.Backup `json:"backup"`
	Spec   serverSpec   `json:"spec"`
}

func (a *Agent) backup(ctx context.Context, id, backupID, name string) (model.Backup, error) {
	var backup model.Backup
	if !identifier.MatchString(backupID) {
		return backup, errors.New("valid backup_id required")
	}
	if err := a.requireStopped(ctx, id); err != nil {
		return backup, err
	}
	spec, err := a.readSpec(id)
	if err != nil {
		return backup, err
	}
	dir, err := a.serverDir(id)
	if err != nil {
		return backup, err
	}
	backupDir := filepath.Join(a.cfg.DataDir, "backups", id)
	if err = os.MkdirAll(backupDir, 0700); err != nil {
		return backup, err
	}
	target := filepath.Join(backupDir, backupID+".tar.gz")
	if _, err = os.Stat(target); err == nil {
		return backup, errors.New("backup already exists")
	} else if !os.IsNotExist(err) {
		return backup, err
	}
	f, err := os.CreateTemp(backupDir, ".mp-backup-*")
	if err != nil {
		return backup, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	hasher := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(f, hasher))
	tw := tar.NewWriter(gz)
	r, err := os.OpenRoot(dir)
	if err != nil {
		f.Close()
		return backup, err
	}
	defer r.Close()
	err = filepath.WalkDir(dir, func(p string, entry os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		relative, e := filepath.Rel(dir, p)
		if e != nil {
			return e
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("backup refuses symbolic links")
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("backup refuses special files")
		}
		header, e := tar.FileInfoHeader(info, "")
		if e != nil {
			return e
		}
		header.Name = filepath.ToSlash(relative)
		header.Uid = 1000
		header.Gid = 1000
		header.Uname = ""
		header.Gname = ""
		if e = tw.WriteHeader(header); e != nil {
			return e
		}
		if info.IsDir() {
			return nil
		}
		in, e := r.Open(relative)
		if e != nil {
			return e
		}
		_, e = io.Copy(tw, contextReader{ctx: ctx, r: in})
		closeErr := in.Close()
		if e == nil {
			e = closeErr
		}
		return e
	})
	if e := tw.Close(); err == nil {
		err = e
	}
	if e := gz.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = f.Sync()
	}
	info, e := f.Stat()
	if err == nil {
		err = e
	}
	if e = f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return backup, err
	}
	if name == "" {
		name = "Snapshot " + time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	}
	backup = model.Backup{ID: backupID, ServerID: id, NodeID: a.cfg.NodeID, Name: name, Size: info.Size(), SHA256: hex.EncodeToString(hasher.Sum(nil)), CreatedAt: time.Now().UTC().Format(time.RFC3339), Path: target}
	if err = os.Rename(tmp, target); err != nil {
		return model.Backup{}, err
	}
	if err = atomicJSON(filepath.Join(backupDir, backupID+".json"), backupRecord{Backup: backup, Spec: spec}, 0600); err != nil {
		_ = os.Remove(target)
		return model.Backup{}, err
	}
	return backup, nil
}

func (a *Agent) restore(ctx context.Context, id, backupID string, logs *[]string) error {
	if !identifier.MatchString(backupID) {
		return errors.New("invalid backup ID")
	}
	if err := a.requireStopped(ctx, id); err != nil {
		return err
	}
	backupDir := filepath.Join(a.cfg.DataDir, "backups", id)
	b, err := os.ReadFile(filepath.Join(backupDir, backupID+".json"))
	if err != nil {
		return err
	}
	var record backupRecord
	if err = json.Unmarshal(b, &record); err != nil {
		return err
	}
	if record.Backup.ID != backupID || record.Backup.ServerID != id || record.Backup.NodeID != a.cfg.NodeID || record.Spec.Server.ID != id {
		return errors.New("backup identity mismatch")
	}
	f, err := os.Open(filepath.Join(backupDir, backupID+".tar.gz"))
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, contextReader{ctx: ctx, r: f}); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), record.Backup.SHA256) {
		return errors.New("backup checksum mismatch")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Join(a.cfg.DataDir, "servers"), ".restore-"+id+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var expanded int64
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		header, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if filepath.IsAbs(header.Name) || strings.HasPrefix(header.Name, "/") {
			return errors.New("absolute archive path forbidden")
		}
		clean, e := cleanPath(header.Name)
		if e != nil || clean == "." {
			return errors.New("unsafe archive path")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if e = makeDir(stage, clean); e != nil {
				return e
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > 1<<40 || expanded > 1<<40-header.Size {
				return errors.New("backup exceeds 1 TiB extraction budget")
			}
			expanded += header.Size
			if e = makeDir(stage, filepath.ToSlash(filepath.Dir(clean))); e != nil {
				return e
			}
			if e = jailedWrite(stage, clean, contextReader{ctx: ctx, r: tr}, header.Size, true); e != nil {
				return e
			}
		default:
			return fmt.Errorf("archive entry type %d is forbidden; links and devices are not restored", header.Typeflag)
		}
	}
	if err = prepareOwnership(stage); err != nil {
		return err
	}
	if err = a.removeContainer(ctx, id, logs); err != nil {
		return err
	}
	dir, _ := a.serverDir(id)
	old := dir + ".before-restore"
	if _, err = os.Stat(old); err == nil {
		return errors.New("interrupted restore directory exists; recover it before retrying")
	}
	if err = os.Rename(dir, old); err != nil {
		return err
	}
	if err = os.Rename(stage, dir); err != nil {
		_ = os.Rename(old, dir)
		return err
	}
	if err = atomicJSON(a.specPath(id), record.Spec, 0600); err != nil {
		_ = os.Rename(dir, stage)
		_ = os.Rename(old, dir)
		return err
	}
	if err = os.RemoveAll(old); err != nil {
		*logs = append(*logs, "Restored successfully; prior directory requires cleanup: "+err.Error())
	}
	*logs = append(*logs, "Offline snapshot restored, including its original runtime profile; server remains stopped")
	return nil
}
