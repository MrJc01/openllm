package ssh

import (
	"strings"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/crom-org/openllm/internal/config"
	"golang.org/x/crypto/ssh"
)

// EnsureKeysExist verifica se as chaves SSH existem em .openllm/id_ed25519.
// Se não existirem, gera (ed25519) e registra a chave pública no Vast.ai usando a API key.
// Se já existirem, GARANTE que a chave pública continua registrada no provedor
// (registro idempotente, re-validado a cada 12h) — chaves removidas da conta
// quebravam todos os deploys seguintes com "unable to authenticate".
//
// ed25519 substitui o RSA-2048 histórico: hosts novos do provedor passaram a
// rejeitar RSA no handshake ("no supported methods remain"), travando todo
// deploy. ed25519 é o padrão moderno aceito universalmente.
func EnsureKeysExist(apiKey string) (string, string, error) {
	stateDir, err := config.GetStateDir()
	if err != nil {
		return "", "", err
	}

	privPath := filepath.Join(stateDir, "id_ed25519")
	pubPath := filepath.Join(stateDir, "id_ed25519.pub")

	if _, err := os.Stat(privPath); err != nil {
		if err := generateKeyPair(privPath, pubPath); err != nil {
			return "", "", err
		}
		// Chave nova: registra imediatamente
		if apiKey != "" {
			pubBytes, err := ioutil.ReadFile(pubPath)
			if err != nil {
				return "", "", err
			}
			if err := registerKeyOnVastAi(apiKey, string(pubBytes)); err != nil {
				return "", "", fmt.Errorf("failed to register SSH key on provider (SSH auth will fail): %w", err)
			}
			_ = ioutil.WriteFile(filepath.Join(stateDir, keyRegisteredMarker), []byte(time.Now().Format(time.RFC3339)), 0644)
		}
		return privPath, pubPath, nil
	}

	// Chave existe: re-registra periodicamente para se autocurar de remoções
	// na conta do provedor (fallo silencioso de SSH = máquina queimando $).
	if apiKey != "" {
		marker := filepath.Join(stateDir, keyRegisteredMarker)
		if needsRevalidation(marker) {
			pubBytes, err := ioutil.ReadFile(pubPath)
			if err == nil {
				if err := registerKeyOnVastAi(apiKey, string(pubBytes)); err != nil {
					return "", "", fmt.Errorf("failed to re-register SSH key on provider: %w", err)
				}
				_ = ioutil.WriteFile(marker, []byte(time.Now().Format(time.RFC3339)), 0644)
			}
		}
	}

	return privPath, pubPath, nil
}

const keyRegisteredMarker = ".key_registered_at"
const keyRevalidateInterval = 12 * time.Hour

func needsRevalidation(marker string) bool {
	data, err := ioutil.ReadFile(marker)
	if err != nil {
		return true // nunca registrado
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return true
	}
	return time.Since(t) > keyRevalidateInterval
}

func generateKeyPair(privPath, pubPath string) error {
	// Gerar novo par de chaves ed25519
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate private key: %w", err)
	}

	privBlock, err := ssh.MarshalPrivateKey(privKey, "openllm-managed")
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}
	privFile, err := os.OpenFile(privPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer privFile.Close()
	if err := pem.Encode(privFile, privBlock); err != nil {
		return err
	}

	pubSSH, err := ssh.NewPublicKey(privKey.Public())
	if err != nil {
		return err
	}
	pubBytes := ssh.MarshalAuthorizedKey(pubSSH)

	return ioutil.WriteFile(pubPath, pubBytes, 0644)
}

func registerKeyOnVastAi(apiKey string, pubKey string) error {
	url := "https://console.vast.ai/api/v0/ssh/"
	payload := map[string]string{
		"ssh_key": pubKey,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		bodyStr := string(body)
		// Chave já registrada = sucesso idempotente
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(bodyStr, "duplicate") {
			return nil
		}
		return fmt.Errorf("vast.ai returned status %d: %s", resp.StatusCode, bodyStr)
	}
	return nil
}

type SSHClient struct {
	client *ssh.Client
}

func Connect(host string, port int, privKeyPath string) (*SSHClient, error) {
	keyBytes, err := ioutil.ReadFile(privKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key %s: %w", privKeyPath, err)
	}

	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	sshConfig := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	client, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to dial ssh %s: %w", addr, err)
	}

	return &SSHClient{client: client}, nil
}

func (s *SSHClient) Close() error {
	return s.client.Close()
}

// RunCommand executa um comando remoto e retorna stdout/stderr combinados
func (s *SSHClient) RunCommand(cmd string) (string, error) {
	session, err := s.client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	var buf bytes.Buffer
	session.Stdout = &buf
	session.Stderr = &buf

	err = session.Run(cmd)
	return buf.String(), err
}

// CopyFile copia o conteúdo para o destino especificado na máquina remota via piping no stdin
func (s *SSHClient) CopyFile(destPath string, content []byte, mode string) error {
	session, err := s.client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	session.Stdin = bytes.NewReader(content)
	cmd := fmt.Sprintf("cat > %s && chmod %s %s", destPath, mode, destPath)
	
	var errBuf bytes.Buffer
	session.Stderr = &errBuf

	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("failed to copy file: %s (error: %w)", errBuf.String(), err)
	}

	return nil
}

// StartReverseTunnel abre um túnel reverso da porta remota (ex: 127.0.0.1:17291)
// para a porta local correspondente (ex: 127.0.0.1:17291)
func (s *SSHClient) StartReverseTunnel(ctx context.Context, remoteAddr, localAddr string) error {
	// Escuta na porta remota
	listener, err := s.client.Listen("tcp", remoteAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on remote address %s: %w", remoteAddr, err)
	}

	go func() {
		defer listener.Close()

		// Encerra o túnel se o contexto for cancelado
		go func() {
			<-ctx.Done()
			listener.Close()
		}()

		for {
			remoteConn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return // Fechamento esperado
				default:
					fmt.Printf("Tunnel accept error: %v\n", err)
					return
				}
			}

			go func(remote net.Conn) {
				defer remote.Close()

				// Conecta ao daemon local
				localConn, err := net.Dial("tcp", localAddr)
				if err != nil {
					fmt.Printf("Failed to dial local address %s: %v\n", localAddr, err)
					return
				}
				defer localConn.Close()

				// Bidirectional copy
				errChan := make(chan error, 2)
				go func() {
					_, err := io.Copy(localConn, remote)
					errChan <- err
				}()
				go func() {
					_, err := io.Copy(remote, localConn)
					errChan <- err
				}()

				// Aguarda um dos lados terminar
				<-errChan
			}(remoteConn)
		}
	}()

	return nil
}

// StartLocalForward escuta em um endereço local e encaminha conexões para o endereço remoto
func (s *SSHClient) StartLocalForward(ctx context.Context, localAddr, remoteAddr string) error {
	listener, err := net.Listen("tcp", localAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on local address %s: %w", localAddr, err)
	}

	go func() {
		defer listener.Close()

		go func() {
			<-ctx.Done()
			listener.Close()
		}()

		for {
			localConn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					fmt.Printf("Local forward accept error: %v\n", err)
					return
				}
			}

			go func(local net.Conn) {
				defer local.Close()

				// Estabelece conexão através do canal SSH
				remoteConn, err := s.client.Dial("tcp", remoteAddr)
				if err != nil {
					fmt.Printf("Failed to dial remote address %s via SSH: %v\n", remoteAddr, err)
					return
				}
				defer remoteConn.Close()

				errChan := make(chan error, 2)
				go func() {
					_, err := io.Copy(remoteConn, local)
					errChan <- err
				}()
				go func() {
					_, err := io.Copy(local, remoteConn)
					errChan <- err
				}()

				<-errChan
			}(localConn)
		}
	}()

	return nil
}

