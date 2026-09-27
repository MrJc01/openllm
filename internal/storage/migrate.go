package storage

import (
	"database/sql"
	"fmt"
	"strings"
)

// migrations versionadas por PRAGMA user_version: a migração i leva o banco
// da versão i para i+1. Só acrescentar ao fim; cada uma roda numa transação
// e deve ser idempotente (bancos de builds intermediários já podem ter parte).
var migrations = []func(tx *sql.Tx) error{
	// 1: instance_models — conjunto de modelos por instância (principal,
	// extras e alvos de add/swap) com o último estado conhecido.
	func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS instance_models (
			instance_id TEXT NOT NULL,
			model TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'extra',
			state TEXT NOT NULL DEFAULT '',
			updated_at DATETIME,
			PRIMARY KEY (instance_id, model)
		)`)
		return err
	},
	// 2: detail (motivo de falha) e version (guarda de ordem das escritas).
	func(tx *sql.Tx) error {
		if err := addColumn(tx, `ALTER TABLE instance_models ADD COLUMN detail TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		return addColumn(tx, `ALTER TABLE instance_models ADD COLUMN version INTEGER NOT NULL DEFAULT 0`)
	},
}

func addColumn(tx *sql.Tx, stmt string) error {
	if _, err := tx.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	return nil
}

// SchemaVersion devolve o PRAGMA user_version atual.
func (s *DB) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

func (s *DB) migrate() error {
	v, err := s.SchemaVersion()
	if err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		err := s.withTx(func(tx *sql.Tx) error {
			if err := migrations[i](tx); err != nil {
				return err
			}
			_, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}
