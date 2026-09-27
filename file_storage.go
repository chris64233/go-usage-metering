package usagemetering

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// FileStorage 把 State 以 JSON 文件持久化到磁盘。
// 通过进程内互斥锁串行化所有事务，写盘采用“临时文件 + 原子重命名 +
// fsync”，保证崩溃后状态要么是旧版本要么是新版本，不会出现半写文件。
//
// FileStorage 适用于单实例部署；多实例共享存储时请改用基于数据库
// 事务（SELECT ... FOR UPDATE / SERIALIZABLE）的 Storage 实现。
type FileStorage struct {
	path string
	mu   sync.RWMutex
}

// NewFileStorage 创建文件存储。文件与所在目录会在首次写入时自动创建。
func NewFileStorage(path string) *FileStorage {
	return &FileStorage{path: path}
}

// View 实现 Storage。只读，不触发写盘。
func (f *FileStorage) View(ctx context.Context, fn func(State) (any, error)) (any, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := f.loadLocked()
	if err != nil {
		return nil, err
	}
	return fn(state)
}

// Update 实现 Storage。
func (f *FileStorage) Update(ctx context.Context, fn func(State) (State, any, error)) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	state, err := f.loadLocked()
	if err != nil {
		return nil, err
	}

	newState, result, err := fn(state)
	if err != nil {
		return result, err
	}
	if err := validateState(newState); err != nil {
		return nil, err
	}
	if err := f.persistLocked(newState); err != nil {
		return nil, err
	}
	return result, nil
}

func (f *FileStorage) loadLocked() (State, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newState(), nil
		}
		return State{}, newError(KindStorage, "read state file: %v", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, newError(KindStorage, "decode state file: %v", err)
	}
	if s.Snapshots == nil {
		s.Snapshots = map[string]Snapshot{}
	}
	return s, nil
}

func (f *FileStorage) persistLocked(s State) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return newError(KindStorage, "create state directory: %v", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return newError(KindStorage, "encode state: %v", err)
	}

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return newError(KindStorage, "create temp file: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return newError(KindStorage, "write temp file: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return newError(KindStorage, "fsync temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return newError(KindStorage, "close temp file: %v", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return newError(KindStorage, "rename temp file: %v", err)
	}
	// 同步目录，保证重命名本身落盘。
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// validateState 在写盘前做基本完整性检查，防止程序缺陷污染持久化数据。
func validateState(s State) error {
	seenEvents := map[string]bool{}
	seenCorr := map[string]bool{}
	for _, e := range s.Events {
		switch e.Type {
		case KindEvent:
			if seenEvents[e.ExternalID] {
				return newError(KindStorage, "duplicate external event id %q in state", e.ExternalID)
			}
			seenEvents[e.ExternalID] = true
		case KindCorrection:
			if seenCorr[e.CorrectionID] {
				return newError(KindStorage, "duplicate correction id %q in state", e.CorrectionID)
			}
			seenCorr[e.CorrectionID] = true
			if !seenEvents[e.OriginEventID] {
				return newError(KindStorage, "correction %q references missing origin event %q", e.CorrectionID, e.OriginEventID)
			}
		default:
			return newError(KindStorage, "unknown event type %q", e.Type)
		}
	}
	for id, snap := range s.Snapshots {
		if snap.PeriodID != id {
			return newError(KindStorage, "snapshot map key %q != period id %q", id, snap.PeriodID)
		}
	}
	return nil
}
