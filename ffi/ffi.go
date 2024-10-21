package ffi

import (
	"time"

	"ipfs-mobile/client"
	"ipfs-mobile/utils"
)

type Config struct {
	BootstrapPeers string
	Port           int32
	SizeLimit      int64
	Timeout        int64
}

func Get(cid string, output string, config *Config) error {
	nodeConfig := &client.NodeConfig{
		BootstrapPeers: utils.GetStringSlice(config.BootstrapPeers),
		Port:           config.Port,
	}

	var optTimeout *time.Duration = nil
	if (config.Timeout >= 0) {
		timeout := time.Duration(config.Timeout) * time.Millisecond
		optTimeout = &timeout
	}

	execConfig := &client.ExecConfig{
		SizeLimit: config.SizeLimit,
		Timeout:   optTimeout,
	}

	return client.Get(cid, output, nodeConfig, execConfig)
}
