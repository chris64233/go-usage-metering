package usagemetering

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoragePersistsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	st := NewFileStorage(path)
	ctx := context.Background()

	_, err := st.Update(ctx, func(s State) (State, any, error) {
		s.Events = append(s.Events, Event{
			Type: KindEvent, ExternalID: "e1", TenantID: "t1", MeterID: "m1",
			OccurredAt: time.Unix(0, 0).UTC(), Delta: MustParseAmount("1"),
			PeriodID:    "2026-09",
			SubmittedAt: time.Unix(0, 0).UTC(),
		})
		return s, "ok", nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// 目录此前不存在，应被创建并写入文件。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not created: %v", err)
	}

	// 不应残留临时文件。
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".state-*.tmp")); len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}

	// 重新打开能读到数据。
	st2 := NewFileStorage(path)
	got, err := st2.View(ctx, func(s State) (any, error) { return len(s.Events), nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.(int) != 1 {
		t.Errorf("events after reopen = %v", got)
	}
}

func TestFileStorageViewDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := NewFileStorage(path)
	ctx := context.Background()

	_, _ = st.Update(ctx, func(s State) (State, any, error) {
		s.Events = append(s.Events, Event{Type: KindEvent, ExternalID: "e1", PeriodID: "p"})
		return s, nil, nil
	})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mtime := info.ModTime()
	size := info.Size()

	time.Sleep(10 * time.Millisecond)
	for i := 0; i < 5; i++ {
		_, err := st.View(ctx, func(s State) (any, error) {
			_ = len(s.Events)
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	info2, _ := os.Stat(path)
	if info2.ModTime() != mtime || info2.Size() != size {
		t.Errorf("View modified the state file: %+v -> %+v", info, info2)
	}
}

func TestFileStorageUpdateRollsBackOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := NewFileStorage(path)
	ctx := context.Background()

	boom := newError(KindValidation, "boom")
	_, err := st.Update(ctx, func(s State) (State, any, error) {
		s.Events = append(s.Events, Event{Type: KindEvent, ExternalID: "e1"})
		return s, nil, boom
	})
	if err != boom {
		t.Fatalf("want boom, got %v", err)
	}

	// 失败事务不应产生文件。
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state file should not exist after failed tx, stat err=%v", err)
	}
}

func TestFileStorageRejectsCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := NewFileStorage(path)
	_, err := st.View(context.Background(), func(s State) (any, error) { return nil, nil })
	if !isKind(err, KindStorage) {
		t.Fatalf("want storage error, got %v", err)
	}
}

func TestFileStorageValidatesBeforePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := NewFileStorage(path)
	ctx := context.Background()

	// 修正引用了不存在的原事件，validateState 必须拒绝写盘。
	_, err := st.Update(ctx, func(s State) (State, any, error) {
		s.Events = append(s.Events, Event{
			Type: KindCorrection, CorrectionID: "c1", OriginEventID: "missing",
		})
		return s, nil, nil
	})
	if !isKind(err, KindStorage) {
		t.Fatalf("want storage validation error, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("invalid state must not be persisted")
	}
}
