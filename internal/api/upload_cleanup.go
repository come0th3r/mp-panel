package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/jackc/pgx/v5"
)

var uploadIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Match the same assignments the agent artifact endpoint authorizes. Completed
// command history is retained for a week, so a missing reference alone never
// proves that a newly uploaded file can be removed.
const uploadReferences = `c.payload->'payload'->'artifact'->>'id'=$1 OR EXISTS (
 SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(c.payload->'payload'->'files')='array'
 THEN c.payload->'payload'->'files' ELSE '[]'::jsonb END) f WHERE f->'artifact'->>'id'=$1)
 OR EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(c.payload->'install'->'artifacts','[]'::jsonb)
 ||COALESCE(c.payload->'install'->'extra_jars','[]'::jsonb)) f WHERE f->>'id'=$1)`

func commandUploadIDs(cmd model.Command) []string {
	ids := map[string]bool{}
	add := func(asset model.Artifact) {
		if uploadIDPattern.MatchString(asset.ID) {
			ids[asset.ID] = true
		}
	}
	if cmd.Install != nil {
		for _, asset := range append(append([]model.Artifact{}, cmd.Install.Artifacts...), cmd.Install.ExtraJars...) {
			add(asset)
		}
	}
	var payload struct {
		Artifact model.Artifact `json:"artifact"`
		Files    []struct {
			Artifact model.Artifact `json:"artifact"`
		} `json:"files"`
	}
	if json.Unmarshal(cmd.Payload, &payload) == nil {
		add(payload.Artifact)
		for _, file := range payload.Files {
			add(file.Artifact)
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (a *API) lockUploads(ctx context.Context, tx pgx.Tx, cmd model.Command) error {
	ids := commandUploadIDs(cmd)
	if len(ids) == 0 {
		return nil
	}
	// Hold these locks until the command is committed. Cleanup locks each upload
	// exclusively and rechecks references in a separate, fresh SQL statement.
	rows, err := tx.Query(ctx, `SELECT id,payload FROM mp_entities WHERE kind='upload' AND id=ANY($1) ORDER BY id FOR KEY SHARE`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return err
		}
		var upload storedUpload
		if json.Unmarshal(raw, &upload) != nil || upload.ID != id || upload.Path != filepath.Join(a.Config.DataDir, "uploads", id) {
			return fmt.Errorf("Загруженный файл недоступен. Загрузите его заново")
		}
		info, err := os.Lstat(upload.Path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("Загруженный файл недоступен. Загрузите его заново")
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count != len(ids) {
		return fmt.Errorf("Срок хранения загрузки истёк. Загрузите файл заново")
	}
	return nil
}

func (a *API) cleanupUploads(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	rows, err := a.Store.Pool.Query(ctx, `SELECT id FROM mp_entities WHERE kind='upload' AND created_at<now()-interval '10 minutes' AND id>$1 ORDER BY id LIMIT 500`, a.uploadCursor)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		a.uploadCursor = ""
		return nil
	}
	root, err := os.OpenRoot(a.Config.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat("uploads")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("uploads must be a regular directory")
	}
	uploads, err := root.OpenRoot("uploads")
	if err != nil {
		return err
	}
	defer uploads.Close()
	var removed int
	var bytes int64
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		a.uploadCursor = id
		size, ok, err := a.cleanupUpload(ctx, uploads, id)
		if err != nil {
			log.Printf("upload cleanup %s: %v", id, err)
			continue
		}
		if ok {
			removed++
			bytes += size
		}
	}
	if removed > 0 {
		log.Printf("upload cleanup: removed %d temporary copies, %d bytes", removed, bytes)
	}
	return ctx.Err()
}

func (a *API) cleanupUpload(ctx context.Context, root *os.Root, id string) (int64, bool, error) {
	if !uploadIDPattern.MatchString(id) {
		return 0, false, nil
	}
	tx, err := a.Store.Pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	var created time.Time
	err = tx.QueryRow(ctx, `SELECT payload,created_at FROM mp_entities WHERE kind='upload' AND id=$1 FOR UPDATE SKIP LOCKED`, id).Scan(&raw, &created)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var upload storedUpload
	if json.Unmarshal(raw, &upload) != nil || upload.ID != id || upload.Path != filepath.Join(a.Config.DataDir, "uploads", id) {
		return 0, false, fmt.Errorf("unexpected upload metadata")
	}
	now := time.Now()
	eligible := created.Before(now.Add(-24 * time.Hour))
	rows, err := tx.Query(ctx, `SELECT c.state,c.deadline,c.created_at,c.result,COALESCE(j.payload->>'updated_at','')
 FROM mp_commands c LEFT JOIN mp_entities j ON j.kind='job' AND j.id=c.job_id WHERE (`+uploadReferences+`)`, id)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var state, updated string
		var deadline, started time.Time
		var result []byte
		if err = rows.Scan(&state, &deadline, &started, &result, &updated); err != nil {
			return 0, false, err
		}
		if state == "pending" || state == "delivered" {
			return 0, false, nil
		}
		if state == "expired" {
			// An agent can still be finishing a timed-out operation.
			if deadline.After(now.Add(-24 * time.Hour)) {
				return 0, false, nil
			}
			continue
		}
		if state != "completed" {
			return 0, false, nil
		}
		finished, err := time.Parse(time.RFC3339, updated)
		if err != nil {
			// With no reliable completion timestamp, retain the copy until the
			// command's deadline plus a day, even if it was created long ago.
			if deadline.After(now.Add(-24 * time.Hour)) {
				return 0, false, nil
			}
			continue
		}
		var outcome model.CommandResult
		if json.Unmarshal(result, &outcome) != nil {
			return 0, false, nil
		}
		grace := 24 * time.Hour
		if outcome.Success {
			grace = 10 * time.Minute
		}
		if finished.After(now.Add(-grace)) {
			return 0, false, nil
		}
		if outcome.Success {
			eligible = true
		}
	}
	if err = rows.Err(); err != nil {
		return 0, false, err
	}
	rows.Close()
	if !eligible {
		return 0, false, nil
	}
	info, err := root.Lstat(id)
	if err != nil && !os.IsNotExist(err) {
		return 0, false, err
	}
	var size int64
	if err == nil {
		if !info.Mode().IsRegular() {
			return 0, false, fmt.Errorf("upload is not a regular file")
		}
		size = info.Size()
		if err = root.Remove(id); err != nil {
			return 0, false, err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM mp_entities WHERE kind='upload' AND id=$1`, id); err != nil {
		return 0, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return size, true, nil
}
