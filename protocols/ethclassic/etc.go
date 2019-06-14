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

//Package geth handles geth specific functionality
package ethclassic

import (
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/whiteblock/genesis/db"
	"github.com/whiteblock/genesis/protocols/ethereum"
	"github.com/whiteblock/genesis/protocols/helpers"
	"github.com/whiteblock/genesis/protocols/registrar"
	"github.com/whiteblock/genesis/ssh"
	"github.com/whiteblock/genesis/testnet"
	"github.com/whiteblock/genesis/util"
	"github.com/whiteblock/mustache"
	"regexp"
	"sync"
)

var conf *util.Config

const blockchain = "ethclassic"

func init() {
	conf = util.GetConfig()
	alias := "etc"
	registrar.RegisterBuild(blockchain, build)
	registrar.RegisterBuild(alias, build) //ethereum default to geth

	registrar.RegisterAddNodes(blockchain, add)
	registrar.RegisterAddNodes(alias, add)

	registrar.RegisterServices(blockchain, GetServices)
	registrar.RegisterServices(alias, GetServices)

	registrar.RegisterDefaults(blockchain, helpers.DefaultGetDefaultsFn(blockchain))
	registrar.RegisterDefaults(alias, helpers.DefaultGetDefaultsFn(blockchain))

	registrar.RegisterParams(blockchain, helpers.DefaultGetParamsFn(blockchain))
	registrar.RegisterParams(alias, helpers.DefaultGetParamsFn(blockchain))
}

// build builds out a fresh new ethereum test network using geth
func build(tn *testnet.TestNet) error {
	mux := sync.Mutex{}
	etcconf, err := newConf(tn.LDD.Params)
	if err != nil {
		return util.LogError(err)
	}

	tn.BuildState.SetBuildSteps(8 + (5 * tn.LDD.Nodes))

	tn.BuildState.IncrementBuildProgress()

	tn.BuildState.SetBuildStage("Distributing secrets")

	helpers.MkdirAllNodes(tn, "/geth")

	{
		/**Create the Password files**/
		var data string
		for i := 1; i <= tn.LDD.Nodes; i++ {
			data += "password\n"
		}
		/**Copy over the password file**/
		err = helpers.CopyBytesToAllNodes(tn, data, "/geth/passwd")
		if err != nil {
			return util.LogError(err)
		}
	}

	tn.BuildState.IncrementBuildProgress()

	/**Create the wallets**/
	tn.BuildState.SetBuildStage("Creating the wallets")

	accounts, err := ethereum.GenerateAccounts(tn.LDD.Nodes)
	if err != nil {
		return util.LogError(err)
	}
	err = helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		for i, account := range accounts {
			_, err := client.DockerExec(node, fmt.Sprintf("bash -c 'echo \"%s\" >> /geth/pk%d'", account.HexPrivateKey(), i))
			if err != nil {
				return util.LogError(err)
			}
			_, err = client.DockerExec(node,
				fmt.Sprintf("geth --datadir /geth/ --password /geth/passwd account import /geth/pk%d", i))
			if err != nil {
				return util.LogError(err)
			}
		}
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}

	tn.BuildState.IncrementBuildProgress()
	unlock := ""

	for i, account := range accounts {
		if i != 0 {
			unlock += ","
		}
		unlock += account.HexAddress()
	}
	extraAccounts, err := ethereum.GenerateAccounts(int(etcconf.ExtraAccounts))
	if err != nil {
		return util.LogError(err)
	}
	accounts = append(accounts, extraAccounts...)
	tn.BuildState.IncrementBuildProgress()

	tn.BuildState.SetBuildStage("Creating the genesis block")
	err = createGenesisfile(etcconf, tn, accounts)
	if err != nil {
		return util.LogError(err)
	}

	tn.BuildState.IncrementBuildProgress()
	tn.BuildState.SetBuildStage("Bootstrapping network")

	err = helpers.CopyToAllNodes(tn, "chain.json", "/geth/")
	if err != nil {
		return util.LogError(err)
	}

	staticNodes := make([]string, tn.LDD.Nodes)

	tn.BuildState.SetBuildStage("Initializing geth")

	err = helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		//Load the CustomGenesis file
		// _, err := client.DockerExec(node,
		// 	fmt.Sprintf("geth --datadir=/geth/ --network-id=%d --chain=chain.json", etcconf.NetworkID))

		// log.WithFields(log.Fields{"node": node.GetAbsoluteNumber()}).Trace("creating block directory")
		
		gethResults, err := client.DockerExec(node,
			fmt.Sprintf("bash -c 'echo -e \"admin.nodeInfo.enode\\nexit\\n\" | "+
				"geth --rpc --datadir=/geth/ --network-id=%d --chain=/geth/chain.json console'", etcconf.NetworkID))
		if err != nil {
			return util.LogError(err)
		}
		log.WithFields(log.Fields{"raw": gethResults}).Trace("grabbed raw enode info")
		enodePattern := regexp.MustCompile(`enode:\/\/[A-z|0-9]+@(\[\:\:\]|([0-9]|\.)+)\:[0-9]+`)
		enode := enodePattern.FindAllString(gethResults, 1)[0]
		log.WithFields(log.Fields{"enode": enode}).Trace("parsed the enode")
		enodeAddressPattern := regexp.MustCompile(`\[\:\:\]|([0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3})`)

		enode = enodeAddressPattern.ReplaceAllString(enode, node.GetIP())

		mux.Lock()
		staticNodes[node.GetAbsoluteNumber()] = enode
		mux.Unlock()
		

		tn.BuildState.IncrementBuildProgress()
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}

	out, err := json.Marshal(staticNodes)
	if err != nil {
		return util.LogError(err)
	}

	tn.BuildState.IncrementBuildProgress()
	tn.BuildState.SetBuildStage("Starting geth")
	//Copy static-nodes to every server
	err = helpers.CopyBytesToAllNodes(tn, string(out), "/geth/static-nodes.json")
	if err != nil {
		return util.LogError(err)
	}

	err = helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		//Load the CustomGenesis file
		mux.Lock()
		_, err := client.DockerExec(node, fmt.Sprintf("cp /geth/mainnet/keystore/* /geth/%s/keystore/", etcconf.Identity))
		if err != nil {
			return util.LogError(err)
		}
		mux.Unlock()
		log.WithFields(log.Fields{"node": node.GetAbsoluteNumber()}).Trace("adding accounts to right directory")

		tn.BuildState.IncrementBuildProgress()
		return nil
	})

	err = helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		tn.BuildState.IncrementBuildProgress()

		gethCmd := fmt.Sprintf(
		`geth --datadir=/geth/ --maxpeers=%d --network-id=%d --chain=/geth/chain.json --rpc --nodiscover --rpcaddr=%s`+
		` --rpcapi="web3,db,eth,net,personal,miner,txpool" --rpccorsdomain="0.0.0.0" --mine --unlock="%s"`+
		` --password=/geth/passwd --etherbase=%s console  2>&1 | tee %s`,
		etcconf.MaxPeers,
		etcconf.NetworkID,
		node.GetIP(),
		unlock,
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

	tn.BuildState.SetExt("networkID", etcconf.NetworkID)
	tn.BuildState.SetExt("accounts", ethereum.ExtractAddresses(accounts))
	tn.BuildState.SetExt("port", 8545)

	for _, account := range accounts {
		tn.BuildState.SetExt(account.HexAddress(), map[string]string{
			"privateKey": account.HexPrivateKey(),
			"publicKey":  account.HexPublicKey(),
		})
	}

	return nil
}

/***************************************************************************************************************************/

// Add handles adding a node to the geth testnet
// TODO
func add(tn *testnet.TestNet) error {
	return nil
}

// MakeFakeAccounts creates ethereum addresses which can be marked as funded to produce a
// larger initial state
func MakeFakeAccounts(accs int) []string {
	out := make([]string, accs)
	for i := 1; i <= accs; i++ {
		out[i-1] = fmt.Sprintf("%.40x", i)
	}
	return out
}

/**
 * Create the custom genesis file for Ethereum
 * @param  *etcconf etcconf     The chain configuration
 * @param  []string wallets     The wallets to be allocated a balance
 */

func createGenesisfile(etcconf *etcConf, tn *testnet.TestNet, accounts []*ethereum.Account) error {

	alloc := map[string]map[string]string{}
	for _, account := range accounts {
		alloc[account.HexAddress()[2:]] = map[string]string{
			"balance": etcconf.InitBalance,
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
		"identity":       etcconf.Identity,
		"name":           etcconf.Name,
		"network":        etcconf.NetworkID,
		"chainId":        etcconf.NetworkID,
		"homesteadBlock": etcconf.HomesteadBlock,
		"difficulty":     fmt.Sprintf("0x0%X", etcconf.Difficulty),
		"gasLimit":       fmt.Sprintf("0x%X", etcconf.GasLimit),
		"consensus":      etcconf.Consensus,
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

	accs := MakeFakeAccounts(int(etcconf.ExtraAccounts))

	for _, wallet := range accs {
		alloc[wallet] = map[string]string{
			"balance": etcconf.InitBalance,
		}
	}
	genesis["alloc"] = alloc
	genesis["consensusParams"] = consensusParams
	dat, err := helpers.GetBlockchainConfig("ethclassic", 0, "genesis.json", tn.LDD)
	if err != nil {
		return util.LogError(err)
	}

	data, err := mustache.Render(string(dat), util.ConvertToStringMap(genesis))
	if err != nil {
		return util.LogError(err)
	}
	return tn.BuildState.Write("chain.json", data)
}