// Package bootstrap holds the cluster-level SQL that runs before goose:
// the group roles of ADR-0013. See roles.sql.
package bootstrap

import _ "embed" // go:embed

// RolesSQL creates the koala_owner, koala_app and koala_maint group roles
// if they are missing. It must run as a role with CREATEROLE.
//
//go:embed roles.sql
var RolesSQL string
