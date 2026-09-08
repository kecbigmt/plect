package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type sidecarPendingDelivery struct {
	Subscribe   map[string][]string `json:"subscribe"`
	Unsubscribe map[string][]string `json:"unsubscribe"`
}

type sidecarChainAttempt struct {
	session     string
	instance    string
	chainID     string
	generation  string
	fingerprint string
}

type sidecarRetry struct {
	session  string
	action   string
	resource string
}

type sidecarFiles struct {
	paths []string
	dirs  []string
}

// ImportSidecars moves post-cutover JSON sidecars into their tables. It is
// safe to rerun after an interruption: rows are upserted before source files
// are removed, so a second run only repeats idempotent inserts and cleanup.
func (db *DB) ImportSidecars(ctx context.Context, dir string) error {
	unlockCoord, err := db.gate.acquireCoordinationExclusive(ctx)
	if err != nil {
		return err
	}
	defer unlockCoord()
	unlockAccess, err := db.gate.accessExclusive()
	if err != nil {
		return err
	}
	defer unlockAccess()

	chains, retries, files, err := readSidecars(dir)
	if err != nil {
		return err
	}
	if len(files.paths) == 0 && len(files.dirs) == 0 {
		return nil
	}

	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sidecar import: %w", err)
	}
	if err := importSidecarsTx(ctx, tx, chains, retries, files); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return fmt.Errorf("%w (rollback also failed: %v)", err, rollbackErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sidecar import: %w", err)
	}
	sortSidecarFiles(&files)
	for _, path := range files.paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove imported sidecar %s: %w", path, err)
		}
	}
	for _, dir := range files.dirs {
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove retired events directory %s: %w", dir, err)
		}
	}
	return nil
}

func readSidecars(dir string) ([]sidecarChainAttempt, []sidecarRetry, sidecarFiles, error) {
	var chains []sidecarChainAttempt
	var retries []sidecarRetry
	files := sidecarFiles{}
	pendingPath := filepath.Join(dir, "pending_delivery.json")
	if data, err := os.ReadFile(pendingPath); err == nil {
		var pending sidecarPendingDelivery
		if err := json.Unmarshal(data, &pending); err != nil {
			return nil, nil, files, fmt.Errorf("read subscription retries from %s: %w", pendingPath, err)
		}
		for session, resources := range pending.Subscribe {
			for _, resource := range resources {
				retries = append(retries, sidecarRetry{session: session, action: subscriptionRetrySubscribe, resource: resource})
			}
		}
		for session, resources := range pending.Unsubscribe {
			for _, resource := range resources {
				retries = append(retries, sidecarRetry{session: session, action: subscriptionRetryUnsubscribe, resource: resource})
			}
		}
		files.paths = append(files.paths, pendingPath)
	} else if !os.IsNotExist(err) {
		return nil, nil, files, fmt.Errorf("read subscription retries from %s: %w", pendingPath, err)
	}
	if _, err := os.Stat(pendingPath + ".lock"); err == nil {
		files.paths = append(files.paths, pendingPath+".lock")
	} else if !os.IsNotExist(err) {
		return nil, nil, files, fmt.Errorf("stat subscription retry lock: %w", err)
	}

	eventsRoot := filepath.Join(dir, "events")
	entries, err := os.ReadDir(eventsRoot)
	if os.IsNotExist(err) {
		return chains, retries, files, nil
	}
	if err != nil {
		return nil, nil, files, fmt.Errorf("read retired events directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, nil, files, fmt.Errorf("retired events directory contains unexpected path %s", filepath.Join(eventsRoot, entry.Name()))
		}
		session, err := url.PathUnescape(entry.Name())
		if err != nil {
			return nil, nil, files, fmt.Errorf("decode retired event directory %q: %w", entry.Name(), err)
		}
		sessionDir := filepath.Join(eventsRoot, entry.Name())
		entries, err := os.ReadDir(sessionDir)
		if err != nil {
			return nil, nil, files, fmt.Errorf("read retired event directory %s: %w", sessionDir, err)
		}
		for _, file := range entries {
			if file.IsDir() {
				return nil, nil, files, fmt.Errorf("retired event directory contains unexpected path %s", filepath.Join(sessionDir, file.Name()))
			}
			path := filepath.Join(sessionDir, file.Name())
			switch file.Name() {
			case "tombstone.json", ".lock", "log.jsonl", ".gen":
				files.paths = append(files.paths, path)
			case "chain_attempts.json":
				data, err := os.ReadFile(path)
				if err != nil {
					return nil, nil, files, fmt.Errorf("read chain attempts from %s: %w", path, err)
				}
				attempts := map[string]string{}
				if err := json.Unmarshal(data, &attempts); err != nil {
					return nil, nil, files, fmt.Errorf("read chain attempts from %s: %w", path, err)
				}
				for key, fingerprint := range attempts {
					parts := strings.Split(key, "\x00")
					if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" || fingerprint == "" {
						return nil, nil, files, fmt.Errorf("read chain attempts from %s: invalid key", path)
					}
					chains = append(chains, sidecarChainAttempt{session: session, instance: parts[0], chainID: parts[1], generation: parts[2], fingerprint: fingerprint})
				}
				files.paths = append(files.paths, path)
			default:
				if strings.HasPrefix(file.Name(), ".cursor.") {
					files.paths = append(files.paths, path)
					continue
				}
				return nil, nil, files, fmt.Errorf("retired event directory contains unexpected path %s", path)
			}
		}
		files.dirs = append(files.dirs, sessionDir)
	}
	files.dirs = append(files.dirs, eventsRoot)
	return chains, retries, files, nil
}

func importSidecarsTx(ctx context.Context, tx *sql.Tx, chains []sidecarChainAttempt, retries []sidecarRetry, files sidecarFiles) error {
	resolved := map[string]string{}
	resolve := func(name string) (string, error) {
		if id, ok := resolved[name]; ok {
			return id, nil
		}
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE name = ? AND status <> 'destroyed'`, name).Scan(&id)
		if errorsIsNoRows(err) {
			err = tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE name = ? AND status = 'destroyed' ORDER BY destroyed_at DESC LIMIT 1`, name).Scan(&id)
		}
		if errorsIsNoRows(err) {
			return "", fmt.Errorf("sidecar import: no live or destroyed session named %q", name)
		}
		if err != nil {
			return "", err
		}
		resolved[name] = id
		return id, nil
	}
	for _, retry := range retries {
		id, err := resolve(retry.session)
		if err != nil {
			return err
		}
		if err := queueSubscriptionRetryTx(ctx, tx, id, retry.action, retry.resource); err != nil {
			return fmt.Errorf("import subscription retry: %w", err)
		}
	}
	for _, attempt := range chains {
		id, err := resolve(attempt.session)
		if err != nil {
			return err
		}
		var generationID string
		err = tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE id = ?`, attempt.generation).Scan(&generationID)
		if err == nil && generationID != id {
			return fmt.Errorf("sidecar import: chain attempt for %q names generation %q from another session", attempt.session, attempt.generation)
		}
		if err != nil && !errorsIsNoRows(err) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chain_attempts (session_id, instance, chain_id, generation, fingerprint) VALUES (?, ?, ?, ?, ?) ON CONFLICT(session_id, instance, chain_id, generation) DO UPDATE SET fingerprint = excluded.fingerprint`, id, attempt.instance, attempt.chainID, attempt.generation, attempt.fingerprint); err != nil {
			return fmt.Errorf("import chain attempt: %w", err)
		}
	}
	return nil
}

func errorsIsNoRows(err error) bool { return err == sql.ErrNoRows }

func sortSidecarFiles(files *sidecarFiles) {
	sort.Strings(files.paths)
	sort.Sort(sort.Reverse(sort.StringSlice(files.dirs)))
}
