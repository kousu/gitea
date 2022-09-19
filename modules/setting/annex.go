// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"code.gitea.io/gitea/modules/log"
)

// Annex represents the configuration for git-annex
var Annex = struct {
	Enabled bool `ini:"ENABLED"`
}{}

func loadAnnexFrom(rootCfg ConfigProvider) {
	sec := rootCfg.Section("annex")
	if err := sec.MapTo(&Annex); err != nil {
		log.Fatal("Failed to map Annex settings: %v", err)
	}
}
