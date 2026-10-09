package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// The journal carries only newly-owned credential filenames, never keys or
// arbitrary paths. The state file remains the transaction commit record.
type authJournal struct {
	Version    int      `json:"version"`
	AuthDir    string   `json:"auth_dir,omitempty"`
	NewAuthIDs []string `json:"new_auth_ids"`
	AuthIDs    []string `json:"auth_ids,omitempty"` // canonical destinations whose derived staging files we own
}

func (p *Pool) journalPath() string { return p.cfg.StatePath + ".journal" }
func canonicalAuthID(id string) bool {
	prefix := ProviderID + "-key-"
	return strings.HasPrefix(id, prefix) && strings.HasSuffix(id, ".json") && validFingerprint(strings.TrimSuffix(strings.TrimPrefix(id, prefix), ".json"))
}
func (p *Pool) writeJournalLocked(ids, stagedIDs []string) error {
	path := p.journalPath()
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return ErrStorage
		}
	} else if !os.IsNotExist(err) {
		return ErrStorage
	}
	data, err := json.Marshal(authJournal{Version: 1, AuthDir: p.cfg.AuthDir, NewAuthIDs: ids, AuthIDs: stagedIDs})
	if err != nil {
		return ErrStorage
	}
	temp, err := stageOwnedFile(path, data)
	if err != nil {
		return ErrStorage
	}
	defer os.Remove(temp)
	if err = os.Rename(temp, path); err != nil {
		return ErrStorage
	}
	if syncDir(filepath.Dir(path)) != nil {
		return ErrStorage
	}
	return nil
}
func (p *Pool) clearJournalLocked() error {
	path := p.journalPath()
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrStorage
	}
	if os.Remove(path) != nil || syncDir(filepath.Dir(path)) != nil {
		return ErrStorage
	}
	return nil
}

// Recovery never scans CPA's auth directory. A durable old state rebuilds its
// own auth records; only canonical journal-listed IDs absent from that state
// can be orphaned new credentials, and only those are removed here.
func (p *Pool) recoverJournalLocked() error {
	data, err := readPrivate(p.journalPath())
	if err != nil && !os.IsNotExist(err) {
		return ErrStorage
	}
	journal := authJournal{Version: 1}
	if err == nil && (json.Unmarshal(data, &journal) != nil || journal.Version != 1 || (journal.AuthDir != "" && journal.AuthDir != p.cfg.AuthDir)) {
		return ErrStorage
	}
	seen := make(map[string]bool, len(journal.NewAuthIDs))
	for _, id := range journal.NewAuthIDs {
		if !canonicalAuthID(id) || seen[id] {
			return ErrStorage
		}
		seen[id] = true
	}
	stagedIDs := make(map[string]bool, len(journal.AuthIDs))
	for _, id := range journal.AuthIDs {
		if !canonicalAuthID(id) || stagedIDs[id] {
			return ErrStorage
		}
		stagedIDs[id] = true
	}
	if len(journal.AuthIDs) > 0 {
		for id := range seen {
			if !stagedIDs[id] {
				return ErrStorage
			}
		}
	}
	owned := make(map[string]bool, len(p.accounts))
	for _, a := range p.accounts {
		owned[a.AuthID] = true
		stagedIDs[a.AuthID] = true
	}
	for id := range seen {
		stagedIDs[id] = true
	} // legacy journal lacked AuthIDs
	stagePaths := []string{ownedStagePath(p.cfg.StatePath), ownedStagePath(p.journalPath())}
	for id := range stagedIDs {
		destination := filepath.Join(p.cfg.AuthDir, id)
		if !regularOrMissing(destination) {
			return ErrStorage
		}
		stagePaths = append(stagePaths, ownedStagePath(destination))
	}
	// Validate the whole recovery set before touching any file. Every staging
	// path is derived from canonical IDs or fixed configuration, not user paths.
	for _, path := range stagePaths {
		if !regularOrMissing(path) {
			return ErrStorage
		}
	}
	for _, id := range journal.NewAuthIDs {
		if owned[id] {
			continue
		}
		if e := os.Remove(filepath.Join(p.cfg.AuthDir, id)); e != nil && !os.IsNotExist(e) {
			return ErrStorage
		}
	}
	for _, path := range stagePaths {
		if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
			return ErrStorage
		}
	}
	if syncDir(p.cfg.AuthDir) != nil || syncDir(filepath.Dir(p.cfg.StatePath)) != nil {
		return ErrStorage
	}
	// Keep the journal until persistLocked has replayed the state's auth set.
	return nil
}

func regularOrMissing(path string) bool {
	info, err := os.Lstat(path)
	return os.IsNotExist(err) || (err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0)
}
