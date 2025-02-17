package operations

import (
	"context"
	"math/big"
	"os"
	"os/exec"
	"time"

	"github.com/0xPolygonHermez/zkevm-bridge-service/bridgectrl"
	"github.com/0xPolygonHermez/zkevm-bridge-service/bridgectrl/pb"
	"github.com/0xPolygonHermez/zkevm-bridge-service/db"
	"github.com/0xPolygonHermez/zkevm-bridge-service/db/pgstorage"
	"github.com/0xPolygonHermez/zkevm-bridge-service/etherman"
	"github.com/0xPolygonHermez/zkevm-bridge-service/utils"
	"github.com/0xPolygonHermez/zkevm-bridge-service/utils/gerror"
	"github.com/0xPolygonHermez/zkevm-node/encoding"
	"github.com/0xPolygonHermez/zkevm-node/etherman/smartcontracts/bridge"
	"github.com/0xPolygonHermez/zkevm-node/etherman/smartcontracts/globalexitrootmanager"
	erc20 "github.com/0xPolygonHermez/zkevm-node/etherman/smartcontracts/matic"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/test/contracts/bin/ERC20"
	"github.com/0xPolygonHermez/zkevm-node/test/operations"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v4"
)

// NetworkSID is used to identify the network.
type NetworkSID string

// NetworkSID constants
const (
	L1 NetworkSID = "l1"
	L2 NetworkSID = "l2"

	waitRootSyncDeadline = 120 * time.Second
)

const (
	l1NetworkURL = "http://localhost:8545"
	l2NetworkURL = "http://localhost:8123"

	// MaticTokenAddress token address
	MaticTokenAddress = "0x5FbDB2315678afecb367f032d93F642f64180aa3" //nolint:gosec
	l1BridgeAddr      = "0x0165878A594ca255338adfa4d48449f69242Eb8F"
	l2BridgeAddr      = "0x9d98deabc42dd696deb9e40b4f1cab7ddbf55988"

	l1AccHexAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"

	sequencerAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"

	makeCmd = "make"
	cmdDir  = "../.."
)

var (
	dbConfig = pgstorage.NewConfigFromEnv()
	networks = map[NetworkSID]uint{
		L1: 0,
		L2: 1,
	}
	accHexPrivateKeys = map[NetworkSID]string{
		L1: "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80", //0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266
		L2: "0xdfd01798f92667dbf91df722434e8fbe96af0211d4d1b82bbbbc8f1def7a814f", //0xc949254d682d8c9ad5682521675b8f43b102aec4
	}
)

type storageInterface interface {
	GetLastBlock(ctx context.Context, networkID uint, dbTx pgx.Tx) (*etherman.Block, error)
	GetLatestExitRoot(ctx context.Context, isRollup bool, dbTx pgx.Tx) (*etherman.GlobalExitRoot, error)
	GetLatestL1SyncedExitRoot(ctx context.Context, dbTx pgx.Tx) (*etherman.GlobalExitRoot, error)
	GetLatestTrustedExitRoot(ctx context.Context, dbTx pgx.Tx) (*etherman.GlobalExitRoot, error)
	GetTokenWrapped(ctx context.Context, originalNetwork uint, originalTokenAddress common.Address, dbTx pgx.Tx) (*etherman.TokenWrapped, error)
	GetDepositCountByRoot(ctx context.Context, root []byte, network uint8, dbTx pgx.Tx) (uint, error)
	UpdateBlocksForTesting(ctx context.Context, networkID uint, blockNum uint64, dbTx pgx.Tx) error
	GetLastBatchNumber(ctx context.Context, dbTx pgx.Tx) (uint64, error)
	UpdateBatchesForTesting(ctx context.Context, batchNum uint64, dbTx pgx.Tx) error
}

// Config is the main Manager configuration.
type Config struct {
	Storage db.Config
	BT      bridgectrl.Config
}

// Manager controls operations and has knowledge about how to set up and tear
// down a functional environment.
type Manager struct {
	cfg *Config
	ctx context.Context

	storage       storageInterface
	bridgetree    *bridgectrl.BridgeController
	bridgeService pb.BridgeServiceServer

	clients map[NetworkSID]*utils.Client
}

// NewManager returns a manager ready to be used and a potential error caused
// during its creation (which can come from the setup of the db connection).
func NewManager(ctx context.Context, cfg *Config) (*Manager, error) {
	// Init database instance
	err := pgstorage.InitOrReset(dbConfig)
	if err != nil {
		return nil, err
	}

	opsman := &Manager{
		cfg: cfg,
		ctx: ctx,
	}
	//Init storage and mt
	pgst, err := pgstorage.NewPostgresStorage(dbConfig)
	if err != nil {
		return nil, err
	}
	st, err := db.NewStorage(cfg.Storage)
	if err != nil {
		return nil, err
	}
	bt, err := bridgectrl.NewBridgeController(cfg.BT, []uint{0, 1}, pgst, pgst)
	if err != nil {
		return nil, err
	}
	l1Client, err := utils.NewClient(ctx, l1NetworkURL)
	if err != nil {
		return nil, err
	}
	l2Client, err := utils.NewClient(ctx, l2NetworkURL)
	if err != nil {
		return nil, err
	}
	bService := bridgectrl.NewBridgeService(pgst, bt)
	opsman.storage = st.(storageInterface)
	opsman.bridgetree = bt
	opsman.bridgeService = bService
	opsman.clients = make(map[NetworkSID]*utils.Client)
	opsman.clients[L1] = l1Client
	opsman.clients[L2] = l2Client
	return opsman, err
}

// SendL1Deposit sends a deposit from l1 to l2.
func (m *Manager) SendL1Deposit(ctx context.Context, tokenAddr common.Address, amount *big.Int,
	destNetwork uint32, destAddr *common.Address,
) error {
	client := m.clients[L1]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[L1])
	if err != nil {
		return err
	}

	orgExitRoot, err := m.storage.GetLatestExitRoot(ctx, false, nil)
	if err != nil && err != gerror.ErrStorageNotFound {
		return err
	}

	err = client.SendBridgeAsset(ctx, tokenAddr, amount, destNetwork, destAddr, []byte{}, common.HexToAddress(l1BridgeAddr), auth)
	if err != nil {
		return err
	}

	// sync for new exit root
	return m.WaitExitRootToBeSynced(ctx, orgExitRoot, false)
}

// SendL2Deposit sends a deposit from l2 to l1.
func (m *Manager) SendL2Deposit(ctx context.Context, tokenAddr common.Address, amount *big.Int,
	destNetwork uint32, destAddr *common.Address,
) error {
	client := m.clients[L2]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[L2])
	if err != nil {
		return err
	}

	orgExitRoot, err := m.storage.GetLatestExitRoot(ctx, true, nil)
	if err != nil && err != gerror.ErrStorageNotFound {
		return err
	}

	err = client.SendBridgeAsset(ctx, tokenAddr, amount, destNetwork, destAddr, []byte{}, common.HexToAddress(l2BridgeAddr), auth)
	if err != nil {
		return err
	}

	// sync for new exit root
	return m.WaitExitRootToBeSynced(ctx, orgExitRoot, true)
}

// SendL1BridgeMessage bridges a message from l1 to l2.
func (m *Manager) SendL1BridgeMessage(ctx context.Context, destAddr common.Address, destNetwork uint32, amount *big.Int, metadata []byte) error {
	client := m.clients[L1]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[L1])
	if err != nil {
		return err
	}

	orgExitRoot, err := m.storage.GetLatestExitRoot(ctx, true, nil)
	if err != nil && err != gerror.ErrStorageNotFound {
		return err
	}

	auth.Value = amount
	err = client.SendBridgeMessage(ctx, destNetwork, destAddr, metadata, common.HexToAddress(l1BridgeAddr), auth)
	if err != nil {
		return err
	}

	// sync for new exit root
	return m.WaitExitRootToBeSynced(ctx, orgExitRoot, false)
}

// SendL2BridgeMessage bridges a message from l2 to l1.
func (m *Manager) SendL2BridgeMessage(ctx context.Context, destAddr common.Address, destNetwork uint32, amount *big.Int, metadata []byte) error {
	client := m.clients[L2]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[L2])
	if err != nil {
		return err
	}

	orgExitRoot, err := m.storage.GetLatestExitRoot(ctx, true, nil)
	if err != nil && err != gerror.ErrStorageNotFound {
		return err
	}

	auth.Value = amount
	err = client.SendBridgeMessage(ctx, destNetwork, destAddr, metadata, common.HexToAddress(l2BridgeAddr), auth)
	if err != nil {
		return err
	}

	// sync for new exit root
	return m.WaitExitRootToBeSynced(ctx, orgExitRoot, true)
}

// Setup creates all the required components and initializes them according to
// the manager config.
func (m *Manager) Setup() error {
	// Run network container
	err := m.startNetwork()
	if err != nil {
		log.Error("network start failed")
		return err
	}
	const t time.Duration = 3
	time.Sleep(t * time.Second)

	// Start prover container
	err = m.startProver()
	if err != nil {
		log.Error("prover start failed")
		return err
	}

	//Send funds to zkevm node
	err = m.AddFunds(m.ctx)
	if err != nil {
		log.Error("addfunds failed")
		return err
	}

	// Run zkevm node container
	err = m.startZKEVMNode()
	if err != nil {
		log.Error("zkevm node start failed")
		return err
	}
	//Wait for set the genesis and sync
	time.Sleep(t * time.Second)

	// Run bridge container
	err = m.StartBridge()
	if err != nil {
		log.Error("bridge start failed")
		// return err
	}

	//Wait for sync
	const t2 time.Duration = 5
	time.Sleep(t2 * time.Second)

	return nil
}

// AddFunds adds matic and eth to the zkevm node wallet.
func (m *Manager) AddFunds(ctx context.Context) error {
	// Eth client
	log.Infof("Connecting to l1")
	client := m.clients[L1]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[L1])
	if err != nil {
		return err
	}

	// Getting l1 info
	log.Infof("Getting L1 info")
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return err
	}

	// Send some Ether from l1Acc to sequencer acc
	log.Infof("Transferring ETH to the sequencer")
	fromAddress := common.HexToAddress(l1AccHexAddress)
	nonce, err := client.PendingNonceAt(ctx, fromAddress)
	if err != nil {
		return err
	}
	const gasLimit = 21000
	toAddress := common.HexToAddress(sequencerAddress)
	ethAmount, _ := big.NewInt(0).SetString("200000000000000000000", encoding.Base10)
	tx := types.NewTransaction(nonce, toAddress, ethAmount, gasLimit, gasPrice, nil)
	signedTx, err := auth.Signer(auth.From, tx)
	if err != nil {
		return err
	}
	err = client.SendTransaction(ctx, signedTx)
	if err != nil {
		return err
	}

	// Wait eth transfer to be mined
	log.Infof("Waiting tx to be mined")
	const txETHTransferTimeout = 5 * time.Second
	err = WaitTxToBeMined(ctx, client.Client, signedTx, txETHTransferTimeout)
	if err != nil {
		return err
	}

	// Create matic maticTokenSC sc instance
	log.Infof("Loading Matic token SC instance")
	maticAddr := common.HexToAddress(MaticTokenAddress)
	maticTokenSC, err := operations.NewToken(maticAddr, client)
	if err != nil {
		return err
	}

	// Send matic to sequencer
	log.Infof("Transferring MATIC tokens to sequencer")
	maticAmount, _ := big.NewInt(0).SetString("200000000000000000000000", encoding.Base10)
	tx, err = maticTokenSC.Transfer(auth, toAddress, maticAmount)
	if err != nil {
		return err
	}

	// wait matic transfer to be mined
	log.Infof("Waiting tx to be mined")
	const txMaticTransferTimeout = 5 * time.Second
	return WaitTxToBeMined(ctx, client.Client, tx, txMaticTransferTimeout)
}

// Teardown stops all the components.
func Teardown() error {
	err := StopBridge()
	if err != nil {
		return err
	}

	err = stopZKEVMNode()
	if err != nil {
		return err
	}

	err = stopProver()
	if err != nil {
		return err
	}

	err = stopNetwork()
	if err != nil {
		return err
	}

	return nil
}

func (m *Manager) startNetwork() error {
	if err := stopNetwork(); err != nil {
		return err
	}
	cmd := exec.Command(makeCmd, "run-network")
	err := runCmd(cmd)
	if err != nil {
		return err
	}
	// Wait network to be ready
	return poll(defaultInterval, defaultDeadline, networkUpCondition)
}

func stopNetwork() error {
	cmd := exec.Command(makeCmd, "stop-network")
	return runCmd(cmd)
}

func (m *Manager) startZKEVMNode() error {
	if err := stopZKEVMNode(); err != nil {
		return err
	}
	cmd := exec.Command(makeCmd, "run-node")
	err := runCmd(cmd)
	if err != nil {
		return err
	}
	// Wait zkevm node to be ready
	return poll(defaultInterval, defaultDeadline, zkevmNodeUpCondition)
}

func stopZKEVMNode() error {
	cmd := exec.Command(makeCmd, "stop-node")
	return runCmd(cmd)
}

func (m *Manager) startProver() error {
	if err := stopProver(); err != nil {
		return err
	}
	cmd := exec.Command(makeCmd, "run-prover")
	err := runCmd(cmd)
	if err != nil {
		return err
	}
	// Wait prover to be ready
	return poll(defaultInterval, defaultDeadline, proverUpCondition)
}

func stopProver() error {
	cmd := exec.Command(makeCmd, "stop-prover")
	return runCmd(cmd)
}

func runCmd(c *exec.Cmd) error {
	c.Dir = cmdDir
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// StartBridge restarts the bridge service.
func (m *Manager) StartBridge() error {
	if err := StopBridge(); err != nil {
		return err
	}
	cmd := exec.Command(makeCmd, "run-bridge")
	err := runCmd(cmd)
	if err != nil {
		return err
	}
	// Wait bridge to be ready
	return poll(defaultInterval, defaultDeadline, bridgeUpCondition)
}

// StopBridge stops the bridge service.
func StopBridge() error {
	cmd := exec.Command(makeCmd, "stop-bridge")
	return runCmd(cmd)
}

// CheckAccountBalance checks the balance by address
func (m *Manager) CheckAccountBalance(ctx context.Context, network NetworkSID, account *common.Address) (*big.Int, error) {
	client := m.clients[network]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[network])
	if err != nil {
		return big.NewInt(0), nil
	}

	if account == nil {
		account = &auth.From
	}
	balance, err := client.BalanceAt(ctx, *account, nil)
	if err != nil {
		return big.NewInt(0), nil
	}
	return balance, nil
}

// CheckAccountTokenBalance checks the balance by address
func (m *Manager) CheckAccountTokenBalance(ctx context.Context, network NetworkSID, tokenAddr common.Address, account *common.Address) (*big.Int, error) {
	client := m.clients[network]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[network])
	if err != nil {
		return big.NewInt(0), nil
	}

	if account == nil {
		account = &auth.From
	}
	erc20Token, err := erc20.NewMatic(tokenAddr, client)
	if err != nil {
		return big.NewInt(0), nil
	}
	balance, err := erc20Token.BalanceOf(&bind.CallOpts{Pending: false}, *account)
	if err != nil {
		return big.NewInt(0), nil
	}
	return balance, nil
}

// GetClaimData gets the claim data
func (m *Manager) GetClaimData(networkID, depositCount uint) ([][bridgectrl.KeyLen]byte, *etherman.GlobalExitRoot, error) {
	return m.bridgetree.GetClaim(networkID, depositCount)
}

// GetBridgeInfoByDestAddr gets the bridge info
func (m *Manager) GetBridgeInfoByDestAddr(ctx context.Context, addr *common.Address) ([]*pb.Deposit, error) {
	auth, err := m.clients[L2].GetSigner(ctx, accHexPrivateKeys[L2])
	if err != nil {
		return []*pb.Deposit{}, err
	}
	if addr == nil {
		addr = &auth.From
	}
	req := pb.GetBridgesRequest{
		DestAddr: addr.String(),
	}
	res, err := m.bridgeService.GetBridges(ctx, &req)
	if err != nil {
		return []*pb.Deposit{}, err
	}
	return res.Deposits, nil
}

// SendL1Claim send an L1 claim
func (m *Manager) SendL1Claim(ctx context.Context, deposit *pb.Deposit, smtProof [][32]byte, globalExitRoot *etherman.GlobalExitRoot) error {
	client := m.clients[L1]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[L1])
	if err != nil {
		return err
	}

	return client.SendClaim(ctx, deposit, smtProof, globalExitRoot, common.HexToAddress(l1BridgeAddr), auth)
}

// SendL2Claim send an L2 claim
func (m *Manager) SendL2Claim(ctx context.Context, deposit *pb.Deposit, smtProof [][32]byte, globalExitRoot *etherman.GlobalExitRoot) error {
	client := m.clients[L2]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[L2])
	if err != nil {
		return err
	}

	if deposit.LeafType == utils.LeafTypeAsset {
		auth.GasPrice = big.NewInt(0)
	}

	err = client.SendClaim(ctx, deposit, smtProof, globalExitRoot, common.HexToAddress(l2BridgeAddr), auth)
	return err
}

// GetTrustedGlobalExitRootSynced reads the latest globalexitroot of a batch proposal from db
func (m *Manager) GetTrustedGlobalExitRootSynced(ctx context.Context) (*etherman.GlobalExitRoot, error) {
	return m.storage.GetLatestTrustedExitRoot(ctx, nil)
}

// GetLatestGlobalExitRootFromL1 reads the latest synced globalexitroot in l1 from db
func (m *Manager) GetLatestGlobalExitRootFromL1(ctx context.Context) (*etherman.GlobalExitRoot, error) {
	return m.storage.GetLatestL1SyncedExitRoot(ctx, nil)
}

// GetCurrentGlobalExitRootFromSmc reads the globalexitroot from the smc
func (m *Manager) GetCurrentGlobalExitRootFromSmc(ctx context.Context) (*etherman.GlobalExitRoot, error) {
	client := m.clients[L1]
	br, err := bridge.NewBridge(common.HexToAddress(l1BridgeAddr), client)
	if err != nil {
		return nil, err
	}
	GlobalExitRootManAddr, err := br.GlobalExitRootManager(&bind.CallOpts{Pending: false})
	if err != nil {
		return nil, err
	}
	globalManager, err := globalexitrootmanager.NewGlobalexitrootmanager(GlobalExitRootManAddr, client)
	if err != nil {
		return nil, err
	}
	gMainnet, err := globalManager.LastMainnetExitRoot(&bind.CallOpts{Pending: false})
	if err != nil {
		return nil, err
	}
	gRollup, err := globalManager.LastRollupExitRoot(&bind.CallOpts{Pending: false})
	if err != nil {
		return nil, err
	}
	result := etherman.GlobalExitRoot{
		ExitRoots: []common.Hash{gMainnet, gRollup},
	}
	return &result, nil
}

// DeployERC20 deploys erc20 smc
func (m *Manager) DeployERC20(ctx context.Context, name, symbol string, network NetworkSID) (common.Address, *ERC20.ERC20, error) {
	client := m.clients[network]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[network])
	if err != nil {
		return common.Address{}, nil, err
	}

	return client.DeployERC20(ctx, name, symbol, auth)
}

// DeployBridgeMessageReceiver deploys the brdige message receiver smc.
func (m *Manager) DeployBridgeMessageReceiver(ctx context.Context, network NetworkSID) (common.Address, error) {
	client := m.clients[network]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[network])
	if err != nil {
		return common.Address{}, err
	}

	return client.DeployBridgeMessageReceiver(ctx, auth)
}

// MintERC20 mint erc20 tokens
func (m *Manager) MintERC20(ctx context.Context, erc20Addr common.Address, amount *big.Int, network NetworkSID) error {
	client := m.clients[network]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[network])
	if err != nil {
		return err
	}

	var bridgeAddress = l1BridgeAddr
	if network == L2 {
		bridgeAddress = l2BridgeAddr
	}

	err = client.ApproveERC20(ctx, erc20Addr, common.HexToAddress(bridgeAddress), amount, auth)
	if err != nil {
		return err
	}

	return client.MintERC20(ctx, erc20Addr, amount, auth)
}

// ApproveERC20 approves erc20 tokens
func (m *Manager) ApproveERC20(ctx context.Context, erc20Addr, bridgeAddr common.Address, amount *big.Int, network NetworkSID) error {
	client := m.clients[network]
	auth, err := client.GetSigner(ctx, accHexPrivateKeys[network])
	if err != nil {
		return err
	}
	if network == L2 {
		auth.GasPrice = big.NewInt(0)
	}
	return client.ApproveERC20(ctx, erc20Addr, bridgeAddr, amount, auth)
}

// GetTokenWrapped get token wrapped info
func (m *Manager) GetTokenWrapped(ctx context.Context, originNetwork uint, originalTokenAddr common.Address, isCreated bool) (*etherman.TokenWrapped, error) {
	if isCreated {
		blockID, err := m.getLastBlockID(ctx)
		if err != nil {
			return nil, err
		}

		err = operations.Poll(defaultInterval, defaultDeadline, func() (bool, error) {
			wrappedToken, err := m.storage.GetTokenWrapped(ctx, originNetwork, originalTokenAddr, nil)
			if err != nil {
				return false, err
			}
			return wrappedToken.BlockID >= blockID, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return m.storage.GetTokenWrapped(ctx, originNetwork, originalTokenAddr, nil)
}

// UpdateBlocksForTesting updates the hash of blocks.
func (m *Manager) UpdateBlocksForTesting(ctx context.Context, networkID uint, blockNum uint64) error {
	return m.storage.UpdateBlocksForTesting(ctx, networkID, blockNum, nil)
}

// GetLastBatchNumber returns the last batch number.
func (m *Manager) GetLastBatchNumber(ctx context.Context) (uint64, error) {
	return m.storage.GetLastBatchNumber(ctx, nil)
}

// UpdateBatchesForTesting updates batches for testing.
func (m *Manager) UpdateBatchesForTesting(ctx context.Context, batchNum uint64) error {
	return m.storage.UpdateBatchesForTesting(ctx, batchNum, nil)
}

// WaitExitRootToBeSynced waits unitl new exit root is synced.
func (m *Manager) WaitExitRootToBeSynced(ctx context.Context, orgExitRoot *etherman.GlobalExitRoot, isRollup bool) error {
	log.Debugf("WaitExitRootToBeSynced: %v\n", orgExitRoot)
	if orgExitRoot == nil {
		orgExitRoot = &etherman.GlobalExitRoot{
			ExitRoots: []common.Hash{{}, {}},
		}
	}
	return operations.Poll(defaultInterval, waitRootSyncDeadline, func() (bool, error) {
		exitRoot, err := m.storage.GetLatestExitRoot(ctx, isRollup, nil)
		if err != nil {
			if err == gerror.ErrStorageNotFound {
				return false, nil
			}
			return false, err
		}
		tID := 0
		if isRollup {
			tID = 1
		}
		return exitRoot.ExitRoots[tID] != orgExitRoot.ExitRoots[tID], nil
	})
}

func (m *Manager) getLastBlockID(ctx context.Context) (uint64, error) {
	var lastBlockID uint64 = 0

	lastBlock, err := m.storage.GetLastBlock(ctx, networks[L1], nil)
	if err != nil {
		if err != gerror.ErrStorageNotFound {
			return 0, err
		}
	} else if lastBlock.ID > lastBlockID {
		lastBlockID = lastBlock.ID
	}

	lastBlock, err = m.storage.GetLastBlock(ctx, networks[L2], nil)
	if err != nil {
		if err != gerror.ErrStorageNotFound {
			return 0, err
		}
	} else if lastBlock.ID > lastBlockID {
		lastBlockID = lastBlock.ID
	}

	return lastBlockID, nil
}
