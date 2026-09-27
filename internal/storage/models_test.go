package storage

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// Banco legado (sem instance_models) migra até a última versão e aceita o
// conjunto de modelos da instância antiga; reabrir é idempotente.
func TestInstanceModelsMigrationOnLegacyDB(t *testing.T) {
	db, _ := setupDB(t, true)
	if v, _ := db.SchemaVersion(); v != len(migrations) {
		t.Fatalf("user_version = %d, quer %d", v, len(migrations))
	}
	if err := db.PutInstanceModel("old-1", "llama3.2:3b", RolePrimary, "ready", "", 1); err != nil {
		t.Fatalf("instance_models ausente após migração: %v", err)
	}
	db.Close()
	db2, err := OpenDB()
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if rows, _ := db2.ListInstanceModels("old-1"); len(rows) != 1 || rows[0].State != "ready" {
		t.Fatalf("dados perdidos ao reabrir: %+v", rows)
	}
}

// Banco de um build intermediário: tabela v1 sem detail/version e
// user_version 0 — a migração 2 acrescenta as colunas sem perder linhas.
func TestInstanceModelsMigrationFromIntermediateSchema(t *testing.T) {
	db, _ := setupDB(t, false)
	db.Close()
	conn, err := sql.Open("sqlite3", ".openllm/openllm.db")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE instance_models`,
		`CREATE TABLE instance_models (instance_id TEXT NOT NULL, model TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'extra', state TEXT NOT NULL DEFAULT '', updated_at DATETIME, PRIMARY KEY (instance_id, model))`,
		`INSERT INTO instance_models (instance_id, model, role, state) VALUES ('i', 'm', 'extra', 'ready')`,
		`PRAGMA user_version = 0`,
	} {
		if _, err := conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	db2, err := OpenDB()
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	rows, err := db2.ListInstanceModels("i")
	if err != nil || len(rows) != 1 || rows[0].Detail != "" || rows[0].Version != 0 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestInstanceModelsRoundTrip(t *testing.T) {
	type op func(db *DB) error
	tests := []struct {
		name string
		ops  []op
		want []InstanceModel
	}{
		{
			name: "principal primeiro, extras na ordem de inclusão",
			ops: []op{
				func(db *DB) error { return db.PutInstanceModel("i", "b", RoleExtra, "ready", "", 1) },
				func(db *DB) error { return db.PutInstanceModel("i", "a", RoleExtra, "ready", "", 1) },
				func(db *DB) error { return db.PutInstanceModel("i", "p", RolePrimary, "loading", "", 1) },
			},
			want: []InstanceModel{{Model: "p", Role: RolePrimary, State: "loading"}, {Model: "b", Role: RoleExtra, State: "ready"}, {Model: "a", Role: RoleExtra, State: "ready"}},
		},
		{
			name: "update grava estado e motivo, preserva papel",
			ops: []op{
				func(db *DB) error { return db.PutInstanceModel("i", "x", RoleAdding, "loading", "", 1) },
				func(db *DB) error {
					_, err := db.UpdateInstanceModelState("i", "x", "failed", "timeout", 2)
					return err
				},
			},
			want: []InstanceModel{{Model: "x", Role: RoleAdding, State: "failed", Detail: "timeout"}},
		},
		{
			name: "escrita obsoleta (version menor) não sobrescreve",
			ops: []op{
				func(db *DB) error { return db.PutInstanceModel("i", "x", RoleExtra, "ready", "", 5) },
				func(db *DB) error {
					ok, err := db.UpdateInstanceModelState("i", "x", "loading", "", 4)
					if ok {
						return errors.New("escrita obsoleta aplicada")
					}
					return err
				},
			},
			want: []InstanceModel{{Model: "x", Role: RoleExtra, State: "ready"}},
		},
		{
			name: "update não cria linha; ensure não sobrescreve",
			ops: []op{
				func(db *DB) error {
					ok, err := db.UpdateInstanceModelState("i", "ghost", "ready", "", 9)
					if ok {
						return errors.New("update criou linha")
					}
					return err
				},
				func(db *DB) error { return db.PutInstanceModel("i", "p", RolePrimary, "ready", "", 1) },
				func(db *DB) error { return db.EnsureInstanceModel("i", "p", RolePrimary, "loading", 2) },
			},
			want: []InstanceModel{{Model: "p", Role: RolePrimary, State: "ready"}},
		},
		{
			name: "promote e delete; promote de removido é ErrModelGone",
			ops: []op{
				func(db *DB) error { return db.PutInstanceModel("i", "x", RoleAdding, "loading", "", 1) },
				func(db *DB) error { return db.PromoteExtraModel("i", "x", 2) },
				func(db *DB) error { return db.PutInstanceModel("i", "z", RoleAdding, "loading", "", 1) },
				func(db *DB) error { return db.DeleteInstanceModel("i", "z") },
				func(db *DB) error {
					if err := db.PromoteExtraModel("i", "z", 3); !errors.Is(err, ErrModelGone) {
						return errors.New("promote de removido deveria falhar")
					}
					return nil
				},
			},
			want: []InstanceModel{{Model: "x", Role: RoleExtra, State: "ready"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := setupDB(t, false)
			for i, o := range tc.ops {
				if err := o(db); err != nil {
					t.Fatalf("op %d: %v", i, err)
				}
			}
			got, err := db.ListInstanceModels("i")
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				g, w := got[i], tc.want[i]
				if g.Model != w.Model || g.Role != w.Role || g.State != w.State || g.Detail != w.Detail {
					t.Fatalf("[%d] got %+v, want %+v", i, g, w)
				}
			}
		})
	}
}

func TestCommitSwapAndDeleteInstanceCascade(t *testing.T) {
	db, _ := setupDB(t, false)
	inst := &Instance{ID: "s1", Provider: "mock", Status: "running", Model: "old", Engine: "ollama", GroupID: "ollama/old", CreatedAt: time.Now()}
	db.SaveInstance(inst)
	db.PutInstanceModel("s1", "old", RolePrimary, "ready", "", 1)
	db.PutInstanceModel("s1", "keep", RoleExtra, "ready", "", 1)
	db.PutInstanceModel("s1", "new", RoleSwapping, "loading", "", 1)
	db.PutInstanceModel("other", "m", RolePrimary, "ready", "", 1)

	if err := db.CommitSwap("s1", "old", "gone", "ollama/gone", 2); !errors.Is(err, ErrModelGone) {
		t.Fatalf("swap sem alvo = %v", err)
	}
	if got, _ := db.GetInstance("s1"); got.Model != "old" {
		t.Fatal("swap abortado deveria dar rollback")
	}
	if err := db.CommitSwap("s1", "old", "new", "ollama/new", 3); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetInstance("s1")
	if got.Model != "new" || got.GroupID != "ollama/new" {
		t.Fatalf("instância não atualizada: %+v", got)
	}
	rows, _ := db.ListInstanceModels("s1")
	if len(rows) != 2 || rows[0].Model != "new" || rows[0].Role != RolePrimary || rows[1].Model != "keep" {
		t.Fatalf("rows após swap: %+v", rows)
	}
	if err := db.DeleteInstance("s1"); err != nil {
		t.Fatal(err)
	}
	all, _ := db.ListAllInstanceModels()
	if len(all["s1"]) != 0 || len(all["other"]) != 1 {
		t.Fatalf("delete deveria limpar só s1: %+v", all)
	}
}

func TestSweepInstanceModels(t *testing.T) {
	db, _ := setupDB(t, false)
	db.SaveInstance(&Instance{ID: "live", Provider: "mock", Status: "running", Model: "p", CreatedAt: time.Now()})
	db.PutInstanceModel("live", "p", RolePrimary, "failed", "x", 1)    // principal: fica
	db.PutInstanceModel("live", "old", RoleAdding, "failed", "x", 1)   // falha velha: sai
	db.PutInstanceModel("live", "fresh", RoleAdding, "failed", "x", 1) // falha recente: fica
	db.PutInstanceModel("live", "ok", RoleExtra, "ready", "", 1)       // fica
	db.PutInstanceModel("orphan", "m", RolePrimary, "ready", "", 1)    // órfã: sai
	if _, err := db.db.Exec(`UPDATE instance_models SET updated_at = ? WHERE model IN ('old', 'p')`, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, err := db.SweepInstanceModels(24 * time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("sweep n=%d err=%v", n, err)
	}
	all, _ := db.ListAllInstanceModels()
	if len(all["live"]) != 3 || len(all["orphan"]) != 0 {
		t.Fatalf("após sweep: %+v", all)
	}
}
