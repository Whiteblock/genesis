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
package multigeth

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"regexp"
	log "github.com/sirupsen/logrus"
	"github.com/whiteblock/genesis/db"
	"github.com/whiteblock/genesis/protocols/ethereum"
	"github.com/whiteblock/genesis/protocols/parity"
	"github.com/whiteblock/genesis/protocols/helpers"
	"github.com/whiteblock/genesis/protocols/registrar"
	"github.com/whiteblock/genesis/ssh"
	"github.com/whiteblock/genesis/testnet"
	"github.com/whiteblock/genesis/util"
	"github.com/whiteblock/mustache"
	"sync"
)

var conf *util.Config

const (
	blockchain      = "multigeth"
	password        = "password"
	defaultMode     = "default"
	expansionMode   = "expand"
	p2pPort         = 30303
	rpcPort         = 8545
	genesisFileName = "CustomGenesis.json"
)

func init() {
	conf = util.GetConfig()
	registrar.RegisterBuild(blockchain, build)
	registrar.RegisterAddNodes(blockchain, add)
	registrar.RegisterServices(blockchain, GetServices)
	registrar.RegisterDefaults(blockchain, helpers.DefaultGetDefaultsFn(blockchain))
	registrar.RegisterParams(blockchain, helpers.DefaultGetParamsFn(blockchain))
}

// build builds out a fresh new ethereum test network using geth
func build(tn *testnet.TestNet) error {
	ethconf, err := newConf(tn.LDD.Params)
	if err != nil {
		return util.LogError(err)
	}

	err = loadForExpand(tn, ethconf) //Prepare everything if it is large state
	if err != nil {
		return util.LogError(err)
	}

	validFlags := checkFlagsExist(tn)
	tn.BuildState.SetBuildSteps(8 + (5 * tn.LDD.Nodes))
	tn.BuildState.IncrementBuildProgress()
	tn.BuildState.SetBuildStage("Distributing secrets")
	helpers.MkdirAllNodes(tn, "/multi-geth")

	{
		/**Create the Password files**/
		var data string
		for i := 1; i <= tn.LDD.Nodes; i++ {
			data += password + "\n"
		}
		/**Copy over the password file**/
		err = helpers.CopyBytesToAllNodes(tn, data, "/multi-geth/passwd")
		if err != nil {
			return util.LogError(err)
		}
	}
	tn.BuildState.IncrementBuildProgress()

	/* get the proper directory for specified network */
	var network string
	switch ethconf.Network {
	case "classic":
		network = "--classic"
	}

	/**Create the wallets**/
	wallets := make([]string, tn.LDD.Nodes)
	tn.BuildState.SetBuildStage("Creating the wallets")

	accounts, err := getAccountPool(tn, int(ethconf.ExtraAccounts)+tn.LDD.Nodes)
	if err != nil {
		return util.LogError(err)
	}
	err = helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		for i, account := range accounts[:tn.LDD.Nodes] {
			_, err := client.DockerExec(node, fmt.Sprintf("bash -c 'echo \"%s\" > /multi-geth/pk%d'", account.HexPrivateKey(), i))
			if err != nil {
				return util.LogError(err)
			}
			walletOut, err := client.DockerExec(node,
				fmt.Sprintf("geth %s --datadir /multi-geth/ --password /multi-geth/passwd account import /multi-geth/pk%d", network, i))
			if err != nil {
				util.LogError(err) //dont report the error
			}
			reAddr := regexp.MustCompile(`(?m)Address: {(.{41})`)
			regAddr := reAddr.FindAllString(walletOut, 1)[0]
			fmt.Println(regAddr)
			addr := strings.Replace(regAddr, "Address: {", "", -1)
			addr = strings.Replace(addr, "}", "", -1)
			wallets = append(wallets, addr)
		}
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}

	tn.BuildState.IncrementBuildProgress()

	err = handleGenesisFileDist(tn, ethconf, network, accounts)
	if err != nil {
		return util.LogError(err)
	}

	staticNodes := getEnodes(tn, accounts)

	tn.BuildState.SetBuildStage("Initializing geth")

	out, err := json.Marshal(staticNodes)
	if err != nil {
		return util.LogError(err)
	}

	tn.BuildState.IncrementBuildProgress()
	tn.BuildState.SetBuildStage("Starting multi-geth")
	//Copy static-nodes to every server
	err = helpers.CopyBytesToAllNodes(tn, string(out), "/multi-geth/static-nodes.json")
	if err != nil {
		return util.LogError(err)
	}

	err = helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		tn.BuildState.IncrementBuildProgress()
		account := accounts[node.GetAbsoluteNumber()]
		gethCmd := fmt.Sprintf(
			`geth %s --datadir /multi-geth/ %s --rpc --nodiscover --rpcaddr 0.0.0.0`+
				` --rpcapi "admin,web3,db,eth,net,personal,miner,txpool" --rpccorsdomain "0.0.0.0" --mine`+
				` --txpool.nolocals --port %d console  2>&1 | tee %s`,
			network,
			getExtraFlags(ethconf, account, validFlags[node.GetAbsoluteNumber()]), p2pPort,
			conf.DockerOutputFile)

		_, err := client.DockerExecdit(node, fmt.Sprintf("bash -ic '%s'", gethCmd))
		tn.BuildState.IncrementBuildProgress()
		return util.LogError(err)
	})
	if err != nil {
		return util.LogError(err)
	}
	tn.BuildState.IncrementBuildProgress()

	tn.BuildState.SetExt("networkID", ethconf.NetworkID)
	tn.BuildState.SetExt("accounts", ethereum.ExtractAddresses(accounts))
	tn.BuildState.SetExt("password", password)
	tn.BuildState.SetExt("port", rpcPort)
	tn.BuildState.SetExt("namespace", "eth")

	for _, account := range accounts {
		tn.BuildState.SetExt(account.HexAddress(), map[string]string{
			"privateKey": account.HexPrivateKey(),
			"publicKey":  account.HexPublicKey(),
		})
	}

	StoreParameters(tn, ethconf, wallets, staticNodes)

	return nil
}


/***************************************************************************************************************************/

// Add handles adding a node to the geth testnet
// TODO
func add(tn *testnet.TestNet) error {

	//parity attachment
	mux := sync.Mutex{}

	tn.BuildState.IncrementBuildProgress()

	tn.BuildState.SetBuildStage("Pulling the genesis block")

	var mgethConf MgethConf
	tn.BuildState.GetP("multigethConf", &mgethConf)

	var genesisAlloc map[string]map[string]string
	tn.BuildState.GetP("alloc", &genesisAlloc)

	parityConf, err := parity.NewParityConf(tn.LDD.Params)
	tn.BuildState.SetBuildSteps(1 + 2*len(tn.NewlyBuiltNodes)) //TODO
	if err != nil {
		return util.LogError(err)
	}

	parityConf.NetworkID = mgethConf.NetworkID
	parityConf.ChainID = mgethConf.NetworkID
	parityConf.Difficulty = mgethConf.Difficulty
	parityConf.ExtraData = mgethConf.ExtraData
	parityConf.GasLimit = mgethConf.GasLimit

	helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		_, err := client.DockerExec(node, fmt.Sprintf("mkdir -p /parity"))
		return err
	})
	if err != nil {
		return util.LogError(err)
	}

	/**Create the Password file and copy it over**/
	{
		var data string
		for i := 1; i <= tn.LDD.Nodes; i++ {
			data += "password\n"
		}
		tn.BuildState.IncrementBuildProgress()
		err = helpers.CopyBytesToAllNewNodes(tn, data, "/parity/passwd")
		if err != nil {
			return util.LogError(err)
		}
		tn.BuildState.IncrementBuildProgress()
	}

	genWallets := []string{}
	wallets := []string{}
	rawWallets := []string{}
	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		res, err := client.DockerExec(node, "parity --base-path=/parity/ --password=/parity/passwd account new")
		if err != nil {
			return util.LogError(err)
		}

		if len(res) == 0 {
			return fmt.Errorf("account new returned an empty response")
		}

		mux.Lock()
		wallets = append(wallets, res[:len(res)-1])
		mux.Unlock()

		res, err = client.DockerExec(node, "bash -c 'cat /parity/keys/ethereum/*'")
		if err != nil {
			return util.LogError(err)
		}
		tn.BuildState.IncrementBuildProgress()

		mux.Lock()
		rawWallets = append(rawWallets, strings.Replace(res, "\"", "\\\"", -1))
		mux.Unlock()
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}

	helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		for i := 0; i < node.GetAbsoluteNumber(); i++ {
			var nodeKeyStores string
			tn.BuildState.GetP(fmt.Sprintf("node%dKey", i), &nodeKeyStores)
			_, err := client.DockerExec(node, fmt.Sprintf("bash -c 'echo \"%s\" >> /parity/account%d'", nodeKeyStores, i+1))
			if err != nil {
				return err
			}
			rawWallets = append(rawWallets, nodeKeyStores)
		}
		return err
	})
	if err != nil {
		return util.LogError(err)
	}

	for i := range genesisAlloc {
		wallets = append(wallets, "0x"+i)
		genWallets = append(genWallets, "0x"+i)
	}

	// ***********************************************************************************************************

	switch mgethConf.Consensus {
	case "ethash":
		err = parity.SetupNewPOW(tn, parityConf, wallets, genWallets)
	case "clique":
		err = parity.SetupNewPOA(tn, parityConf, wallets, genWallets)
	default:
		return util.LogError(fmt.Errorf("Unknown consensus %s", parityConf.Consensus))
	}
	if err != nil {
		return util.LogError(err)
	}

	// ***********************************************************************************************************

	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		for i, rawWallet := range rawWallets {
			_, err := client.DockerExec(node, fmt.Sprintf("bash -c 'echo \"%s\">/parity/account%d'", rawWallet, i))
			if err != nil {
				return util.LogError(err)
			}

			_, err = client.DockerExec(node,
				fmt.Sprintf("parity --base-path=/parity/ --chain /parity/spec.json --password=/parity/passwd account import /parity/account%d", i))
			if err != nil {
				return util.LogError(err)
			}
		}
		tn.BuildState.IncrementBuildProgress()
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}

	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		defer tn.BuildState.IncrementBuildProgress()
		return client.DockerRunMainDaemon(node,
			fmt.Sprintf(`parity --author=%s -c /parity/config.toml --chain=/parity/spec.json`, wallets[node.GetAbsoluteNumber()%tn.LDD.Nodes]))
	})
	if err != nil {
		return util.LogError(err)
	}

	var snodes []string
	tn.BuildState.GetP("staticNodes", &snodes)
	fmt.Println(fmt.Sprintf("enode address : %+v", snodes))
	if err != nil {
		return util.LogError(err)
	}

	//Start peering via curl
	time.Sleep(time.Duration(5 * time.Second))
	//Get the enode addresses
	enodes := make([]string, tn.LDD.Nodes)
	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, server *db.Server, node ssh.Node) error {
		enode := ""
		for len(enode) == 0 {
			res, err := client.KeepTryRun(
				fmt.Sprintf(
					`curl -sS -X POST http://%s:8545 -H "Content-Type: application/json" `+
						` -d '{ "method": "parity_enode", "params": [], "id": 1, "jsonrpc": "2.0" }'`,
					node.GetIP()))

			if err != nil {
				return util.LogError(err)
			}
			var result map[string]interface{}

			err = json.Unmarshal([]byte(res), &result)
			if err != nil {
				return util.LogError(err)
			}
			log.WithFields(log.Fields{"result": result}).Trace("fetched enode addr from parity_enode")

			err = util.GetJSONString(result, "result", &enode)
			if err != nil {
				return util.LogError(err)
			}
		}
		tn.BuildState.IncrementBuildProgress()
		mux.Lock()
		enodes[node.GetAbsoluteNumber()%tn.LDD.Nodes] = enode
		mux.Unlock()
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}
	parity.StoreParameters(tn, parityConf, wallets, enodes)

	tn.BuildState.IncrementBuildProgress()
	tn.BuildState.SetBuildStage("Bootstrapping network")

	return parity.PeerAllNodes(tn, snodes)

	return nil
}

// MakeFakeAccounts creates ethereum addresses which can be marked as funded to produce a
// larger initial state
func MakeFakeAccounts(accs int) []string {
	out := make([]string, accs)
	for i := 1; i <= accs; i++ {
		out[i-1] = fmt.Sprintf("0x%.40x", i)
	}
	return out
}

/**
 * Create the custom genesis file for Ethereum
 * @param  *ethConf ethconf     The chain configuration
 * @param  []string wallets     The wallets to be allocated a balance
 */

func createGenesisfile(ethconf *MgethConf, tn *testnet.TestNet, accounts []*ethereum.Account) (string, error) {

	alloc := map[string]map[string]string{}
	for _, account := range accounts {
		alloc[account.HexAddress()] = map[string]string{
			"balance": ethconf.InitBalance,
		}
	}

	consensusParams := map[string]interface{}{}
	switch ethconf.Consensus {
	case "clique":
		consensusParams["period"] = ethconf.BlockPeriodSeconds
		consensusParams["epoch"] = ethconf.Epoch
	case "ethash":
		consensusParams["difficulty"] = ethconf.Difficulty
	}

	genesis := map[string]interface{}{
		"networkId":           ethconf.NetworkID,
		"chainId":             ethconf.NetworkID,
		"homesteadBlock":      checkIntToNull(ethconf.HomesteadBlock),
		"eip7FBlock":          checkIntToNull(ethconf.EIP7FBlock),
		"eip150Block":         checkIntToNull(ethconf.EIP150Block),
		"eip155Block":         checkIntToNull(ethconf.EIP155Block),
		"eip158Block":         checkIntToNull(ethconf.EIP158Block),
		"byzantiumBlock":      checkIntToNull(ethconf.ByzantiumBlock),
		"disposalBlock":       checkIntToNull(ethconf.DisposalBlock),
		"constantinopleBlock": checkIntToNull(ethconf.ConstantinopleBlock),
		"ecip1017EraRounds":   checkIntToNull(ethconf.ECIP1017EraRounds),
		"eip160FBlock":        checkIntToNull(ethconf.EIP160FBlock),
		"consensus":           ethconf.Consensus,
		"gasLimit":            fmt.Sprintf("0x0%X", ethconf.GasLimit),
		"difficulty":          fmt.Sprintf("0x0%X", ethconf.Difficulty),
	}

	switch ethconf.Consensus {
	case "clique":
		fallthrough
	case "ethash":
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

	dat, err := helpers.GetGlobalBlockchainConfig(tn, "genesis.json")
	if err != nil {
		return "", util.LogError(err)
	}

	data, err := mustache.Render(string(dat), util.ConvertToStringMap(genesis))
	if err != nil {
		return "", util.LogError(err)
	}
	return data, nil
}

func handleGenesisFileDist(tn *testnet.TestNet, ethconf *MgethConf, network string, accounts []*ethereum.Account) error {
	tn.BuildState.IncrementBuildProgress()
	tn.BuildState.SetBuildStage("Creating the genesis block")

	genesisData, err := createGenesisfile(ethconf, tn, accounts)
	if err != nil {
		return util.LogError(err)
	}

	genesisFileLoc := fmt.Sprintf("/multi-geth/%s", genesisFileName)

	tn.BuildState.IncrementBuildProgress()
	tn.BuildState.SetBuildStage("Bootstrapping network")
	hasGenesis := make([]bool, tn.LDD.Nodes)

	if ethconf.Mode != expansionMode {
		err := helpers.CopyBytesToAllNewNodes(tn, genesisData, genesisFileLoc)
		if err != nil {
			return util.LogError(err)
		}
	} else {
		//if it is expansion mode, do not create it if it does not exist

		mux := sync.Mutex{}

		helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
			_, err := client.DockerExec(node, fmt.Sprintf("test -f %s", genesisFileLoc))
			mux.Lock()
			hasGenesis[node.GetAbsoluteNumber()] = (err == nil)
			mux.Unlock()
			return nil
		})

		err := helpers.CreateConfigs(tn, genesisFileLoc, func(node ssh.Node) ([]byte, error) {
			if hasGenesis[node.GetAbsoluteNumber()] {
				log.WithFields(log.Fields{"node": node.GetAbsoluteNumber()}).Debug("node already has a genesis file")
				return nil, nil
			}
			return []byte(genesisData), nil
		})
		if err != nil {
			return util.LogError(err)
		}
	}
	return helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		//Load the CustomGenesis file
		if ethconf.Mode != expansionMode || !hasGenesis[node.GetAbsoluteNumber()] {
			_, err := client.DockerExec(node,
				fmt.Sprintf("geth %s --datadir /multi-geth/ --networkid %d init %s", network, ethconf.NetworkID, genesisFileLoc))
			if err != nil {
				return util.LogError(err)
			}
		}
		log.WithFields(log.Fields{"node": node.GetAbsoluteNumber()}).Trace("creating block directory")
		tn.BuildState.IncrementBuildProgress()
		return nil
	})
}

func loadForExpand(tn *testnet.TestNet, ethconf *MgethConf) error {
	if ethconf.Mode != expansionMode {
		return nil
	}
	masterNode := tn.Nodes[0]
	masterClient := tn.Clients[masterNode.Server]
	files := []string{"/important_data", "/important_info"}
	var data string
	var err error
	for _, file := range files {
		data, err = masterClient.DockerRead(masterNode, file, -1)
		if err == nil {
			break
		}

	}
	if err != nil {
		return util.LogError(err)
	}

	var clientData map[string]interface{}
	err = json.Unmarshal([]byte(data), &clientData)
	if err != nil {
		return util.LogError(err)
	}
	for key, value := range clientData {
		if key == "blockchain" || key == "blockchain_prefix" {
			continue
		}
		tn.BuildState.Set(key, value)
	}
	return nil
}

func getAccountPool(tn *testnet.TestNet, numOfAccounts int) ([]*ethereum.Account, error) {
	accounts := []*ethereum.Account{}
	rawPreGen, err := helpers.FetchPreGeneratedPrivateKeys(tn)
	if err != nil {
		log.Debug("There are not any pregenerated accounts availible")
	} else {
		accounts, err = ethereum.ImportAccounts(rawPreGen)
		if err != nil {
			return nil, util.LogError(err)
		}
	}
	if len(accounts) >= numOfAccounts {
		return accounts, nil
	}
	var tmp []string
	tn.BuildState.GetP("accounts", &tmp)
	for _, addr := range tmp {
		var accountData map[string]string
		ok := tn.BuildState.GetP(addr, &accountData)
		if !ok {
			log.WithFields(log.Fields{"address": addr}).Trace("skipping address without entry")
			continue
		}
		acc, err := ethereum.CreateAccountFromHex(accountData["privateKey"])
		if err != nil {
			log.WithFields(log.Fields{"error": err}).Info("there was an error with the given private key")
		} else {
			accounts = append(accounts, acc)
		}
	}
	if len(accounts) >= numOfAccounts {
		log.Info("Fetched all the accounts from the build state store")
		return accounts, nil
	}
	fillerAccounts, err := ethereum.GenerateAccounts(numOfAccounts - len(accounts))
	if err != nil {
		return nil, util.LogError(err)
	}
	return append(accounts, fillerAccounts...), nil
}

func getExtraFlags(ethconf *MgethConf, account *ethereum.Account, validFlags map[string]bool) string {
	out := fmt.Sprintf("--maxpeers %d --nodekeyhex %s",
		ethconf.MaxPeers, account.HexPrivateKey())
	out += fmt.Sprintf(" --verbosity %d", ethconf.Verbosity)

	if ethconf.Consensus == "ethash" {
		out += fmt.Sprintf(" --miner.etherbase %s", account.HexAddress())
	}

	if ethconf.Mode == expansionMode {
		out += " --syncmode full"
	}

	if ethconf.Unlock {
		out += fmt.Sprintf(` --unlock="%s" --password /multi-geth/passwd`, account.HexAddress())
		if validFlags["--allow-insecure-unlock"] {
			out += " --allow-insecure-unlock"
		}
	}

	return out
}

func checkFlagsExist(tn *testnet.TestNet) []map[string]bool {
	flagsToCheck := []string{"--allow-insecure-unlock"}

	out := make([]map[string]bool, tn.LDD.Nodes)
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

func getEnodes(tn *testnet.TestNet, accounts []*ethereum.Account) []string {
	enodes := []string{}
	for i, node := range tn.Nodes {
		enodeAddress := fmt.Sprintf("enode://%s@%s:%d",
			accounts[i].HexPublicKey(),
			node.IP,
			p2pPort)

		enodes = append(enodes, enodeAddress)
	}
	return enodes
}

func checkIntToNull(v int64) interface{} {
	if v < 0 {
		return nil
	} else {
		return v
	}
}

// StoreParameters stores the parameters to be used for reference regarding build details
func StoreParameters(tn *testnet.TestNet, mgeth *MgethConf, wallets []string, enodes []string) {
	accounts, err := ethereum.GenerateAccounts(tn.LDD.Nodes)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Warn("couldn't create geth accounts")
	}

	tn.BuildState.Set("networkID", mgeth.NetworkID)
	tn.BuildState.Set("accounts", accounts)

	tn.BuildState.SetExt("networkID", mgeth.NetworkID)
	tn.BuildState.SetExt("accounts", ethereum.ExtractAddresses(accounts))
	tn.BuildState.SetExt("port", 8545)

	for _, account := range accounts {
		tn.BuildState.SetExt(account.HexAddress(), map[string]string{
			"privateKey": account.HexPrivateKey(),
			"publicKey":  account.HexPublicKey(),
		})
	}

	switch mgeth.Consensus {
	case "ethash":
		tn.BuildState.Set("mine", true)
	case "poa":
		tn.BuildState.Set("mine", false)
	}

	tn.BuildState.Set("peers", enodes)

	tn.BuildState.Set("mgethConf", map[string]interface{}{
		"networkID":   mgeth.NetworkID,
		"initBalance": mgeth.InitBalance,
		"difficulty":  fmt.Sprintf("0x%x", mgeth.Difficulty),
		"gasLimit":    fmt.Sprintf("0x%x", mgeth.GasLimit),
		"extraData":   mgeth.ExtraData,
		"consensus":   mgeth.Consensus,
		"consensusParams": map[string]interface{}{
			"difficulty": mgeth.Difficulty,
		}})

	tn.BuildState.Set("wallets", wallets)
}