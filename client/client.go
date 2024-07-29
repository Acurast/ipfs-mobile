package client

import (
	"context"
	"time"

	"ipfs-mobile/utils"
)

type ExecConfig struct {
	Timeout *time.Duration
}

func Get(cid string, output string, nodeConfig *NodeConfig, execConfig *ExecConfig) error {
	node, err := GetNode(nodeConfig)
	if err != nil {
		return err
	}
	defer node.Close()

	if execConfig.Timeout == nil {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		return node.Download(ctx, cid, output)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *execConfig.Timeout)
	defer cancel()

	result := make(chan error, 1)

	go func() {
		result <- node.Download(ctx, cid, output)
	}()

	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return utils.Timeout()
	}
}
