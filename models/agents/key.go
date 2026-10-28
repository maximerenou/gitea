// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"

	"gitea.dev/models/db"
	secret_module "gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
)

// UserKey is a user's own API key for an agent, runs they trigger consume it and nobody else's
type UserKey struct {
	ID              int64
	AgentID         int64  `xorm:"UNIQUE(agent_user) INDEX NOT NULL"`
	UserID          int64  `xorm:"UNIQUE(agent_user) INDEX NOT NULL"`
	EncryptedAPIKey string `xorm:"TEXT"`
	Model           string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"` // overrides the agent's model when the agent allows it
	LastCheckUnix   timeutil.TimeStamp
	LastCheckError  string `xorm:"TEXT"`

	Created timeutil.TimeStamp `xorm:"created"`
	Updated timeutil.TimeStamp `xorm:"updated"`
}

func (*UserKey) TableName() string {
	return "agent_user_key"
}

func (k *UserKey) SetAPIKey(key string) error {
	encrypted, err := secret_module.EncryptSecret(setting.SecretKey, key)
	if err != nil {
		return err
	}
	k.EncryptedAPIKey = encrypted
	return nil
}

func (k *UserKey) APIKey() (string, error) {
	return secret_module.DecryptSecret(setting.SecretKey, k.EncryptedAPIKey)
}

// EffectiveModel is the model a user's runs use with an agent
func (k *UserKey) EffectiveModel(a *Agent) string {
	if a.AllowModelOverride && k.Model != "" {
		return k.Model
	}
	return a.Model
}

func GetUserKey(ctx context.Context, agentID, userID int64) (*UserKey, error) {
	k := &UserKey{}
	has, err := db.GetEngine(ctx).Where("agent_id = ? AND user_id = ?", agentID, userID).Get(k)
	if err != nil {
		return nil, err
	} else if !has || k.EncryptedAPIKey == "" {
		return nil, util.NewNotExistErrorf("user %d has no API key for agent %d", userID, agentID)
	}
	return k, nil
}

// FindUserKeys maps agent IDs to the keys of a user
func FindUserKeys(ctx context.Context, userID int64) (map[int64]*UserKey, error) {
	keys := make([]*UserKey, 0, 4)
	if err := db.GetEngine(ctx).Where("user_id = ?", userID).Find(&keys); err != nil {
		return nil, err
	}
	result := make(map[int64]*UserKey, len(keys))
	for _, k := range keys {
		result[k.AgentID] = k
	}
	return result, nil
}

func InsertUserKey(ctx context.Context, k *UserKey) error {
	return db.Insert(ctx, k)
}

func UpdateUserKey(ctx context.Context, k *UserKey, cols ...string) error {
	_, err := db.GetEngine(ctx).ID(k.ID).Cols(cols...).Update(k)
	return err
}

func DeleteUserKey(ctx context.Context, agentID, userID int64) error {
	_, err := db.GetEngine(ctx).Where("agent_id = ? AND user_id = ?", agentID, userID).Delete(new(UserKey))
	return err
}
