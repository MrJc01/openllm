// Package stack descreve stacks multimodais: um YAML declarando vários
// serviços (engine + modelo + réplicas + rotas do proxy) que sobem juntos e
// são atendidos por um único endpoint local.
package stack

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Service é um serviço do stack: um grupo de instâncias do mesmo modelo.
type Service struct {
	Name      string   `yaml:"name" json:"name"`
	Engine    string   `yaml:"engine,omitempty" json:"engine,omitempty"`
	Model     string   `yaml:"model" json:"model"`
	Instances int      `yaml:"instances,omitempty" json:"instances,omitempty"`
	Routes    []string `yaml:"routes,omitempty" json:"routes,omitempty"`
}

// Stack é o documento YAML completo.
type Stack struct {
	Name     string    `yaml:"name" json:"name"`
	Services []Service `yaml:"services" json:"services"`
}

// Parse lê e valida um stack em YAML. O nome é opcional no arquivo — se
// vier vazio, use fallbackName (ex: nome do arquivo).
func Parse(data []byte, fallbackName string) (*Stack, error) {
	var st Stack
	if err := yaml.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("invalid stack YAML: %w", err)
	}
	if st.Name == "" {
		st.Name = fallbackName
	}
	if st.Name == "" {
		return nil, fmt.Errorf("stack name is required (set 'name:' or pass the file name)")
	}
	if len(st.Services) == 0 {
		return nil, fmt.Errorf("stack must declare at least one service")
	}
	for i := range st.Services {
		svc := &st.Services[i]
		if svc.Model == "" {
			return nil, fmt.Errorf("service %q: model is required", svc.Name)
		}
		if svc.Name == "" {
			svc.Name = fmt.Sprintf("service-%d", i+1)
		}
		for j, route := range svc.Routes {
			if !strings.HasPrefix(route, "/") {
				return nil, fmt.Errorf("service %q: route %q must start with '/'", svc.Name, route)
			}
			svc.Routes[j] = strings.TrimSuffix(route, "/")
			if svc.Routes[j] == "" {
				svc.Routes[j] = "/"
			}
		}
	}
	return &st, nil
}