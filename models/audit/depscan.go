// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package audit

// Dependency scanning, kept apart from action.go so the fork rebases cleanly
var (
	RepositoryDependencyScanning     = define("repository:dependencyscanning:update", "Changed the dependency scanning of repository {scope} to {enabled}.")
	RepositoryDependencyAlertDismiss = define("repository:dependencyalert:dismiss", "Dismissed the alert {alert} on {package} in repository {scope}.")
	RepositoryDependencyAlertReopen  = define("repository:dependencyalert:reopen", "Reopened the alert {alert} on {package} in repository {scope}.")

	RepositoryDependencyUpdates         = define("repository:dependencyupdates:update", "Changed the dependency updates of repository {scope}, run by agent {agent}.")
	RepositoryDependencyUpdatesTakeOver = define("repository:dependencyupdates:takeover", "Took over the dependency updates of repository {scope} from user {previous_updater}.")
)
