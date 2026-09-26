package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/crom-org/openllm/internal/providers"
	"github.com/crom-org/openllm/internal/providers/mock"
	"github.com/crom-org/openllm/internal/storage"
)

// Regressão do vazamento em produção (2026-08-28): a instância 49071030 ficou
// alugada no Vast.ai sem ninguém gerenciando — o daemon reiniciou no meio de
// um deploy (status "deploying") e o resume de startup passava
// destroyOnFail=false para TODOS os statuses; quando o SSH falhou, a máquina
// foi preservada (markFailed) e continuou cobrando sem uso.
//
// Regra correta:
//   - resume de "deploying"  -> destrói na falha (deploy novo nunca terminou)
//   - resume de "running"    -> preserva na falha (reconexão; falha transitória)
func TestRecoverActiveInstancesDeployPolicy(t *testing.T) {
	setSSHRetry(t, 1, time.Millisecond)

	m, db, _ := newTestManager(t)
	cfg := testConfig()
	prov := mock.Default()

	// Duas máquinas vivas no provider mock, com SSH inacessível
	// (127.0.0.1:porta alta => conexão recusada) para forçar o caminho de falha.
	dep, err := prov.Deploy(context.Background(), providers.DeployRequest{MachineID: "mock-offer-001"})
	if err != nil {
		t.Fatalf("mock deploy (deploying): %v", err)
	}
	run, err := prov.Deploy(context.Background(), providers.DeployRequest{MachineID: "mock-offer-002"})
	if err != nil {
		t.Fatalf("mock deploy (running): %v", err)
	}

	// Banco espelhando o momento do crash do daemon:
	// dep  = deploy interrompido (nunca terminou o setup)
	// run  = máquina que já estava servindo (túneis perdidos no restart)
	seed := func(id, status string) {
		t.Helper()
		if err := db.SaveInstance(&storage.Instance{
			ID: id, Provider: "mock", MachineID: "mock-offer-001",
			Status: status, Model: "llama3.2:3b", Engine: "ollama",
			GroupID: "ollama/llama3.2:3b",
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed(dep.ID, "deploying")
	seed(run.ID, "running")

	d := &Daemon{cfg: cfg, db: db, manager: m}
	d.recoverActiveInstances()

	// "deploying": falha destrói no provider e remove do banco (anti-vazamento)
	waitForGone(t, db, dep.ID, 15*time.Second)
	if _, err := prov.GetStatus(context.Background(), dep.ID, ""); err == nil {
		t.Fatal("deploying instance survived failed recovery — money leak regression")
	}

	// "running": falha preserva a máquina no provider; registro fica "failed"
	waitForStatus(t, db, run.ID, "failed", 15*time.Second)
	if _, err := prov.GetStatus(context.Background(), run.ID, ""); err != nil {
		t.Fatal("running instance must be preserved at provider after failed recovery (reconnect semantics)")
	}
}
