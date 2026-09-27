package storage

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/crom-org/openllm/internal/config"
	_ "github.com/mattn/go-sqlite3"
)

type Instance struct {
	ID          string  `json:"id"`
	Provider    string  `json:"provider"`
	MachineID   string  `json:"machine_id"`
	GPU         string  `json:"gpu"`
	GPUCount    int     `json:"gpu_count"`
	VRAM        float64 `json:"vram"`
	CostPerHour float64 `json:"cost_per_hour"`
	SSHHost     string  `json:"ssh_host"`
	SSHPort     int     `json:"ssh_port"`
	Status      string  `json:"status"` // "deploying", "running", "stopped", "failed"
	Model       string  `json:"model"`
	Engine      string  `json:"engine"` // "ollama" (default), "localai"
	GroupID     string  `json:"group_id,omitempty"`
	// EngineDefJSON preserva a Definition da engine usada no deploy (engines
	// custom/externas sobrevivem a restarts do daemon).
	EngineDefJSON string     `json:"engine_def,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StoppedAt     *time.Time `json:"stopped_at,omitempty"`
}

// Group representa um grupo de escala: um modelo em N instâncias, com as
// rotas do proxy (stacks multimodais) associadas.
type Group struct {
	ID        string    `json:"id"` // "engine/model"
	Stack     string    `json:"stack,omitempty"`
	Routes    []string  `json:"routes,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Profile struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	TpsTarget      float64   `json:"tps_target"`
	InstancesCount int       `json:"instances_count"`
	CreatedAt      time.Time `json:"created_at"`
}

type DB struct {
	db *sql.DB
}

// OpenDB abre (e inicializa se necessário) o banco SQLite local
func OpenDB() (*DB, error) {
	dbPath, err := config.GetDbPath()
	if err != nil {
		return nil, err
	}
	return openDBAt(dbPath)
}

// OpenDBInDir abre o banco em um diretório específico (usado por testes).
func OpenDBInDir(dir string) (*DB, error) {
	return openDBAt(filepath.Join(dir, config.DbFileName))
}

func openDBAt(dbPath string) (*DB, error) {
	// busy_timeout: leituras (/status) não falham com "database is
	// locked" enquanto deploys paralelos gravam; escritas esperam até 5s.
	conn, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}

	s := &DB{db: conn}
	if err := s.initSchema(); err != nil {
		conn.Close()
		return nil, err
	}

	return s, nil
}

func (s *DB) Close() error {
	return s.db.Close()
}

func (s *DB) initSchema() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS profiles (
			id TEXT PRIMARY KEY,
			name TEXT UNIQUE,
			provider TEXT,
			model TEXT,
			tps_target REAL,
			instances_count INTEGER,
			created_at DATETIME
		);`,
		`CREATE TABLE IF NOT EXISTS instances (
			id TEXT PRIMARY KEY,
			provider TEXT,
			machine_id TEXT,
			gpu TEXT,
			gpu_count INTEGER,
			vram REAL,
			cost_per_hour REAL,
			ssh_host TEXT,
			ssh_port INTEGER,
			status TEXT,
			model TEXT,
			created_at DATETIME,
			stopped_at DATETIME
		);`,
		// instance_models: conjunto de modelos servidos por instância (principal,
		// extras e alvos de add/swap em andamento) e o último estado conhecido.
		// Tabela nova (IF NOT EXISTS): bancos antigos migram sem perda.
		`CREATE TABLE IF NOT EXISTS instance_models (
			instance_id TEXT NOT NULL,
			model TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'extra',
			state TEXT NOT NULL DEFAULT '',
			updated_at DATETIME,
			PRIMARY KEY (instance_id, model)
		);`,
		`CREATE TABLE IF NOT EXISTS groups (
			id TEXT PRIMARY KEY,
			stack TEXT,
			routes TEXT,
			created_at DATETIME
		);`,
	}

	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}

	// Migração best-effort: bancos criados antes do campo `engine` existir
	// não têm a coluna. SQLite não suporta "ADD COLUMN IF NOT EXISTS", então
	// tentamos e ignoramos o erro de coluna duplicada.
	if _, err := s.db.Exec(`ALTER TABLE instances ADD COLUMN engine TEXT DEFAULT 'ollama'`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	if _, err := s.db.Exec(`ALTER TABLE instances ADD COLUMN group_id TEXT DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	if _, err := s.db.Exec(`ALTER TABLE instances ADD COLUMN def_json TEXT DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}

	return nil
}

// --- Operações de Perfis ---

func (s *DB) SaveProfile(p *Profile) error {
	query := `INSERT OR REPLACE INTO profiles (id, name, provider, model, tps_target, instances_count, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.Exec(query, p.ID, p.Name, p.Provider, p.Model, p.TpsTarget, p.InstancesCount, p.CreatedAt)
	return err
}

func (s *DB) GetProfile(name string) (*Profile, error) {
	query := `SELECT id, name, provider, model, tps_target, instances_count, created_at FROM profiles WHERE name = ?`
	row := s.db.QueryRow(query, name)

	var p Profile
	err := row.Scan(&p.ID, &p.Name, &p.Provider, &p.Model, &p.TpsTarget, &p.InstancesCount, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *DB) ListProfiles() ([]Profile, error) {
	query := `SELECT id, name, provider, model, tps_target, instances_count, created_at FROM profiles ORDER BY name`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []Profile
	for rows.Next() {
		var p Profile
		if err := rows.Scan(&p.ID, &p.Name, &p.Provider, &p.Model, &p.TpsTarget, &p.InstancesCount, &p.CreatedAt); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}

func (s *DB) DeleteProfile(name string) error {
	query := `DELETE FROM profiles WHERE name = ?`
	_, err := s.db.Exec(query, name)
	return err
}

// --- Operações de Grupos ---

func (s *DB) SaveGroup(g *Group) error {
	routes := ""
	if len(g.Routes) > 0 {
		b, _ := json.Marshal(g.Routes)
		routes = string(b)
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO groups (id, stack, routes, created_at) VALUES (?, ?, ?, ?)`,
		g.ID, g.Stack, routes, g.CreatedAt)
	return err
}

func (s *DB) GetGroup(id string) (*Group, error) {
	row := s.db.QueryRow(`SELECT id, stack, routes, created_at FROM groups WHERE id = ?`, id)
	var g Group
	var routes string
	err := row.Scan(&g.ID, &g.Stack, &routes, &g.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if routes != "" {
		_ = json.Unmarshal([]byte(routes), &g.Routes)
	}
	return &g, nil
}

func (s *DB) ListGroups() ([]Group, error) {
	rows, err := s.db.Query(`SELECT id, stack, routes, created_at FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []Group
	for rows.Next() {
		var g Group
		var routes string
		if err := rows.Scan(&g.ID, &g.Stack, &routes, &g.CreatedAt); err != nil {
			return nil, err
		}
		if routes != "" {
			_ = json.Unmarshal([]byte(routes), &g.Routes)
		}
		groups = append(groups, g)
	}
	return groups, nil
}

func (s *DB) DeleteGroup(id string) error {
	_, err := s.db.Exec(`DELETE FROM groups WHERE id = ?`, id)
	return err
}

// --- Operações de Instâncias ---

func (s *DB) SaveInstance(inst *Instance) error {
	return saveInstance(s.db, inst)
}

func saveInstance(e execer, inst *Instance) error {
	query := `INSERT OR REPLACE INTO instances (id, provider, machine_id, gpu, gpu_count, vram, cost_per_hour, ssh_host, ssh_port, status, model, engine, group_id, def_json, created_at, stopped_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	var stoppedAtVal interface{}
	if inst.StoppedAt != nil {
		stoppedAtVal = *inst.StoppedAt
	}

	engine := inst.Engine
	if engine == "" {
		engine = "ollama"
	}

	_, err := e.Exec(query, inst.ID, inst.Provider, inst.MachineID, inst.GPU, inst.GPUCount, inst.VRAM, inst.CostPerHour, inst.SSHHost, inst.SSHPort, inst.Status, inst.Model, engine, inst.GroupID, inst.EngineDefJSON, inst.CreatedAt, stoppedAtVal)
	return err
}

const instanceCols = `id, provider, machine_id, gpu, gpu_count, vram, cost_per_hour, ssh_host, ssh_port, status, model, engine, group_id, def_json, created_at, stopped_at`

func scanInstance(scanner interface {
	Scan(dest ...interface{}) error
}) (*Instance, error) {
	var inst Instance
	var stoppedAtVal sql.NullTime
	err := scanner.Scan(&inst.ID, &inst.Provider, &inst.MachineID, &inst.GPU, &inst.GPUCount, &inst.VRAM, &inst.CostPerHour, &inst.SSHHost, &inst.SSHPort, &inst.Status, &inst.Model, &inst.Engine, &inst.GroupID, &inst.EngineDefJSON, &inst.CreatedAt, &stoppedAtVal)
	if err != nil {
		return nil, err
	}
	if stoppedAtVal.Valid {
		inst.StoppedAt = &stoppedAtVal.Time
	}
	return &inst, nil
}

func (s *DB) GetInstance(id string) (*Instance, error) {
	row := s.db.QueryRow(`SELECT `+instanceCols+` FROM instances WHERE id = ?`, id)
	inst, err := scanInstance(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return inst, nil
}

func (s *DB) ListActiveInstances() ([]Instance, error) {
	return s.listInstances(`SELECT `+instanceCols+` FROM instances WHERE status IN ('deploying', 'running') ORDER BY created_at DESC`, nil)
}

// ListInstancesByGroup retorna todas as instâncias (qualquer status) do grupo.
func (s *DB) ListInstancesByGroup(groupID string) ([]Instance, error) {
	return s.listInstances(`SELECT `+instanceCols+` FROM instances WHERE group_id = ? ORDER BY created_at DESC`, groupID)
}

// CountActiveByGroup conta instâncias ativas (running/deploying) de um grupo.
func (s *DB) CountActiveByGroup(groupID string) (int, error) {
	row := s.db.QueryRow(`SELECT COUNT(*) FROM instances WHERE group_id = ? AND status IN ('deploying', 'running')`, groupID)
	var count int
	err := row.Scan(&count)
	return count, err
}

func (s *DB) ListAllInstances() ([]Instance, error) {
	return s.listInstances(`SELECT `+instanceCols+` FROM instances ORDER BY created_at DESC`, nil)
}

func (s *DB) listInstances(query string, arg interface{}) ([]Instance, error) {
	var rows *sql.Rows
	var err error
	if arg != nil {
		rows, err = s.db.Query(query, arg)
	} else {
		rows, err = s.db.Query(query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, *inst)
	}
	return instances, rows.Err()
}

// DeleteInstance remove a instância e seus modelos na mesma transação.
func (s *DB) DeleteInstance(id string) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM instance_models WHERE instance_id = ?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM instances WHERE id = ?`, id)
		return err
	})
}

func (s *DB) withTx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
