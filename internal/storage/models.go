package storage

import (
	"database/sql"
	"time"
)

// Papéis de um modelo numa instância.
const (
	RolePrimary  = "primary"  // modelo principal (instances.model)
	RoleExtra    = "extra"    // adicional já roteado pelo proxy
	RoleAdding   = "adding"   // alvo de /models/add ainda baixando
	RoleSwapping = "swapping" // alvo de /swap ainda baixando
)

// InstanceModel é um modelo de uma instância com seu último estado conhecido
// ("loading", "ready", "failed" ou "" quando nunca verificado).
type InstanceModel struct {
	InstanceID string    `json:"instance_id"`
	Model      string    `json:"model"`
	Role       string    `json:"role"`
	State      string    `json:"state"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type execer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// upsertModel insere ou atualiza. role vazio preserva o papel existente (ou
// usa defaultRole na inserção); state vazio preserva o estado existente.
func upsertModel(e execer, instID, model, role, defaultRole, state string) error {
	insRole := role
	if insRole == "" {
		insRole = defaultRole
	}
	_, err := e.Exec(`INSERT INTO instance_models (instance_id, model, role, state, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(instance_id, model) DO UPDATE SET
			role = CASE WHEN ? = '' THEN role ELSE excluded.role END,
			state = CASE WHEN ? = '' THEN state ELSE excluded.state END,
			updated_at = excluded.updated_at`,
		instID, model, insRole, state, time.Now(), role, state)
	return err
}

// PutInstanceModel grava papel e estado de um modelo (state vazio mantém o atual).
func (s *DB) PutInstanceModel(instID, model, role, state string) error {
	return upsertModel(s.db, instID, model, role, role, state)
}

// SetInstanceModelState atualiza só o estado; se a linha não existe, cria com defaultRole.
func (s *DB) SetInstanceModelState(instID, model, defaultRole, state string) error {
	return upsertModel(s.db, instID, model, "", defaultRole, state)
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
	rows, err := s.db.Query(`SELECT instance_id, model, role, state, updated_at FROM instance_models `+where+
		` ORDER BY instance_id, CASE role WHEN 'primary' THEN 0 ELSE 1 END, rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstanceModel
	for rows.Next() {
		var im InstanceModel
		var upd sql.NullTime
		if err := rows.Scan(&im.InstanceID, &im.Model, &im.Role, &im.State, &upd); err != nil {
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

// PromoteExtraModel marca um alvo de add como extra pronto.
func (s *DB) PromoteExtraModel(instID, model string) error {
	return s.PutInstanceModel(instID, model, RoleExtra, "ready")
}

// CommitSwap conclui um swap atomicamente: grava a instância com o modelo
// novo, promove o alvo a principal (pronto) e remove o principal antigo.
func (s *DB) CommitSwap(inst *Instance, oldModel string) error {
	return s.withTx(func(tx *sql.Tx) error {
		if err := saveInstance(tx, inst); err != nil {
			return err
		}
		if oldModel != "" && oldModel != inst.Model {
			if _, err := tx.Exec(`DELETE FROM instance_models WHERE instance_id = ? AND model = ?`, inst.ID, oldModel); err != nil {
				return err
			}
		}
		return upsertModel(tx, inst.ID, inst.Model, RolePrimary, RolePrimary, "ready")
	})
}
