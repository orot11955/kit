package git

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type WorkflowConfig struct {
	Provider          string `json:"provider"`
	Remote            string `json:"remote"`
	PushRemote        string `json:"push_remote,omitempty"`
	Stable            string `json:"stable"`
	Base              string `json:"base"`
	Source            string `json:"source"`
	AllowInsecureHTTP bool   `json:"allow_insecure_http"`
}

func DefaultWorkflowConfig() WorkflowConfig {
	return WorkflowConfig{
		Provider: "gitea",
		Remote:   "origin",
		Stable:   "main",
		Base:     "develop",
		Source:   "work",
	}
}

func (c WorkflowConfig) PushRemoteName() string {
	if strings.TrimSpace(c.PushRemote) != "" {
		return c.PushRemote
	}
	return c.Remote
}

var workflowConfigKeys = map[string]string{
	"git.provider":            "kit.git.provider",
	"git.remote":              "kit.git.remote",
	"git.push-remote":         "kit.git.push-remote",
	"git.stable":              "kit.git.stable",
	"git.base":                "kit.git.base",
	"git.source":              "kit.git.source",
	"git.allow-insecure-http": "kit.git.allow-insecure-http",
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
	config, _ := s.WorkflowConfigStrict(ctx)
	return config
}

func (s Service) WorkflowConfigStrict(ctx context.Context) (WorkflowConfig, error) {
	config := DefaultWorkflowConfig()
	for name, target := range map[string]*string{
		"git.provider":    &config.Provider,
		"git.remote":      &config.Remote,
		"git.push-remote": &config.PushRemote,
		"git.stable":      &config.Stable,
		"git.base":        &config.Base,
		"git.source":      &config.Source,
	} {
		value, err := s.ConfigGet(ctx, name)
		if errors.Is(err, ErrConfigNotSet) {
			continue
		}
		if err != nil {
			return config, fmt.Errorf("read repository config %q: %w", name, err)
		}
		if value != "" {
			*target = value
		}
	}
	value, err := s.ConfigGet(ctx, "git.allow-insecure-http")
	if err == nil {
		config.AllowInsecureHTTP = value == "true"
	} else if !errors.Is(err, ErrConfigNotSet) {
		return config, fmt.Errorf("read repository config %q: %w", "git.allow-insecure-http", err)
	}
	return config, nil
}

func (s Service) ConfigGet(ctx context.Context, name string) (string, error) {
	key, err := configKey(name)
	if err != nil {
		return "", err
	}
	out, err := s.run(ctx, "config", "--local", "--get", key)
	if err != nil {
		if IsExitCode(err, 1) {
			return "", fmt.Errorf("%w: %q", ErrConfigNotSet, name)
		}
		return "", err
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
	if name == "git.provider" {
		switch value {
		case "auto", "gitea", "generic":
		case "gitlab", "forgejo":
			current, currentErr := s.ConfigGet(ctx, name)
			if currentErr != nil || current != value {
				return fmt.Errorf("git.provider %s is legacy and cannot be newly configured; use gitea", value)
			}
		default:
			return fmt.Errorf("git.provider must be auto, gitea, or generic")
		}
	}
	if name == "git.allow-insecure-http" && value != "true" && value != "false" {
		return fmt.Errorf("git.allow-insecure-http must be true or false")
	}
	if name == "git.remote" || name == "git.push-remote" {
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

func (s Service) MarkKitCreatedBranch(ctx context.Context, branch string) error {
	if err := s.ValidateBranchName(ctx, branch); err != nil {
		return err
	}
	_, err := s.run(ctx, "config", "--local", "branch."+branch+".kitCreated", "true")
	return err
}

func (s Service) IsKitCreatedBranch(ctx context.Context, branch string) (bool, error) {
	if err := s.ValidateBranchName(ctx, branch); err != nil {
		return false, err
	}
	branches, err := s.KitCreatedBranches(ctx)
	if err != nil {
		return false, err
	}
	for _, marked := range branches {
		if marked == branch {
			return true, nil
		}
	}
	return false, nil
}

func (s Service) ClearKitCreatedBranch(ctx context.Context, branch string) error {
	if err := s.ValidateBranchName(ctx, branch); err != nil {
		return err
	}
	_, err := s.run(ctx, "config", "--local", "--unset-all", "branch."+branch+".kitCreated")
	if err != nil {
		if IsExitCode(err, 1) || IsExitCode(err, 5) {
			return nil
		}
		return err
	}
	return nil
}

func (s Service) KitCreatedBranches(ctx context.Context) ([]string, error) {
	out, err := s.run(ctx, "config", "--local", "--get-regexp", "^branch\\..*\\.kitcreated$")
	if err != nil {
		if IsExitCode(err, 1) {
			return nil, nil
		}
		return nil, err
	}
	const prefix = "branch."
	const suffix = ".kitcreated"
	values := make(map[string][]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := fields[0]
		lowerKey := strings.ToLower(key)
		if !strings.HasPrefix(lowerKey, prefix) || !strings.HasSuffix(lowerKey, suffix) {
			continue
		}
		branch := key[len(prefix) : len(key)-len(suffix)]
		if err := s.ValidateBranchName(ctx, branch); err != nil {
			continue
		}
		values[branch] = append(values[branch], strings.TrimSpace(strings.TrimPrefix(line, key)))
	}
	var branches []string
	for branch, markerValues := range values {
		if len(markerValues) == 1 && markerValues[0] == "true" {
			branches = append(branches, branch)
		}
	}
	sort.Strings(branches)
	return branches, nil
}

func (s Service) InitializeWorkflowConfig(ctx context.Context) (WorkflowConfig, error) {
	config, err := s.WorkflowConfigStrict(ctx)
	if err != nil {
		return WorkflowConfig{}, err
	}
	values := map[string]string{
		"git.provider":            config.Provider,
		"git.remote":              config.Remote,
		"git.push-remote":         config.PushRemote,
		"git.stable":              config.Stable,
		"git.base":                config.Base,
		"git.source":              config.Source,
		"git.allow-insecure-http": strconv.FormatBool(config.AllowInsecureHTTP),
	}
	for _, name := range WorkflowConfigNames() {
		if name == "git.push-remote" && values[name] == "" {
			continue
		}
		if err := s.ConfigSet(ctx, name, values[name]); err != nil {
			return WorkflowConfig{}, err
		}
	}
	return config, nil
}
