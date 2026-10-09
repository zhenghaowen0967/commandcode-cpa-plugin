package pool

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type storageLock struct{ file *os.File }
type diskState struct {
	Version  int        `json:"version"`
	Accounts []*account `json:"accounts"`
}

func New(cfg Config) (*Pool, error) {
	if strings.TrimSpace(cfg.AuthDir) == "" || strings.TrimSpace(cfg.StatePath) == "" {
		return nil, ErrInvalid
	}
	if cfg.QuotaMaxAge == 0 {
		cfg.QuotaMaxAge = 5 * time.Minute
	}
	if cfg.QuotaMaxAge < 0 {
		return nil, ErrInvalid
	}
	if cfg.DefaultLimit == 0 {
		cfg.DefaultLimit = 1
	}
	if cfg.DefaultLimit < 1 || cfg.DefaultLimit > 1000 {
		return nil, ErrInvalid
	}
	if cfg.EventLimit <= 0 {
		cfg.EventLimit = 500
	}
	if cfg.EventLimit > 500 {
		cfg.EventLimit = 500
	}
	var err error
	cfg.AuthDir, err = filepath.Abs(cfg.AuthDir)
	if err != nil {
		return nil, ErrStorage
	}
	cfg.StatePath, err = filepath.Abs(cfg.StatePath)
	if err != nil {
		return nil, ErrStorage
	}
	if cfg.AuthDir == string(filepath.Separator) || filepath.Dir(cfg.StatePath) == string(filepath.Separator) || cfg.StatePath == filepath.Join(cfg.AuthDir, ".commandcode-pool.lock") {
		return nil, ErrInvalid
	}
	if strings.HasPrefix(filepath.Base(cfg.StatePath), ".commandcode-pool-stage-") {
		return nil, ErrInvalid
	}
	if filepath.Dir(cfg.StatePath) == cfg.AuthDir && strings.HasPrefix(filepath.Base(cfg.StatePath), ProviderID+"-key-") {
		return nil, ErrInvalid // state must not overwrite a CPA credential identity
	}
	if err = authDirectory(cfg.AuthDir); err != nil {
		return nil, ErrStorage
	}
	if filepath.Dir(cfg.StatePath) != cfg.AuthDir {
		if err = privateDirectory(filepath.Dir(cfg.StatePath)); err != nil {
			return nil, ErrStorage
		}
	}
	p := &Pool{cfg: cfg, accounts: make(map[string]*account), attempts: make(map[string]*Lease), inflight: make(map[string]int), terminal: make(map[string]struct{}), secrets: make(map[string]struct{}), rename: os.Rename}
	// Lock both the auth namespace and state path: two configurations sharing
	// either must not create independent concurrency counters.
	paths := []string{filepath.Join(cfg.AuthDir, ".commandcode-pool.lock"), cfg.StatePath + ".lock", cfg.StatePath + ".journal.lock"}
	sort.Strings(paths)
	for i, path := range paths {
		if i > 0 && path == paths[i-1] {
			continue
		}
		lock, e := openLock(path)
		if e != nil {
			p.releaseLocksLocked()
			return nil, ErrStorage
		}
		p.locks = append(p.locks, lock)
	}
	defer func() {
		if err != nil {
			p.releaseLocksLocked()
		}
	}()
	data, e := readPrivate(cfg.StatePath)
	if e != nil && !os.IsNotExist(e) {
		err = ErrStorage
		return nil, err
	}
	if e == nil {
		var state diskState
		if json.Unmarshal(data, &state) != nil || state.Version != 1 {
			err = ErrStorage
			return nil, err
		}
		hashes := make(map[string]bool)
		caps := make(map[string]int)
		for _, a := range state.Accounts {
			if a == nil || a.ID == "" || a.Name == "" || a.GroupID == "" || a.Limit < 1 || a.Limit > 1000 || !validFingerprint(a.Fingerprint) || a.AuthID != authID(a.Fingerprint) || a.ID != a.Fingerprint || p.accounts[a.ID] != nil || hashes[a.Fingerprint] {
				err = ErrStorage
				return nil, err
			}
			if a.Deleted {
				if a.APIKey != "" || a.Enabled {
					err = ErrStorage
					return nil, err
				}
			} else if a.APIKey == "" || keyHash(a.APIKey) != a.Fingerprint {
				err = ErrStorage
				return nil, err
			}
			if !a.Deleted {
				if cap, exists := caps[a.GroupID]; exists && cap != a.Limit {
					err = ErrStorage
					return nil, err
				}
				caps[a.GroupID] = a.Limit
			}
			if a.KeyLength < 0 || a.KeyLength > 4096 || len(a.Name) > 256 || len(a.GroupID) > 256 || len(a.Identity) > 256 {
				err = ErrStorage
				return nil, err
			}
			if !a.Deleted {
				if a.KeyLength != 0 && a.KeyLength != len(a.APIKey) {
					err = ErrStorage
					return nil, err
				}
				a.KeyLength = len(a.APIKey)
				if a.KeyLength > 4096 {
					err = ErrStorage
					return nil, err
				}
			}
			p.accounts[a.ID] = a
			hashes[a.Fingerprint] = true
			if a.APIKey != "" {
				p.secrets[a.APIKey] = struct{}{}
			}
		}
	}
	for _, a := range p.accounts {
		if p.containsSecretLocked(a.Name) || p.containsSecretLocked(a.GroupID) || p.containsSecretLocked(a.Identity) || (a.CatalogRevision != "" && (!validFingerprint(a.CatalogRevision) || p.containsSecretLocked(a.CatalogRevision))) {
			err = ErrStorage
			return nil, err // never rename real group identities to repair secret metadata
		}
	}
	if e = p.recoverJournalLocked(); e != nil {
		err = e
		return nil, err
	}
	// State is the commit record. Reconcile only identities owned by that state;
	// never scan, import, or delete unrelated CPA auth files.
	if e = p.persistLocked(p.accounts); e != nil {
		err = e
		return nil, err
	}
	return p, nil
}
func validFingerprint(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func attemptID(seq uint64) string { return fmt.Sprintf("attempt-%d", seq) }
func authDirectory(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	created := false
	if err := os.Mkdir(path, 0700); err != nil {
		if !os.IsExist(err) {
			return err
		}
	} else {
		created = true
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrStorage
	}
	if created {
		return os.Chmod(path, 0700)
	}
	return nil
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrStorage
	}
	return os.Chmod(path, 0700)
}
func openLock(path string) (*storageLock, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, ErrStorage
	}
	file := os.NewFile(uintptr(fd), path)
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return nil, ErrStorage
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, ErrStorage
	}
	return &storageLock{file: file}, nil
}
func (p *Pool) releaseLocksLocked() {
	for _, lock := range p.locks {
		if lock != nil && lock.file != nil {
			_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
			_ = lock.file.Close()
		}
	}
	p.locks = nil
}
func readPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrStorage
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

type fileChange struct {
	path    string
	data    []byte
	remove  bool
	old     []byte
	existed bool
	temp    string
}

// All staged payloads are fsynced before any rename. State is renamed last.
// On an ordinary I/O failure both auth files and state are restored before
// returning, and in-memory accounts never advance. If rollback itself fails,
// admission is quarantined for the lifetime of this Pool (fail closed).
func (p *Pool) persistLocked(accounts map[string]*account) error {
	if p.storageBroken {
		return ErrStorage
	}
	state := diskState{Version: 1, Accounts: make([]*account, 0, len(accounts))}
	ids := make([]string, 0, len(accounts))
	for id := range accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	changes := make([]fileChange, 0, len(ids)+1)
	for _, id := range ids {
		a := accounts[id]
		state.Accounts = append(state.Accounts, a)
		change := fileChange{path: filepath.Join(p.cfg.AuthDir, a.AuthID), remove: a.Deleted}
		if !a.Deleted {
			change.data, _ = json.Marshal(struct {
				Type            string `json:"type"`
				ID              string `json:"id"`
				Label           string `json:"label"`
				APIKey          string `json:"api_key"`
				GroupID         string `json:"group_id"`
				Disabled        bool   `json:"disabled"`
				CatalogRevision string `json:"catalog_revision,omitempty"`
			}{ProviderID, a.AuthID, a.Name, a.APIKey, a.GroupID, !a.Enabled, a.CatalogRevision})
		}
		changes = append(changes, change)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return ErrStorage
	}
	changes = append(changes, fileChange{path: p.cfg.StatePath, data: encoded})
	// Preflight all destinations before publishing a journal or staging secrets.
	for i := range changes {
		change := &changes[i]
		old, e := readPrivate(change.path)
		if e != nil && !os.IsNotExist(e) {
			return ErrStorage
		}
		change.old, change.existed = old, e == nil
	}
	newAuthIDs, stagedAuthIDs := make([]string, 0), make([]string, 0, len(ids))
	for i, id := range ids {
		stagedAuthIDs = append(stagedAuthIDs, accounts[id].AuthID)
		if p.accounts[id] == nil && !accounts[id].Deleted && !changes[i].existed {
			newAuthIDs = append(newAuthIDs, accounts[id].AuthID)
		}
	}
	// A durable non-secret journal must precede every secret staging write.
	if p.writeJournalLocked(newAuthIDs, stagedAuthIDs) != nil {
		return ErrStorage
	}
	defer cleanupTemps(changes)
	for i := range changes {
		change := &changes[i]
		if !change.remove {
			change.temp, err = stageOwnedFile(change.path, change.data)
			if err != nil {
				if change.temp == "" || !cleanupTemps(changes) || p.clearJournalLocked() != nil {
					p.storageBroken = true
				}
				return ErrStorage
			}
		}
	}
	for i := range changes {
		change := &changes[i]
		if change.remove {
			err = os.Remove(change.path)
			if os.IsNotExist(err) {
				err = nil
			}
		} else {
			err = p.rename(change.temp, change.path)
		}
		if err == nil {
			err = syncDir(filepath.Dir(change.path))
		}
		if err != nil {
			if !cleanupTemps(changes) || !rollback(changes[:i+1]) {
				p.storageBroken = true // leave journal for next process recovery
			} else if p.clearJournalLocked() != nil {
				p.storageBroken = true
			}
			return ErrStorage
		}
	}
	if p.clearJournalLocked() != nil {
		// Removal may have succeeded before its directory fsync failed. Recreate
		// a durable recovery record before rolling back the committed state.
		if p.writeJournalLocked(newAuthIDs, stagedAuthIDs) != nil {
			p.storageBroken = true
			return ErrStorage
		}
		if !cleanupTemps(changes) || !rollback(changes) || p.clearJournalLocked() != nil {
			p.storageBroken = true
		}
		return ErrStorage
	}
	return nil
}
func ownedStagePath(path string) string {
	return filepath.Join(filepath.Dir(path), ".commandcode-pool-stage-"+keyHash(path)+".tmp")
}
func stageOwnedFile(path string, data []byte) (string, error) {
	name := ownedStagePath(path)
	fd, err := syscall.Open(name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(name)
		return name, err
	}
	return name, nil
}
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func cleanupTemps(changes []fileChange) bool {
	ok := true
	for _, change := range changes {
		if change.temp != "" {
			if err := os.Remove(change.temp); err != nil && !os.IsNotExist(err) {
				ok = false
			}
		}
	}
	return ok
}
func rollback(changes []fileChange) bool {
	ok := true
	for i := len(changes) - 1; i >= 0; i-- {
		change := changes[i]
		if change.existed {
			temp, err := stageOwnedFile(change.path, change.old)
			if err == nil {
				err = os.Rename(temp, change.path)
				_ = os.Remove(temp)
			}
			if err != nil {
				ok = false
			}
		} else {
			if err := os.Remove(change.path); err != nil && !os.IsNotExist(err) {
				ok = false
			}
		}
		if syncDir(filepath.Dir(change.path)) != nil {
			ok = false
		}
	}
	return ok
}
