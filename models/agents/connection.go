// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"fmt"
	"slices"

	"gitea.dev/models/db"
	secret_module "gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"

	"xorm.io/builder"
)

type Provider string

const (
	ProviderAnthropic        Provider = "anthropic"
	ProviderOpenAICompatible Provider = "openai-compatible"
	ProviderOllama           Provider = "ollama"
)

func (p Provider) IsValid() bool {
	return slices.Contains([]Provider{ProviderAnthropic, ProviderOpenAICompatible, ProviderOllama}, p)
}

// CLIs are the agent CLI backends a connection can select.
const CLICodex = "codex"

type ConnectionAccessMode string

const (
	ConnectionAccessNone ConnectionAccessMode = "none" // visible to nobody but the admin
	ConnectionAccessAll  ConnectionAccessMode = "all"
	ConnectionAccessList ConnectionAccessMode = "list"
)

func (m ConnectionAccessMode) IsValid() bool {
	return slices.Contains([]ConnectionAccessMode{ConnectionAccessNone, ConnectionAccessAll, ConnectionAccessList}, m)
}

// ConnectionAccessEntries names who may use a global connection in "list" mode.
type ConnectionAccessEntries struct {
	UserIDs []int64
	OrgIDs  []int64
	TeamIDs []int64
}

// Connection describes a model provider. OwnerID 0 means a global (admin-managed) connection.
type Connection struct {
	ID          int64                    `xorm:"pk autoincr"`
	OwnerID     int64                    `xorm:"UNIQUE(owner_name) NOT NULL DEFAULT 0"`
	Name        string                   `xorm:"UNIQUE(owner_name) VARCHAR(255) NOT NULL"`
	Provider    Provider                 `xorm:"VARCHAR(32) NOT NULL"`
	BaseURL     string                   `xorm:"VARCHAR(2048) NOT NULL"`
	Token       string                   `xorm:"TEXT"` // encrypted, write-only
	CLI         string                   `xorm:"VARCHAR(32) NOT NULL"`
	AccessMode  ConnectionAccessMode     `xorm:"VARCHAR(16) NOT NULL DEFAULT 'none'"`
	AccessList  *ConnectionAccessEntries `xorm:"JSON TEXT"`
	CreatedUnix timeutil.TimeStamp       `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp       `xorm:"updated NOT NULL"`
}

func init() {
	db.RegisterModel(new(Connection))
}

var ErrConnectionNotFound = util.NewNotExistErrorf("agent connection does not exist")

// SetToken encrypts and stores the token; an empty token clears it.
func (c *Connection) SetToken(plain string) error {
	if plain == "" {
		c.Token = ""
		return nil
	}
	enc, err := secret_module.EncryptSecret(setting.SecretKey, plain)
	if err != nil {
		return fmt.Errorf("SetToken: %w", err)
	}
	c.Token = enc
	return nil
}

func (c *Connection) HasToken() bool {
	return c.Token != ""
}

// DecryptToken is only for building the run configuration; it must never reach a template or API response.
func (c *Connection) DecryptToken() (string, error) {
	if c.Token == "" {
		return "", nil
	}
	plain, err := secret_module.DecryptSecret(setting.SecretKey, c.Token)
	if err != nil {
		return "", fmt.Errorf("DecryptToken: %w", err)
	}
	return plain, nil
}

func (c *Connection) IsGlobal() bool {
	return c.OwnerID == 0
}

// Validate checks the user-supplied fields.
func (c *Connection) Validate() error {
	switch {
	case c.Name == "" || len(c.Name) > 255:
		return util.NewInvalidArgumentErrorf("connection name is required (max 255 characters)")
	case !c.Provider.IsValid():
		return util.NewInvalidArgumentErrorf("unknown provider %q", c.Provider)
	case c.BaseURL == "":
		return util.NewInvalidArgumentErrorf("base URL is required")
	case c.CLI == "":
		return util.NewInvalidArgumentErrorf("agent CLI is required")
	case !c.AccessMode.IsValid():
		return util.NewInvalidArgumentErrorf("unknown access mode %q", c.AccessMode)
	case !c.IsGlobal() && c.AccessMode != ConnectionAccessNone:
		return util.NewInvalidArgumentErrorf("only global connections can be shared")
	}
	return nil
}

func CreateConnection(ctx context.Context, c *Connection) error {
	if c.AccessMode == "" {
		c.AccessMode = ConnectionAccessNone
	}
	if err := c.Validate(); err != nil {
		return err
	}
	exist, err := db.GetEngine(ctx).Where("owner_id = ? AND name = ?", c.OwnerID, c.Name).Exist(new(Connection))
	if err != nil {
		return err
	} else if exist {
		return util.NewAlreadyExistErrorf("connection %q already exists", c.Name)
	}
	return db.Insert(ctx, c)
}

func GetConnectionByID(ctx context.Context, id int64) (*Connection, error) {
	c, exist, err := db.GetByID[Connection](ctx, id)
	if err != nil {
		return nil, err
	} else if !exist {
		return nil, ErrConnectionNotFound
	}
	return c, nil
}

// UpdateConnection saves everything but the token unless updateToken is set (the token is write-only).
func UpdateConnection(ctx context.Context, c *Connection, updateToken bool) error {
	if err := c.Validate(); err != nil {
		return err
	}
	cols := []string{"name", "provider", "base_url", "cli", "access_mode", "access_list"}
	if updateToken {
		cols = append(cols, "token")
	}
	_, err := db.GetEngine(ctx).ID(c.ID).Cols(cols...).Update(c)
	return err
}

// DeleteConnection refuses to delete a connection still used by an agent.
func DeleteConnection(ctx context.Context, id int64) error {
	used, err := db.GetEngine(ctx).Where("connection_id = ?", id).Exist(new(Agent))
	if err != nil {
		return err
	} else if used {
		return util.NewInvalidArgumentErrorf("connection is used by an agent")
	}
	_, err = db.GetEngine(ctx).ID(id).Delete(new(Connection))
	return err
}

type FindConnectionsOptions struct {
	db.ListOptions
	OwnerID int64 // 0 lists the global connections
}

func (opts FindConnectionsOptions) ToConds() builder.Cond {
	return builder.Eq{"owner_id": opts.OwnerID}
}

func (opts FindConnectionsOptions) ToOrders() string {
	return "name, id"
}

func (*Connection) TableName() string {
	return "agent_connection"
}
