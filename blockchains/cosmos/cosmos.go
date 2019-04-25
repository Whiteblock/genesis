package cosmos

import (
	db "../../db"
	ssh "../../ssh"
	testnet "../../testnet"
	util "../../util"
	helpers "../helpers"
	"fmt"
	"log"
	"strings"
	"sync"
)

var conf *util.Config

func init() {
	conf = util.GetConfig()
}

func Build(tn *testnet.TestNet) ([]string, error) {
	buildState := tn.BuildState

	buildState.SetBuildSteps(4 + (tn.LDD.Nodes * 2))

	buildState.SetBuildStage("Setting up the first node")
	clients := tn.GetFlatClients()
	/**
	 * Set up first node
	 */
	_, err := clients[0].DockerExec(0, "gaiad init --chain-id=whiteblock whiteblock")
	if err != nil {
		log.Println(err)
		return nil, err
	}
	buildState.IncrementBuildProgress()
	_, err = clients[0].DockerExec(0, "bash -c 'echo \"password\\n\" | gaiacli keys add validator -ojson'")
	if err != nil {
		log.Println(err)
		return nil, err
	}

	res, err := clients[0].DockerExec(0, "gaiacli keys show validator -a")
	if err != nil {
		log.Println(err)
		return nil, err
	}
	buildState.IncrementBuildProgress()
	_, err = clients[0].DockerExec(0, fmt.Sprintf("gaiad add-genesis-account %s 100000000stake,100000000validatortoken", res[:len(res)-1]))
	if err != nil {
		log.Println(err)
		return nil, err
	}

	_, err = clients[0].DockerExec(0, "bash -c 'echo \"password\\n\" | gaiad gentx --name validator'")
	if err != nil {
		log.Println(err)
		return nil, err
	}
	buildState.IncrementBuildProgress()
	_, err = clients[0].DockerExec(0, "gaiad collect-gentxs")
	if err != nil {
		log.Println(err)
		return nil, err
	}
	genesisFile, err := clients[0].DockerExec(0, "cat /root/.gaiad/config/genesis.json")
	if err != nil {
		log.Println(err)
		return nil, err
	}
	buildState.IncrementBuildProgress()
	buildState.SetBuildStage("Initializing the rest of the nodes")
	peers := make([]string, tn.LDD.Nodes)
	mux := sync.Mutex{}

	err = helpers.AllNodeExecCon(tn, func(client *ssh.Client, server *db.Server, localNodeNum int, absoluteNodeNum int) error {
		ip := tn.Nodes[absoluteNodeNum].Ip
		if absoluteNodeNum != 0 {
			//init everything
			_, err := client.DockerExec(localNodeNum, "gaiad init --chain-id=whiteblock whiteblock")
			if err != nil {
				log.Println(res)
				return err
			}
		}

		//Get the node id
		res, err := client.DockerExec(localNodeNum, "gaiad tendermint show-node-id")
		if err != nil {
			log.Println(err)
			return err
		}
		nodeId := res[:len(res)-1]
		mux.Lock()
		peers[absoluteNodeNum] = fmt.Sprintf("%s@%s:26656", nodeId, ip)
		mux.Unlock()
		buildState.IncrementBuildProgress()
		return nil
	})

	buildState.SetBuildStage("Copying the genesis file to each node")

	err = helpers.CopyBytesToAllNodes(tn, genesisFile, "/root/.gaiad/config/genesis.json")
	if err != nil {
		log.Println(err)
		return nil, err
	}

	buildState.SetBuildStage("Starting cosmos")

	err = helpers.AllNodeExecCon(tn, func(client *ssh.Client, server *db.Server, localNodeNum int, absoluteNodeNum int) error {
		defer buildState.IncrementBuildProgress()
		peersCpy := make([]string, len(peers))
		copy(peersCpy, peers)
		_, err := client.DockerExecd(localNodeNum, fmt.Sprintf("gaiad start --p2p.persistent_peers=%s",
			strings.Join(append(peersCpy[:absoluteNodeNum], peersCpy[absoluteNodeNum+1:]...), ",")))
		return err
	})
	return nil, err
}

func Add(tn *testnet.TestNet) ([]string, error) {
	return nil, nil
}
