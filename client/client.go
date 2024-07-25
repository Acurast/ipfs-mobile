package client

import (
	"context"
	"fmt"
	"time"
)

type ExecConfig struct {
	Timeout *time.Duration
}

func Get(cid string, output string, nodeConfig *NodeConfig, execConfig *ExecConfig) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if execConfig.Timeout == nil {
		return get(ctx, cid, output, nodeConfig)
	}

	result := make(chan error, 1)

	go func() {
		result <- get(ctx, cid, output, nodeConfig)
	}()

	select {
	case err := <-result:
		return err
	case <-time.After(*execConfig.Timeout):
		return fmt.Errorf("timeout")
	}
}

func get(ctx context.Context, cid string, output string, nodeConfig *NodeConfig) error {
	node, err := GetNode(ctx, nodeConfig)
	if err != nil {
		return err
	}
	defer node.Close()

	go func() {
		err := node.ConnectToPeers(ctx, nodeConfig.BootstrapPeers)
		if err != nil {
			fmt.Printf("failed to connect to peers: %s\n", err)
		}
	}()

	return node.Download(ctx, cid, output)
}
