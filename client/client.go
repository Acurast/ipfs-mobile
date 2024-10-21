package client

import (
	"context"
	"time"

	"ipfs-mobile/utils"
)

type ExecConfig struct {
	SizeLimit int64
	Timeout   *time.Duration
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

		return node.Download(ctx, cid, output, execConfig.SizeLimit)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *execConfig.Timeout)
	defer cancel()

	result := make(chan error, 1)

	go func() {
		result <- node.Download(ctx, cid, output, execConfig.SizeLimit)
	}()

	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return utils.Timeout()
	}
}
