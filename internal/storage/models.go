package storage

import (
	"database/sql"
	"errors"
	"time"
)

// Papéis de um modelo numa instância.
const (
	RolePrimary  = "primary"  // modelo principal (instances.model)
	RoleExtra    = "extra"    // adicional roteado pelo proxy quando pronto
	RoleAdding   = "adding"   // alvo de /models/add ainda baixando
	RoleSwapping = "swapping" // alvo de /swap ainda baixando
)

// ErrModelGone: a linha do modelo (ou a instância) foi removida no meio do
// fluxo — quem chamou deve abortar sem ressuscitar nada.
var ErrModelGone = errors.New("instance model no longer exists")

// InstanceModel é um modelo de uma instância com seu último estado conhecido
// ("loading", "ready", "failed", "unknown" ou "" quando nunca verificado).
type InstanceModel struct {
	InstanceID string    `json:"instance_id"`
	Model      string    `json:"model"`
	Role       string    `json:"role"`
	State      string    `json:"state"`
	Detail     string    `json:"detail,omitempty"` // motivo da falha
	Version    int64     `json:"version"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type execer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// PutInstanceModel cria ou redefine (papel, estado, detalhe) um modelo. Usado
// só em ações explícitas (add/swap/backfill do principal).
func (s *DB) PutInstanceModel(instID, model, role, state, detail string, version int64) error {
	_, err := s.db.Exec(`INSERT INTO instance_models (instance_id, model, role, state, detail, version, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(instance_id, model) DO UPDATE SET role = excluded.role, state = excluded.state,
			detail = excluded.detail, version = excluded.version, updated_at = excluded.updated_at`,
		instID, model, role, state, detail, version, time.Now())
	return err
}

// UpdateInstanceModelState só atualiza linhas existentes e só se version for
// mais nova que a gravada (escrita atrasada não sobrescreve uma recente).
// Devolve false se a linha não existe ou a escrita estava obsoleta.
func (s *DB) UpdateInstanceModelState(instID, model, state, detail string, version int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE instance_models SET state = ?, detail = ?, version = ?, updated_at = ?
		WHERE instance_id = ? AND model = ? AND version < ?`,
		state, detail, version, time.Now(), instID, model, version)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PromoteExtraModel marca um alvo de add como extra pronto. ErrModelGone se
// o modelo foi removido enquanto baixava.
func (s *DB) PromoteExtraModel(instID, model string, version int64) error {
	res, err := s.db.Exec(`UPDATE instance_models SET role = 'extra', state = 'ready', detail = '', version = ?, updated_at = ?
		WHERE instance_id = ? AND model = ? AND role IN ('adding', 'extra')`,
		version, time.Now(), instID, model)
	return requireRow(res, err)
}

func requireRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrModelGone
	}
	return nil
}

// ListInstanceModels devolve os modelos da instância, principal primeiro e
// depois na ordem de inclusão.
func (s *DB) ListInstanceModels(instID string) ([]InstanceModel, error) {
	return s.queryModels(`WHERE instance_id = ?`, instID)
}

// ListAllInstanceModels agrupa por instância (usado pelo /status numa query só).
func (s *DB) ListAllInstanceModels() (map[string][]InstanceModel, error) {
	list, err := s.queryModels(``)
	if err != nil {
		return nil, err
	}
	out := map[string][]InstanceModel{}
	for _, im := range list {
		out[im.InstanceID] = append(out[im.InstanceID], im)
	}
	return out, nil
}

func (s *DB) queryModels(where string, args ...interface{}) ([]InstanceModel, error) {
	rows, err := s.db.Query(`SELECT instance_id, model, role, state, detail, version, updated_at FROM instance_models `+where+
		` ORDER BY instance_id, CASE role WHEN 'primary' THEN 0 ELSE 1 END, rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstanceModel
	for rows.Next() {
		var im InstanceModel
		var upd sql.NullTime
		if err := rows.Scan(&im.InstanceID, &im.Model, &im.Role, &im.State, &im.Detail, &im.Version, &upd); err != nil {
			return nil, err
		}
		if upd.Valid {
			im.UpdatedAt = upd.Time
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

// DeleteInstanceModel remove um modelo da instância.
func (s *DB) DeleteInstanceModel(instID, model string) error {
	_, err := s.db.Exec(`DELETE FROM instance_models WHERE instance_id = ? AND model = ?`, instID, model)
	return err
}

// CommitSwap conclui um swap atomicamente: troca model/group_id da instância
// (UPDATE — nunca recria uma instância apagada), promove o alvo a principal e
// remove o principal antigo. ErrModelGone se instância ou alvo sumiram.
func (s *DB) CommitSwap(instID, oldModel, newModel, groupID string, version int64) error {
	return s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE instances SET model = ?, group_id = ? WHERE id = ?`, newModel, groupID, instID)
		if err := requireRow(res, err); err != nil {
			return err
		}
		res, err = tx.Exec(`UPDATE instance_models SET role = 'primary', state = 'ready', detail = '', version = ?, updated_at = ?
			WHERE instance_id = ? AND model = ? AND role IN ('swapping', 'extra')`, version, time.Now(), instID, newModel)
		if err := requireRow(res, err); err != nil {
			return err
		}
		if oldModel != "" && oldModel != newModel {
			_, err = tx.Exec(`DELETE FROM instance_models WHERE instance_id = ? AND model = ?`, instID, oldModel)
		}
		return err
	})
}

// SweepInstanceModels (startup) remove linhas órfãs (instância apagada) e
// falhas de add/swap mais velhas que maxAge. Devolve quantas removeu.
func (s *DB) SweepInstanceModels(maxAge time.Duration) (int64, error) {
	var total int64
	err := s.withTx(func(tx *sql.Tx) error {
		for _, q := range []struct {
			sql  string
			args []interface{}
		}{
			{`DELETE FROM instance_models WHERE instance_id NOT IN (SELECT id FROM instances)`, nil},
			{`DELETE FROM instance_models WHERE role != 'primary' AND state = 'failed' AND updated_at < ?`, []interface{}{time.Now().Add(-maxAge)}},
		} {
			res, err := tx.Exec(q.sql, q.args...)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			total += n
		}
		return nil
	})
	return total, err
}

// EnsureInstanceModel cria a linha só se ainda não existir (backfill do
// principal de instâncias antigas); nunca sobrescreve.
func (s *DB) EnsureInstanceModel(instID, model, role, state string, version int64) error {
	_, err := s.db.Exec(`INSERT INTO instance_models (instance_id, model, role, state, detail, version, updated_at)
		VALUES (?, ?, ?, ?, '', ?, ?) ON CONFLICT(instance_id, model) DO NOTHING`,
		instID, model, role, state, version, time.Now())
	return err
}
