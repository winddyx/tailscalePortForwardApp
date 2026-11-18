package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PortSpec represents either a single port (1-65535) or a comma-separated list of ports.
// It unmarshals from both JSON numbers and JSON strings.
type PortSpec string

// UnmarshalJSON parses a port specification from a JSON number or string.
func (p *PortSpec) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*p = ""
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*p = PortSpec(strings.TrimSpace(s))
		return nil
	}
	var num int
	if err := json.Unmarshal(data, &num); err != nil {
		return err
	}
	*p = PortSpec(strconv.Itoa(num))
	return nil
}

// ForwardRule defines a single resolved TCP port forwarding rule from Tailscale IP to target.
type ForwardRule struct {
	Port   int
	Target string
}

// UserConfig defines a Tailscale user/tailnet account to connect concurrently.
type UserConfig struct {
	Name       string   `json:"name"`
	AuthKey    string   `json:"auth_key,omitempty"`
	ControlURL string   `json:"control_url,omitempty"`
	Ports      PortSpec `json:"ports,omitempty"`
	Target     string   `json:"target,omitempty"`
}

// Config represents the JSON configuration file for gtsa.
type Config struct {
	Hostname   string       `json:"hostname"`
	StateDir   string       `json:"state_dir,omitempty"`
	ControlURL string       `json:"control_url,omitempty"`
	Verbose    bool         `json:"verbose,omitempty"`
	Ports      PortSpec     `json:"ports"`
	Target     string       `json:"target,omitempty"`
	Users      []UserConfig `json:"users"`
}

// ResolvedUser holds validated and normalized runtime settings for a single Tailscale user node.
type ResolvedUser struct {
	Name       string
	AuthKey    string
	ControlURL string
	StateDir   string
	Forwards   []ForwardRule
}

// ResolvedConfig holds the validated runtime configuration.
type ResolvedConfig struct {
	Hostname  string
	StateRoot string
	Verbose   bool
	Users     []ResolvedUser
}

// LoadConfig reads, validates, and normalizes a config.json file.
func LoadConfig(path string, portableDir string) (*ResolvedConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	var raw Config
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	return resolveConfig(raw, portableDir)
}

func resolveConfig(raw Config, portableDir string) (*ResolvedConfig, error) {
	hostname := strings.TrimSpace(raw.Hostname)
	if hostname == "" {
		hostname = "tspfa"
	}

	stateRoot := strings.TrimSpace(raw.StateDir)
	if stateRoot == "" {
		stateRoot = filepath.Join(portableDir, "state")
	} else if !filepath.IsAbs(stateRoot) {
		stateRoot = filepath.Join(portableDir, stateRoot)
	}
	stateRoot = filepath.Clean(stateRoot)

	var globalForwards []ForwardRule
	if strings.TrimSpace(string(raw.Ports)) != "" {
		ports, err := parsePorts(string(raw.Ports))
		if err != nil {
			return nil, fmt.Errorf("global ports: %w", err)
		}
		globalForwards = buildForwardRules(ports, raw.Target)
	}

	if len(raw.Users) == 0 {
		return nil, fmt.Errorf("users list cannot be empty")
	}

	seenUsers := make(map[string]struct{}, len(raw.Users))
	resolvedUsers := make([]ResolvedUser, 0, len(raw.Users))

	for i, u := range raw.Users {
		name := strings.TrimSpace(u.Name)
		if err := validateUserName(name); err != nil {
			return nil, fmt.Errorf("users[%d].name: %w", i, err)
		}

		// Use lower-case key to prevent state directory collisions on case-insensitive filesystems.
		lowerName := strings.ToLower(name)
		if _, exists := seenUsers[lowerName]; exists {
			return nil, fmt.Errorf("duplicate user name %q (case-insensitive)", name)
		}
		seenUsers[lowerName] = struct{}{}

		userForwards := globalForwards
		if strings.TrimSpace(string(u.Ports)) != "" {
			ports, err := parsePorts(string(u.Ports))
			if err != nil {
				return nil, fmt.Errorf("users[%d] (%s) ports: %w", i, name, err)
			}
			target := raw.Target
			if strings.TrimSpace(u.Target) != "" {
				target = u.Target
			}
			userForwards = buildForwardRules(ports, target)
		} else if strings.TrimSpace(u.Target) != "" {
			if len(globalForwards) == 0 {
				return nil, fmt.Errorf("users[%d] (%s): no ports configured", i, name)
			}
			ports := make([]int, len(globalForwards))
			for j, f := range globalForwards {
				ports[j] = f.Port
			}
			userForwards = buildForwardRules(ports, u.Target)
		}

		if len(userForwards) == 0 {
			return nil, fmt.Errorf("users[%d] (%s): no ports configured (specify ports like \"445,80\")", i, name)
		}

		controlURL := strings.TrimSpace(u.ControlURL)
		if controlURL == "" {
			controlURL = strings.TrimSpace(raw.ControlURL)
		}

		resolvedUsers = append(resolvedUsers, ResolvedUser{
			Name:       name,
			AuthKey:    strings.TrimSpace(u.AuthKey),
			ControlURL: controlURL,
			StateDir:   filepath.Join(stateRoot, name),
			Forwards:   userForwards,
		})
	}

	return &ResolvedConfig{
		Hostname:  hostname,
		StateRoot: stateRoot,
		Verbose:   raw.Verbose,
		Users:     resolvedUsers,
	}, nil
}

func validateUserName(name string) error {
	if name == "" {
		return fmt.Errorf("must not be empty")
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid name %q: must not start with '.'", name)
	}
	for _, r := range name {
		isValid := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.'
		if !isValid {
			return fmt.Errorf("invalid character %q in %q (allowed: a-z, A-Z, 0-9, '-', '_', '.')", r, name)
		}
	}
	return nil
}

func parsePorts(spec string) ([]int, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return nil, fmt.Errorf("ports must not be empty")
	}

	parts := strings.Split(trimmed, ",")
	ports := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))

	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" {
			return nil, fmt.Errorf("empty port entry in %q", spec)
		}
		p, err := strconv.Atoi(token)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("port %q must be an integer between 1 and 65535", token)
		}
		if _, exists := seen[p]; !exists {
			seen[p] = struct{}{}
			ports = append(ports, p)
		}
	}

	return ports, nil
}

func buildForwardRules(ports []int, rawTarget string) []ForwardRule {
	rules := make([]ForwardRule, 0, len(ports))
	for _, p := range ports {
		rules = append(rules, ForwardRule{
			Port:   p,
			Target: normalizeTarget(rawTarget, p),
		})
	}
	return rules
}
