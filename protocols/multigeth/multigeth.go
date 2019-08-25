/*
	Copyright 2019 whiteblock Inc.
	This file is a part of the genesis.

	Genesis is free software: you can redistribute it and/or modify
	it under the terms of the GNU General Public License as published by
	the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	Genesis is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU General Public License for more details.

	You should have received a copy of the GNU General Public License
	along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

//Package multigeth handles multigeth specific functionality
package multigeth

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/whiteblock/genesis/db"
	"github.com/whiteblock/genesis/protocols/ethereum"
	"github.com/whiteblock/genesis/protocols/helpers"
	"github.com/whiteblock/genesis/protocols/registrar"
	"github.com/whiteblock/genesis/protocols/services"
	"github.com/whiteblock/genesis/ssh"
	"github.com/whiteblock/genesis/testnet"
	"github.com/whiteblock/genesis/util"
	"github.com/whiteblock/mustache"
	"sync"
	"time"
)

var conf = util.GetConfig()

const (
	blockchain     = "multigeth"
	peeringRetries = 10
	password       = "password"
	passwordFile   = "/geth/passwd"
	genesisFileLoc = "/geth/chain.json"
)

func init() {
	registrar.RegisterBuild(blockchain, build)
	registrar.RegisterAddNodes(blockchain, add)
	registrar.RegisterServices(blockchain, func() []services.Service { return nil })
	registrar.RegisterDefaults(blockchain, helpers.DefaultGetDefaultsFn(blockchain))
	registrar.RegisterParams(blockchain, helpers.DefaultGetParamsFn(blockchain))
}

func build(tn *testnet.TestNet) error {
	etcconf, err := newConf(tn)
	if err != nil {
		return util.LogError(err)
	}
	return util.LogError(deploy(tn, etcconf, false))
}

func add(tn *testnet.TestNet) error {
	etcconf, err := restoreConf(tn)
	if err != nil {
		return util.LogError(err)
	}
	return util.LogError(deploy(tn, etcconf, true))

}

func deploy(tn *testnet.TestNet, etcconf *ethConf, isAppend bool) error {

	validFlags := checkFlagsExist(tn)

	tn.BuildState.SetBuildSteps(8 + (5 * tn.LDD.Nodes) + (tn.LDD.Nodes * (tn.LDD.Nodes - 1)))

	tn.BuildState.IncrementBuildProgress()

	tn.BuildState.SetBuildStage("Distributing secrets")

	helpers.MkdirAllNewNodes(tn, "/geth")

	tn.BuildState.IncrementBuildProgress()

	/**Create the wallets**/
	tn.BuildState.SetBuildStage("Creating the wallets")

	accounts := ethereum.GetExistingAccounts(tn)

	if len(accounts) < tn.LDD.Nodes+int(etcconf.ExtraAccounts) {
		additionalAccounts, err := ethereum.GenerateAccounts((tn.LDD.Nodes + int(etcconf.ExtraAccounts)) - len(accounts))
		if err != nil {
			return util.LogError(err)
		}
		accounts = append(accounts, additionalAccounts...)

	}

	err := ethereum.CreateNPasswordFile(tn, len(accounts), password, passwordFile)
	if err != nil {
		return util.LogError(err)
	}

	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		for i, account := range accounts {
			_, err := client.DockerExec(node, fmt.Sprintf("bash -c 'echo \"%s\" >> /geth/pk%d'", account.HexPrivateKey(), i))
			if err != nil {
				return util.LogError(err)
			}
			_, err = client.DockerExec(node,
				fmt.Sprintf("geth --datadir /geth/ --nousb --password %s account import /geth/pk%d",
					passwordFile, i))
			if err != nil {
				return util.LogError(err)
			}
		}
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}
	tn.BuildState.Set("generatedAccs", accounts)

	tn.BuildState.IncrementBuildProgress()
	unlock := ""

	for i, account := range accounts {
		if i != 0 {
			unlock += ","
		}
		unlock += account.HexAddress()
	}

	tn.BuildState.IncrementBuildProgress()

	tn.BuildState.SetBuildStage("Creating the genesis block")
	if isAppend {

		err = createGenesisfile(etcconf, tn, ethereum.GetExistingAccounts(tn))
		if err != nil {
			return util.LogError(err)
		}
	} else {
		err = createGenesisfile(etcconf, tn, accounts)
		if err != nil {
			return util.LogError(err)
		}
	}

	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		defer tn.BuildState.IncrementBuildProgress()
		log.WithFields(log.Fields{"node": node.GetAbsoluteNumber()}).Trace("creating block directory")

		//Load the CustomGenesis file
		_, err := client.DockerExec(node,
			fmt.Sprintf("geth --datadir /geth/ --networkid %d --nousb init %s", etcconf.NetworkID, genesisFileLoc))
		return util.LogError(err)
	})
	if err != nil {
		return util.LogError(err)
	}

	tn.BuildState.IncrementBuildProgress()
	tn.BuildState.SetBuildStage("Bootstrapping network")

	staticNodes := ethereum.GetEnodes(tn, accounts)

	tn.BuildState.SetBuildStage("Starting geth")

	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		tn.BuildState.IncrementBuildProgress()

		gethCmd := fmt.Sprintf(
			`geth --datadir=/geth/ %s --rpc --nodiscover --rpcaddr=%s`+
				` --rpcapi="admin,web3,db,eth,net,personal,miner,txpool" --rpccorsdomain="*" --mine`+
				` --etherbase=%s --nousb console  2>&1 | tee %s`,
			getExtraFlags(etcconf, accounts[node.GetAbsoluteNumber()], validFlags[node.GetAbsoluteNumber()]),
			node.GetIP(),
			accounts[node.GetAbsoluteNumber()].HexAddress(),
			conf.DockerOutputFile)

		_, err := client.DockerExecdit(node, fmt.Sprintf("bash -ic '%s'", gethCmd))
		if err != nil {
			return util.LogError(err)
		}

		tn.BuildState.IncrementBuildProgress()
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}
	tn.BuildState.IncrementBuildProgress()
	if !isAppend {
		tn.BuildState.SetExt("networkID", etcconf.NetworkID)
		tn.BuildState.SetExt("port", ethereum.RPCPort)
		tn.BuildState.SetExt("namespace", "eth")
		tn.BuildState.SetExt("password", password)
		tn.BuildState.SetBuildStage("peering the nodes")
		ethereum.ExposeAccounts(tn, accounts)
	}

	time.Sleep(3 * time.Second)
	log.WithFields(log.Fields{"staticNodes": staticNodes}).Debug("peering")
	err = peerAllNodes(tn, staticNodes)
	if err != nil {
		return util.LogError(err)
	}
	return ethereum.UnlockAllAccounts(tn, accounts, password)
}

/**
 * Create the custom genesis file for Ethereum
 * @param  *etcconf etcconf     The chain configuration
 * @param  []string wallets     The wallets to be allocated a balance
 */

func createGenesisfile(etcconf *ethConf, tn *testnet.TestNet, accounts []*ethereum.Account) error {
	isAppend := len(tn.Details) > 1
	alloc := map[string]map[string]string{}
	if isAppend {
		tn.BuildState.GetP("alloc", &alloc)
	} else {
		for _, account := range accounts {
			alloc[account.HexAddress()[2:]] = map[string]string{
				"balance": etcconf.InitBalance,
			}
		}
	}

	consensusParams := map[string]interface{}{}
	switch etcconf.Consensus {
	case "clique":
		consensusParams["period"] = etcconf.BlockPeriodSeconds
		consensusParams["epoch"] = etcconf.Epoch
	case "ethash":
		consensusParams["difficulty"] = etcconf.Difficulty
	}

	genesis := map[string]interface{}{
		"networkId":          etcconf.NetworkID,
		"chainId":            etcconf.NetworkID, //etcconf.ChainID,
		"homesteadBlock":     etcconf.HomesteadBlock,
		"eip150Block":        etcconf.EIP150Block,
		"eip155Block":        etcconf.EIP155Block,
		"eip158Block":        etcconf.EIP158Block,
		"byzantiumBlock":     etcconf.ByzantiumBlock,
		"disposalBlock":      etcconf.DisposalBlock,
		"ecip1017EraRounds":  etcconf.ECIP1017EraRounds,
		"eip160Block":        etcconf.EIP160Block,
		"ecip1010PauseBlock": etcconf.ECIP1010PauseBlock,
		"ecip1010Length":     etcconf.ECIP1010Length,
		"gasLimit":           fmt.Sprintf("0x%x", etcconf.GasLimit),
		"difficulty":         fmt.Sprintf("0x0%x", etcconf.Difficulty),
		"mixHash":            etcconf.MixHash,
		"nonce":              etcconf.Nonce,
		"timestamp":          fmt.Sprintf("0x0%x", etcconf.Timestamp),
		"extraData":          etcconf.ExtraData,
	}

	switch etcconf.Consensus {
	case "clique":
		extraData := "0x0000000000000000000000000000000000000000000000000000000000000000"
		//it does not work when there are multiple signers put into this extraData field
		/*
			for i := 0; i < len(accounts) && i < tn.LDD.Nodes; i++ {
				extraData += accounts[i].HexAddress()[2:]
			}
		*/
		extraData += accounts[0].HexAddress()[2:]
		extraData += "000000000000000000000000000000000000000000000000000000000000000000" +
			"0000000000000000000000000000000000000000000000000000000000000000"
		genesis["extraData"] = extraData
	}

	genesis["alloc"] = alloc
	genesis["consensusParams"] = consensusParams
	tn.BuildState.Set("alloc", alloc)
	tn.BuildState.Set("etcconf", etcconf)

	return helpers.CreateConfigsNewNodes(tn, genesisFileLoc, func(node ssh.Node) ([]byte, error) {
		template, err := helpers.GetBlockchainConfig(blockchain, node.GetAbsoluteNumber(), "chain.json", tn.LDD)
		if err != nil {
			return nil, util.LogError(err)
		}

		data, err := mustache.Render(string(template), util.ConvertToStringMap(genesis))
		if err != nil {
			return nil, util.LogError(err)
		}
		return []byte(data), nil
	})
}

func getExtraFlags(ethconf *ethConf, account *ethereum.Account, validFlags map[string]bool) string {
	out := fmt.Sprintf("--nodekeyhex %s", account.HexPrivateKey())
	if ethconf.MaxPeers != -1 {
		out += fmt.Sprintf(" --maxpeers %d", ethconf.MaxPeers)
	}
	out += fmt.Sprintf(" --networkid %d", ethconf.NetworkID)
	out += fmt.Sprintf(" --verbosity %d", ethconf.Verbosity)
	if ethconf.Consensus == "ethash" {
		out += fmt.Sprintf(" --miner.gaslimit %d", ethconf.GasLimit)
		out += fmt.Sprintf(" --miner.gastarget %d", ethconf.GasLimit)
		out += fmt.Sprintf(" --miner.etherbase %s", account.HexAddress())
	}
	if validFlags["--allow-insecure-unlock"] {
		out += " --allow-insecure-unlock"
	}
	out += fmt.Sprintf(` --unlock="%s" --password %s`, account.HexAddress(), passwordFile)

	return out
}

func checkFlagsExist(tn *testnet.TestNet) []map[string]bool {
	flagsToCheck := []string{"--allow-insecure-unlock"}

	out := make([]map[string]bool, len(tn.Nodes))
	for i := range tn.Nodes {
		out[i] = map[string]bool{}
	}
	mux := sync.Mutex{}

	helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		for _, flag := range flagsToCheck {
			_, err := client.DockerExec(node, fmt.Sprintf("geth --help | grep -- '%s'", flag))
			mux.Lock()
			out[node.GetAbsoluteNumber()][flag] = (err == nil)
			mux.Unlock()
		}
		return nil
	})
	return out
}

func peerAllNodes(tn *testnet.TestNet, enodes []string) error {
	return helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		for i, enode := range enodes {
			if i == node.GetAbsoluteNumber() {
				continue
			}
			var err error
			for i := 0; i < peeringRetries; i++ { //give it some extra tries
				_, err = client.KeepTryRun(
					fmt.Sprintf(
						`curl -sS -X POST http://%s:8545 -H "Content-Type: application/json"  -d `+
							`'{ "method": "admin_addPeer", "params": ["%s"], "id": 1, "jsonrpc": "2.0" }'`,
						node.GetIP(), enode))
				if err == nil {
					break
				}
				time.Sleep(1 * time.Second)
			}
			tn.BuildState.IncrementBuildProgress()
			if err != nil {
				return util.LogError(err)
			}
		}
		return nil
	})
}