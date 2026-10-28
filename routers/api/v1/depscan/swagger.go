// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	api "gitea.dev/modules/structs"
)

// DependencyAlert
// swagger:response DependencyAlert
type swaggerDependencyAlert struct {
	// in:body
	Body api.DependencyAlert `json:"body"`
}

// DependencyAlertList
// swagger:response DependencyAlertList
type swaggerDependencyAlertList struct {
	// in:body
	Body []api.DependencyAlert `json:"body"`
}

// DependencyList
// swagger:response DependencyList
type swaggerDependencyList struct {
	// in:body
	Body []api.Dependency `json:"body"`
}

// DependencyUpdates
// swagger:response DependencyUpdates
type swaggerDependencyUpdates struct {
	// in:body
	Body api.DependencyUpdates `json:"body"`
}

// DependencyUpdatesRun
// swagger:response DependencyUpdatesRun
type swaggerDependencyUpdatesRun struct {
	// in:body
	Body api.DependencyUpdatesRun `json:"body"`
}

// swagger:parameters repoEditDependencyAlert
type swaggerEditDependencyAlertBody struct {
	// in:body
	Body api.EditDependencyAlertOption
}

// swagger:parameters repoEditDependencyUpdates
type swaggerEditDependencyUpdatesBody struct {
	// in:body
	Body api.DependencyUpdatesOption
}
