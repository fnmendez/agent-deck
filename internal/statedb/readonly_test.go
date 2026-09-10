package statedb

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type readOnlyFileFingerprint struct {
	Mode, Size, ModTime string
	SHA                 [32]byte
}

func fingerprintReadOnlyFixture(t *testing.T, directory string) map[string]readOnlyFileFingerprint {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]readOnlyFileFingerprint, len(entries))
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("unsafe fixture entry %s: %v", entry.Name(), err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = readOnlyFileFingerprint{Mode: info.Mode().String(),
			Size: fmt.Sprint(info.Size()), ModTime: fmt.Sprint(info.ModTime().UnixNano()), SHA: sha256.Sum256(data)}
	}
	return result
}

func TestOpenReadOnlySeesLiveWALWithoutChangingRegistry(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.DB().Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		if _, err := writer.DB().Exec(`INSERT INTO instances (id,title,project_path,created_at) VALUES (?,?,?,1)`,
			fmt.Sprintf("row-%d", index), fmt.Sprintf("Checkpointed row %d", index), "/project"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.DB().Exec(`UPDATE instances SET title='Newest committed WAL value' WHERE id='row-3'`); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("fixture lacks uncheckpointed WAL: %v", err)
	}
	before := fingerprintReadOnlyFixture(t, directory)
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := reader.LoadInstances()
	if err != nil {
		t.Fatal(err)
	}
	newest := ""
	for _, row := range rows {
		if row.ID == "row-3" {
			newest = row.Title
		}
	}
	if len(rows) != 4 || newest != "Newest committed WAL value" {
		t.Fatalf("read-only loader missed newest WAL row: %+v", rows)
	}
	if _, err := reader.DB().Exec(`UPDATE instances SET title='forbidden' WHERE id='wal-newest'`); err == nil {
		t.Fatal("read-only registry accepted a mutator")
	}
	if err := reader.SaveInstances(rows[:1]); err == nil {
		t.Fatal("read-only registry accepted a destructive sweep")
	}
	if _, err := os.Stat(reader.path + ".bak"); err != nil {
		t.Fatalf("sweep did not exercise a private backup attempt: %v", err)
	}
	snapshot := reader.snapshot
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
		t.Fatal("private read-only registry snapshot was not removed")
	}
	after := fingerprintReadOnlyFixture(t, directory)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("read-only open/load/close changed registry files:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestOpenReadOnlyRefusesMissingAndIncompatibleSchema(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing", "state.db")
	if _, err := OpenReadOnly(missing); err == nil {
		t.Fatal("missing registry accepted")
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatal("read-only open created a missing profile directory")
	}

	path := filepath.Join(t.TempDir(), "state.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.DB().Exec(`UPDATE metadata SET value='12' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("incompatible registry schema accepted")
	}
}

func TestReadOnlySnapshotRejectsSourceChangeAfterCopy(t *testing.T) {
	for _, mutation := range []string{"commit", "checkpoint"} {
		t.Run(mutation, func(t *testing.T) {
			temp := t.TempDir()
			t.Setenv("TMPDIR", filepath.Join(temp, "scratch"))
			if err := os.MkdirAll(os.Getenv("TMPDIR"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(temp, "state.db")
			writer, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if err := writer.Migrate(); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.DB().Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.DB().Exec(`INSERT INTO instances (id,title,project_path,created_at) VALUES ('row','before','/project',1)`); err != nil {
				t.Fatal(err)
			}
			mutate := func() error {
				if mutation == "commit" {
					_, err := writer.DB().Exec(`UPDATE instances SET title='after' WHERE id='row'`)
					return err
				}
				_, err := writer.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)")
				return err
			}
			if _, _, err := snapshotReadOnlyRegistryWithHook(path, mutate); err == nil {
				t.Fatalf("%s between copy and final fingerprint was accepted", mutation)
			}
			entries, err := os.ReadDir(os.Getenv("TMPDIR"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("refused snapshot leaked private files: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestOpenReadOnlyRejectsRollbackJournalAndOversize(t *testing.T) {
	for _, fault := range []string{"rollback_journal", "oversize"} {
		t.Run(fault, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			writer, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Migrate(); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "rollback_journal":
				if err := os.WriteFile(path+"-journal", []byte("hot"), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.Truncate(path, maxReadOnlyRegistrySnapshotBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := OpenReadOnly(path); err == nil {
				t.Fatalf("%s registry accepted", fault)
			}
		})
	}
}
