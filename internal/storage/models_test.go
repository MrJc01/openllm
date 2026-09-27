package storage

import (
	"testing"
	"time"
)

// Banco legado (sem instance_models) ganha a tabela na abertura e aceita o
// conjunto de modelos da instância antiga.
func TestInstanceModelsMigrationOnLegacyDB(t *testing.T) {
	db, _ := setupDB(t, true)
	if err := db.PutInstanceModel("old-1", "llama3.2:3b", RolePrimary, "ready"); err != nil {
		t.Fatalf("instance_models ausente após migração: %v", err)
	}
	rows, err := db.ListInstanceModels("old-1")
	if err != nil || len(rows) != 1 || rows[0].State != "ready" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	// Reabrir (migração idempotente) mantém os dados.
	db.Close()
	db2, err := OpenDB()
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if rows, _ := db2.ListInstanceModels("old-1"); len(rows) != 1 {
		t.Fatalf("dados perdidos ao reabrir: %+v", rows)
	}
}

func TestInstanceModelsRoundTrip(t *testing.T) {
	type op func(db *DB) error
	tests := []struct {
		name string
		ops  []op
		want []InstanceModel // Model/Role/State na ordem esperada
	}{
		{
			name: "principal primeiro, extras na ordem de inclusão",
			ops: []op{
				func(db *DB) error { return db.PutInstanceModel("i", "b", RoleExtra, "ready") },
				func(db *DB) error { return db.PutInstanceModel("i", "a", RoleExtra, "ready") },
				func(db *DB) error { return db.PutInstanceModel("i", "p", RolePrimary, "loading") },
			},
			want: []InstanceModel{{Model: "p", Role: RolePrimary, State: "loading"}, {Model: "b", Role: RoleExtra, State: "ready"}, {Model: "a", Role: RoleExtra, State: "ready"}},
		},
		{
			name: "SetInstanceModelState preserva papel e ordem",
			ops: []op{
				func(db *DB) error { return db.PutInstanceModel("i", "x", RoleAdding, "loading") },
				func(db *DB) error { return db.PutInstanceModel("i", "y", RoleExtra, "ready") },
				func(db *DB) error { return db.SetInstanceModelState("i", "x", RolePrimary, "failed") },
			},
			want: []InstanceModel{{Model: "x", Role: RoleAdding, State: "failed"}, {Model: "y", Role: RoleExtra, State: "ready"}},
		},
		{
			name: "SetInstanceModelState cria com papel default; state vazio não apaga",
			ops: []op{
				func(db *DB) error { return db.SetInstanceModelState("i", "n", RoleAdding, "loading") },
				func(db *DB) error { return db.PutInstanceModel("i", "n", RoleExtra, "") },
			},
			want: []InstanceModel{{Model: "n", Role: RoleExtra, State: "loading"}},
		},
		{
			name: "promote e delete",
			ops: []op{
				func(db *DB) error { return db.PutInstanceModel("i", "x", RoleAdding, "loading") },
				func(db *DB) error { return db.PromoteExtraModel("i", "x") },
				func(db *DB) error { return db.PutInstanceModel("i", "z", RoleExtra, "ready") },
				func(db *DB) error { return db.DeleteInstanceModel("i", "z") },
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
				if got[i].Model != tc.want[i].Model || got[i].Role != tc.want[i].Role || got[i].State != tc.want[i].State {
					t.Fatalf("[%d] got %+v, want %+v", i, got[i], tc.want[i])
				}
				if got[i].UpdatedAt.IsZero() {
					t.Fatalf("[%d] updated_at vazio", i)
				}
			}
		})
	}
}

func TestCommitSwapAndDeleteInstanceCascade(t *testing.T) {
	db, _ := setupDB(t, false)
	inst := &Instance{ID: "s1", Provider: "mock", Status: "running", Model: "old", Engine: "ollama", GroupID: "ollama/old", CreatedAt: time.Now()}
	db.SaveInstance(inst)
	db.PutInstanceModel("s1", "old", RolePrimary, "ready")
	db.PutInstanceModel("s1", "keep", RoleExtra, "ready")
	db.PutInstanceModel("s1", "new", RoleSwapping, "loading")
	db.PutInstanceModel("other", "m", RolePrimary, "ready")

	inst.Model, inst.GroupID = "new", "ollama/new"
	if err := db.CommitSwap(inst, "old"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetInstance("s1")
	if got.Model != "new" || got.GroupID != "ollama/new" {
		t.Fatalf("instância não atualizada: %+v", got)
	}
	rows, _ := db.ListInstanceModels("s1")
	if len(rows) != 2 || rows[0].Model != "new" || rows[0].Role != RolePrimary || rows[0].State != "ready" || rows[1].Model != "keep" {
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
