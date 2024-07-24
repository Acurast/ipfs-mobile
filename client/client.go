package client

import (
	"context"
	"fmt"
)

func Get(cid string, output string, nodeConfig *NodeConfig) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, err := StartNode(ctx, nodeConfig)
	if err != nil {
		return err
	}

	go func() {
		err := node.ConnectToPeers(ctx, nodeConfig.BootstrapPeers)
		if err != nil {
			fmt.Printf("failed to connect to peers: %s\n", err)
		}
	}()

	return node.Download(ctx, cid, output)
}
