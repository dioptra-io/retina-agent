// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// This file achieves 100% coverage of config.go.

//nolint:funlen // Test functions can be long for readability
package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// -- test helpers -------------------------------------------------------------

func validConfig() *Config {
	return DefaultConfig()
}

func createTempProber(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	proberPath := filepath.Join(tmpDir, "test-prober")
	//nolint:gosec // Test file needs to be executable
	if err := os.WriteFile(proberPath, []byte("#!/bin/sh\necho test\n"), 0755); err != nil {
		t.Fatalf("failed to create temp prober: %v", err)
	}
	return proberPath
}

func testDurationField(t *testing.T, fieldName string, setField func(*Config, time.Duration)) {
	t.Helper()
	t.Parallel()

	tests := []struct {
		name     string
		duration time.Duration
		wantErr  bool
	}{
		{
			name:     "valid duration",
			duration: 5 * time.Second,
			wantErr:  false,
		},
		{
			name:     "small valid duration",
			duration: 100 * time.Millisecond,
			wantErr:  false,
		},
		{
			name:     "zero duration",
			duration: 0,
			wantErr:  true,
		},
		{
			name:     "negative duration",
			duration: -1 * time.Second,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			setField(cfg, tt.duration)

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("%s: expected error but got none", fieldName)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("%s: unexpected error: %v", fieldName, err)
			}
		})
	}
}

// -- DefaultConfig ------------------------------------------------------------

func TestDefaultConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	if cfg.AgentID == "" {
		t.Error("AgentID should not be empty")
	}
	if cfg.OrchestratorAddr == "" {
		t.Error("OrchestratorAddr should not be empty")
	}
	if cfg.Secret != "" {
		t.Error("Secret should be empty by default (no auth)")
	}
	if cfg.ReadDeadline <= 0 {
		t.Error("ReadDeadline should be positive")
	}
	if cfg.WriteDeadline <= 0 {
		t.Error("WriteDeadline should be positive")
	}
	if cfg.MaxReconnectBackoff <= 0 {
		t.Error("MaxReconnectBackoff should be positive")
	}
	if cfg.ProberType != ProberTypeMock && cfg.ProberType != ProberTypeCaracal {
		t.Errorf("ProberType should be mock or caracal, got: %s", cfg.ProberType)
	}
	if cfg.WriteQueueSize <= 0 {
		t.Error("WriteQueueSize should be positive")
	}
	if cfg.CleanupInterval <= 0 {
		t.Error("CleanupInterval should be positive")
	}
	if cfg.ProbeTimeout <= 0 {
		t.Error("ProbeTimeout should be positive")
	}
	if cfg.PDsBufferSize <= 0 {
		t.Error("PDsBufferSize should be positive")
	}
	if cfg.FIEsBufferSize <= 0 {
		t.Error("FIEsBufferSize should be positive")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("DefaultConfig should validate successfully: %v", err)
	}
}

func TestProberTypeConstants(t *testing.T) {
	t.Parallel()

	if ProberTypeCaracal != "caracal" {
		t.Errorf("ProberTypeCaracal should be 'caracal', got: %s", ProberTypeCaracal)
	}
	if ProberTypeMock != "mock" {
		t.Errorf("ProberTypeMock should be 'mock', got: %s", ProberTypeMock)
	}
}

// -- Validate: agent identity -------------------------------------------------

func TestValidate_AgentID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		agentID string
		wantErr bool
	}{
		{
			name:    "valid agent ID",
			agentID: "agent-1",
			wantErr: false,
		},
		{
			name:    "empty agent ID",
			agentID: "",
			wantErr: true,
		},
		{
			name:    "complex agent ID",
			agentID: "prod-agent-us-east-1a",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			cfg.AgentID = tt.agentID

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected error but got none")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "agent ID") {
				t.Errorf("error should mention agent ID, got: %v", err)
			}
		})
	}
}

// -- Validate: orchestrator connection ----------------------------------------

func TestValidate_OrchestratorAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		addr    string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid hostname with port",
			addr:    "localhost:50050",
			wantErr: false,
		},
		{
			name:    "valid IP with port",
			addr:    "192.168.1.1:50050",
			wantErr: false,
		},
		{
			name:    "valid IPv6 with port",
			addr:    "[::1]:50050",
			wantErr: false,
		},
		{
			name:    "empty address",
			addr:    "",
			wantErr: true,
			errMsg:  "cannot be empty",
		},
		{
			name:    "missing port",
			addr:    "localhost",
			wantErr: true,
			errMsg:  "host:port format",
		},
		{
			name:    "port only",
			addr:    ":50050",
			wantErr: false, // net.SplitHostPort allows this
		},
		{
			name:    "invalid format",
			addr:    "localhost:port:extra",
			wantErr: true,
			errMsg:  "host:port format",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			cfg.OrchestratorAddr = tt.addr

			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				} else if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("error should contain %q, got: %v", tt.errMsg, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

// -- Validate: secret (authentication) ----------------------------------------

func TestValidate_Secret(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		secret  string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "empty secret (valid - no auth)",
			secret:  "",
			wantErr: false,
		},
		{ //nolint:gosec // G101: test value, not a real credential
			name:    "valid strong secret",
			secret:  "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6",
			wantErr: false,
		},
		{
			name:    "valid minimum length (16 chars)",
			secret:  "1234567890abcdef",
			wantErr: false,
		},
		{
			name:    "too short (15 chars)",
			secret:  "123456789012345",
			wantErr: true,
			errMsg:  "too short",
		},
		{
			name:    "too short (1 char)",
			secret:  "a",
			wantErr: true,
			errMsg:  "too short",
		},
		{
			name:    "weak secret: test",
			secret:  "test",
			wantErr: true,
			errMsg:  "weak/test value",
		},
		{
			name:    "weak secret: secret",
			secret:  "secret",
			wantErr: true,
			errMsg:  "weak/test value",
		},
		{
			name:    "weak secret: password",
			secret:  "password",
			wantErr: true,
			errMsg:  "weak/test value",
		},
		{
			name:    "weak secret: 123456",
			secret:  "123456",
			wantErr: true,
			errMsg:  "weak/test value",
		},
		{
			name:    "weak secret: abc123",
			secret:  "abc123",
			wantErr: true,
			errMsg:  "weak/test value",
		},
		{
			name:    "weak secret: changeme",
			secret:  "changeme",
			wantErr: true,
			errMsg:  "weak/test value",
		},
		{
			name:    "long strong secret",
			secret:  "thisIsAVeryLongAndStrongSecretThatIsDefinitelySecure123456",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			cfg.Secret = tt.secret

			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				} else if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("error should contain %q, got: %v", tt.errMsg, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

// -- Validate: deadlines and backoff ------------------------------------------

func TestValidate_Deadlines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setField func(*Config)
		wantErr  bool
		errMsg   string
	}{
		{
			name: "valid read deadline",
			setField: func(c *Config) {
				c.ReadDeadline = 30 * time.Second
			},
			wantErr: false,
		},
		{
			name: "zero read deadline",
			setField: func(c *Config) {
				c.ReadDeadline = 0
			},
			wantErr: true,
			errMsg:  "read deadline",
		},
		{
			name: "negative read deadline",
			setField: func(c *Config) {
				c.ReadDeadline = -1 * time.Second
			},
			wantErr: true,
			errMsg:  "read deadline",
		},
		{
			name: "zero write deadline",
			setField: func(c *Config) {
				c.WriteDeadline = 0
			},
			wantErr: true,
			errMsg:  "write deadline",
		},
		{
			name: "negative write deadline",
			setField: func(c *Config) {
				c.WriteDeadline = -1 * time.Second
			},
			wantErr: true,
			errMsg:  "write deadline",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			tt.setField(cfg)

			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("error should contain %q, got: %v", tt.errMsg, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestValidate_MaxReconnectBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backoff time.Duration
		wantErr bool
	}{
		{
			name:    "valid backoff",
			backoff: 5 * time.Minute,
			wantErr: false,
		},
		{
			name:    "zero backoff",
			backoff: 0,
			wantErr: true,
		},
		{
			name:    "negative backoff",
			backoff: -1 * time.Second,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			cfg.MaxReconnectBackoff = tt.backoff

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected error but got none")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidate_MaxConsecutiveDecodeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   int
		wantErr bool
	}{
		{
			name:    "zero (valid - never terminate)",
			value:   0,
			wantErr: false,
		},
		{
			name:    "positive value",
			value:   3,
			wantErr: false,
		},
		{
			name:    "large positive value",
			value:   100,
			wantErr: false,
		},
		{
			name:    "negative value",
			value:   -1,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			cfg.MaxConsecutiveDecodeErrors = tt.value

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected error but got none")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// -- Validate: prober configuration -------------------------------------------

func TestValidate_ProberType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		proberType string
		wantErr    bool
	}{
		{
			name:       "valid caracal",
			proberType: ProberTypeCaracal,
			wantErr:    false,
		},
		{
			name:       "valid mock",
			proberType: ProberTypeMock,
			wantErr:    false,
		},
		{
			name:       "invalid type",
			proberType: "invalid",
			wantErr:    true,
		},
		{
			name:       "empty type",
			proberType: "",
			wantErr:    true,
		},
		{
			name:       "wrong case",
			proberType: "Caracal",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			cfg.ProberType = tt.proberType

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected error but got none")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidate_ProberPath(t *testing.T) {
	t.Parallel()

	validPath := createTempProber(t)

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "empty path (valid - searches PATH)",
			path:    "",
			wantErr: false,
		},
		{
			name:    "valid existing path",
			path:    validPath,
			wantErr: false,
		},
		{
			name:    "non-existent path",
			path:    "/nonexistent/path/to/prober",
			wantErr: true,
		},
		{
			name:    "relative non-existent path",
			path:    "./nonexistent",
			wantErr: true,
		},
		{
			name:    "path is a directory",
			path:    t.TempDir(),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			cfg.ProberPath = tt.path

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected error but got none")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidate_WriteQueueSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{
			name:    "valid size",
			size:    1000,
			wantErr: false,
		},
		{
			name:    "small valid size",
			size:    1,
			wantErr: false,
		},
		{
			name:    "large valid size",
			size:    100000,
			wantErr: false,
		},
		{
			name:    "zero size",
			size:    0,
			wantErr: true,
		},
		{
			name:    "negative size",
			size:    -1,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			cfg.WriteQueueSize = tt.size

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected error but got none")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidate_CleanupInterval(t *testing.T) {
	testDurationField(t, "CleanupInterval", func(c *Config, d time.Duration) {
		c.CleanupInterval = d
	})
}

func TestValidate_ProbeTimeout(t *testing.T) {
	testDurationField(t, "ProbeTimeout", func(c *Config, d time.Duration) {
		c.ProbeTimeout = d
	})
}

// -- Validate: pipeline buffers -----------------------------------------------

func TestValidate_BufferSizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setField func(*Config)
		wantErr  bool
		errMsg   string
	}{
		{
			name: "valid buffer sizes",
			setField: func(c *Config) {
				c.PDsBufferSize = 100
				c.FIEsBufferSize = 100
			},
			wantErr: false,
		},
		{
			name: "large buffer sizes",
			setField: func(c *Config) {
				c.PDsBufferSize = 10000
				c.FIEsBufferSize = 10000
			},
			wantErr: false,
		},
		{
			name: "zero PDs buffer",
			setField: func(c *Config) {
				c.PDsBufferSize = 0
			},
			wantErr: true,
			errMsg:  "PDs buffer",
		},
		{
			name: "negative PDs buffer",
			setField: func(c *Config) {
				c.PDsBufferSize = -1
			},
			wantErr: true,
			errMsg:  "PDs buffer",
		},
		{
			name: "zero FIEs buffer",
			setField: func(c *Config) {
				c.FIEsBufferSize = 0
			},
			wantErr: true,
			errMsg:  "FIEs buffer",
		},
		{
			name: "negative FIEs buffer",
			setField: func(c *Config) {
				c.FIEsBufferSize = -1
			},
			wantErr: true,
			errMsg:  "FIEs buffer",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			tt.setField(cfg)

			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				} else if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("error should contain %q, got: %v", tt.errMsg, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

// -- Validate: complete config -------------------------------------------------

func TestValidate_ValidConfig(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config should pass validation: %v", err)
	}
}

func TestValidate_MultipleErrors(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.AgentID = ""
	cfg.ProbeTimeout = 0

	// Should return first error encountered (AgentID)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error but got none")
	}
	if !strings.Contains(err.Error(), "agent ID") {
		t.Errorf("expected first error to be about agent ID, got: %v", err)
	}
}
