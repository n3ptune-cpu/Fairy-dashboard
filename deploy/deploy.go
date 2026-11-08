// Package deploy holds the files `fairy update` installs on the panel host for the Updates page's
// "Update panel" button: the root oneshot unit and the polkit rule that lets the panel start it.
package deploy

import _ "embed"

// PanelUpdateUnit is fairy-panel-update.service.
//
//go:embed fairy-panel-update.service
var PanelUpdateUnit []byte

// PanelPolkitRules is 49-fairy-panel.rules.
//
//go:embed fairy-panel.polkit.rules
var PanelPolkitRules []byte
