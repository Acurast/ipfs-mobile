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
	ctx := context.Background()
	if execConfig.Timeout != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *execConfig.Timeout)
		defer cancel()
	}

	result := make(chan error, 1)

	go func() {
		node, err := GetNode(nodeConfig)
		if err != nil {
			result <- err
			return
		}
		defer node.Close()

		result <- node.Download(ctx, cid, output, execConfig.SizeLimit)
	}()

	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return utils.Timeout()
	}
}
