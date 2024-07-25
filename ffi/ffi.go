package ffi

import (
	"time"

	"ipfs-mobile/client"
	"ipfs-mobile/utils"
)

type Config struct {
	BootstrapPeers string
	Plugins 	   string
	Repo 		   string
	TimeoutMs	   int64
}

func Get(cid string, output string, config *Config) error {
	nodeConfig := &client.NodeConfig{
		BootstrapPeers: utils.GetStringSlice(config.BootstrapPeers),
		Plugins: 		config.Plugins,
		Repo:    		config.Repo,
	}

	var optTimeout *time.Duration = nil
	if (config.TimeoutMs >= 0) {
		timeout := time.Duration(config.TimeoutMs) * time.Millisecond
		optTimeout = &timeout
	}

	execConfig := &client.ExecConfig{
		Timeout: optTimeout,
	}

	return client.Get(cid, output, nodeConfig, execConfig)
}
