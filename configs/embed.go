// Package configs embeds the engine's configuration templates so services
// built into containers need no mounted config directory.
package configs

import _ "embed"

// CodexConfig is configs/codex-config.yaml. Only its zones block is loaded
// (internal/zones.DefaultTemplates); see the file's header.
//
//go:embed codex-config.yaml
var CodexConfig []byte
