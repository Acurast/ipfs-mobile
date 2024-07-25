package ffi

import (
	"C"
	"time"
	"unsafe"

	"ipfs-mobile/client"
	"ipfs-mobile/utils"
)

//export Get
func Get(
	cid 		   *C.char,
	boot_peers 	   **C.char,
	boot_peers_len C.int,
	plugins 	   *C.char,
	repo 		   *C.char,
	output 		   *C.char,
	timeout		   C.long,
) {
	nodeConfig := &client.NodeConfig{
		BootstrapPeers: utils.GoStringSlice(unsafe.Pointer(boot_peers), int32(boot_peers_len)),
		Plugins: 		C.GoString(plugins),
		Repo:    		C.GoString(repo),
	}

	
	var optTimeout *time.Duration = nil
	if (timeout >= 0) {
		timeout := time.Duration(int64(timeout)) * time.Millisecond
		optTimeout = &timeout
	}

	execConfig := &client.ExecConfig{
		Timeout: optTimeout,
	}

	err := client.Get(C.GoString(cid), C.GoString(output), nodeConfig, execConfig)
	if err != nil {
		panic(err)
	}
}
