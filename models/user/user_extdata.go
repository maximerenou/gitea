// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package user

import (
	"errors"
	"strconv"
	"strings"

	"gitea.dev/models/auth"
)

type ExtDoerData interface {
	EncodeToString() string
	DecodeFromString(string) error
}

type extDoerGiteaActions struct {
	TaskID int64
}

var _ ExtDoerData = (*extDoerGiteaActions)(nil)

func (e *extDoerGiteaActions) EncodeToString() string {
	return "gitea-actions:" + strconv.FormatInt(e.TaskID, 10)
}

func (e *extDoerGiteaActions) DecodeFromString(s string) (err error) {
	idStr, _ := strings.CutPrefix(s, "gitea-actions:")
	e.TaskID, err = strconv.ParseInt(idStr, 10, 64)
	return err
}

type extDoerDeployKey struct {
	DeployKeyID int64
}

var _ ExtDoerData = (*extDoerDeployKey)(nil)

func (e *extDoerDeployKey) EncodeToString() string {
	return "deploy-key:" + strconv.FormatInt(e.DeployKeyID, 10)
}

func (e *extDoerDeployKey) DecodeFromString(s string) (err error) {
	idStr, _ := strings.CutPrefix(s, "deploy-key:")
	e.DeployKeyID, err = strconv.ParseInt(idStr, 10, 64)
	return err
}

const (
	CredentialAccessToken  = "access-token"
	CredentialOAuth2Grant  = "oauth2-grant"
	CredentialAgentSession = "agent-session" // AGENTS-HOOK: credential kind of an agent session token
)

type extDoerToken struct {
	credential string // empty for package registry tokens
	scope      auth.AccessTokenScope
	publicOnly bool

	// AGENTS-HOOK: an agent session token acts as its bot, limited to what the user who triggered the run can also do
	sessionID     int64
	triggerUserID int64
}

var _ ExtDoerData = (*extDoerToken)(nil)

func NewTokenExtDoerData(credentialKind string, credentialID int64, scope auth.AccessTokenScope) ExtDoerData {
	publicOnly, err := scope.PublicOnly()
	e := &extDoerToken{scope: scope, publicOnly: publicOnly || err != nil}
	if credentialKind != "" {
		e.credential = credentialKind + ":" + strconv.FormatInt(credentialID, 10)
	}
	return e
}

// NewSessionTokenExtDoerData is the doer data of an agent session token; the session and trigger user ids are
// part of the encoding so that git hooks and the permission cache key see them too.
func NewSessionTokenExtDoerData(sessionID, triggerUserID int64, scope auth.AccessTokenScope) ExtDoerData {
	e := NewTokenExtDoerData(CredentialAgentSession, sessionID, scope).(*extDoerToken)
	e.sessionID, e.triggerUserID = sessionID, triggerUserID
	return e
}

func (e *extDoerToken) EncodeToString() string {
	s := e.credential + "|" + string(e.scope)
	if e.sessionID != 0 {
		s += "|" + strconv.FormatInt(e.sessionID, 10) + "|" + strconv.FormatInt(e.triggerUserID, 10)
	}
	return s
}

func (e *extDoerToken) DecodeFromString(s string) error {
	parts := strings.Split(s, "|")
	if len(parts) != 2 && len(parts) != 4 {
		return errors.New("invalid token doer data")
	}
	credential, scope := parts[0], parts[1]
	publicOnly, err := auth.AccessTokenScope(scope).PublicOnly()
	if err != nil {
		return err
	}
	var sessionID, triggerUserID int64
	if len(parts) == 4 {
		if sessionID, err = strconv.ParseInt(parts[2], 10, 64); err != nil || sessionID == 0 {
			return errors.New("invalid token doer data")
		}
		if triggerUserID, err = strconv.ParseInt(parts[3], 10, 64); err != nil {
			return errors.New("invalid token doer data")
		}
	}
	e.credential, e.scope, e.publicOnly = credential, auth.AccessTokenScope(scope), publicOnly
	e.sessionID, e.triggerUserID = sessionID, triggerUserID
	return nil
}

// GetDoerSessionToken reports whether the doer is an agent session token and returns its session and triggering user.
// A trigger user id of 0 means "no human", which must never widen anything.
func GetDoerSessionToken(u *User) (sessionID, triggerUserID int64, ok bool) {
	if token := getDoerToken(u); token != nil && token.sessionID != 0 {
		return token.sessionID, token.triggerUserID, true
	}
	return 0, 0, false
}

func getDoerToken(u *User) *extDoerToken {
	if u == nil {
		return nil
	}
	token, _ := u.ExtDoerData.(*extDoerToken)
	return token
}

func GetDoerTokenScope(u *User) (auth.AccessTokenScope, bool) {
	if token := getDoerToken(u); token != nil {
		return token.scope, true
	}
	return "", false
}

func IsPublicOnlyDoer(u *User) bool {
	token := getDoerToken(u)
	return token != nil && token.publicOnly
}

func DoerCanViewPrivate(doer *User) bool {
	return doer != nil && !IsPublicOnlyDoer(doer)
}

func DoerIsSiteAdminFullAccess(doer *User) bool {
	if !DoerCanViewPrivate(doer) || !doer.IsAdmin {
		return false
	}
	scope, hasScope := GetDoerTokenScope(doer)
	return !hasScope || scope == auth.AccessTokenScopeAll
}

func DoerTokenAllowsOwner(doer, owner *User) bool {
	return !IsPublicOnlyDoer(doer) || owner.Visibility.IsPublic()
}

func GetDoerCredential(u *User) string {
	if u == nil || u.ExtDoerData == nil {
		return ""
	}
	if token := getDoerToken(u); token != nil {
		return token.credential
	}
	return u.ExtDoerData.EncodeToString()
}
