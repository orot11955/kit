package git

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type WorkflowConfig struct {
	Provider string `json:"provider"`
	Remote   string `json:"remote"`
	Stable   string `json:"stable"`
	Base     string `json:"base"`
	Source   string `json:"source"`
}

func DefaultWorkflowConfig() WorkflowConfig {
	return WorkflowConfig{
		Provider: "auto",
		Remote:   "origin",
		Stable:   "main",
		Base:     "develop",
		Source:   "work",
	}
}

var workflowConfigKeys = map[string]string{
	"git.provider": "kit.git.provider",
	"git.remote":   "kit.git.remote",
	"git.stable":   "kit.git.stable",
	"git.base":     "kit.git.base",
	"git.source":   "kit.git.source",
}

func WorkflowConfigNames() []string {
	result := make([]string, 0, len(workflowConfigKeys))
	for name := range workflowConfigKeys {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func configKey(name string) (string, error) {
	key, ok := workflowConfigKeys[name]
	if !ok {
		return "", fmt.Errorf("unknown repository config key %q", name)
	}
	return key, nil
}

func (s Service) WorkflowConfig(ctx context.Context) WorkflowConfig {
	config := DefaultWorkflowConfig()
	for name, target := range map[string]*string{
		"git.provider": &config.Provider,
		"git.remote":   &config.Remote,
		"git.stable":   &config.Stable,
		"git.base":     &config.Base,
		"git.source":   &config.Source,
	} {
		if value, err := s.ConfigGet(ctx, name); err == nil && value != "" {
			*target = value
		}
	}
	return config
}

func (s Service) ConfigGet(ctx context.Context, name string) (string, error) {
	key, err := configKey(name)
	if err != nil {
		return "", err
	}
	out, err := s.run(ctx, "config", "--local", "--get", key)
	if err != nil {
		return "", fmt.Errorf("repository config %q is not set", name)
	}
	return strings.TrimSpace(string(out)), nil
}

func (s Service) ConfigSet(ctx context.Context, name, value string) error {
	key, err := configKey(name)
	if err != nil {
		return err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("repository config %q must not be empty", name)
	}
	if name == "git.provider" && value != "auto" && value != "gitlab" && value != "forgejo" && value != "generic" {
		return fmt.Errorf("git.provider must be auto, gitlab, forgejo, or generic")
	}
	if name == "git.remote" {
		if err := validateRemoteName(value); err != nil {
			return err
		}
	}
	if name == "git.stable" || name == "git.base" || name == "git.source" {
		if err := s.ValidateBranchName(ctx, value); err != nil {
			return fmt.Errorf("%s must be a valid branch name: %w", name, err)
		}
	}
	_, err = s.run(ctx, "config", "--local", key, value)
	return err
}

func (s Service) ConfigUnset(ctx context.Context, name string) error {
	key, err := configKey(name)
	if err != nil {
		return err
	}
	_, err = s.run(ctx, "config", "--local", "--unset-all", key)
	return err
}

func (s Service) InitializeWorkflowConfig(ctx context.Context) (WorkflowConfig, error) {
	config := s.WorkflowConfig(ctx)
	values := map[string]string{
		"git.provider": config.Provider,
		"git.remote":   config.Remote,
		"git.stable":   config.Stable,
		"git.base":     config.Base,
		"git.source":   config.Source,
	}
	for _, name := range WorkflowConfigNames() {
		if err := s.ConfigSet(ctx, name, values[name]); err != nil {
			return WorkflowConfig{}, err
		}
	}
	return config, nil
}
