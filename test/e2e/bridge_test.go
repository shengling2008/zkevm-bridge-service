package e2e

import (
	"context"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/hermeznetwork/hermez-bridge/bridgectrl"
	"github.com/hermeznetwork/hermez-bridge/db"
	"github.com/hermeznetwork/hermez-bridge/test/operations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E tests the flow of deposit and withdraw funds using the vector
func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	defer func() {
		require.NoError(t, operations.Teardown())
	}()

	t.Run("ERC20 Test", func(t *testing.T) {
		ctx := context.Background()

		opsCfg := &operations.Config{
			Storage: db.Config{
				Database: "postgres",
				Name:     "test_db",
				User:     "test_user",
				Password: "test_password",
				Host:     "localhost",
				Port:     "5433",
			},
			BT: bridgectrl.Config{
				Store:  "postgres",
				Height: uint8(32),
			},
		}
		opsman, err := operations.NewManager(ctx, opsCfg)
		require.NoError(t, err)

		//Run environment
		require.NoError(t, opsman.Setup())

		// Check initial globalExitRoot. Must fail because at the beggining, no globalExitRoot event is thrown.
		globalExitRootSMC, err := opsman.GetCurrentGlobalExitRootFromSmc(ctx)
		require.NoError(t, err)
		t.Logf("initial globalExitRootSMC.GlobalExitRootNum: %+v,", globalExitRootSMC)

		// Send L1 deposit
		var destNetwork uint32 = 0
		amount := new(big.Int).SetUint64(10000000000000000000)
		tokenAddr, token, err := opsman.DeployERC20(ctx, "A COIN", "ACO")
		require.NoError(t, err)
		//Mint tokens
		_, err = opsman.MintERC20(ctx, token, amount, "l2")
		require.NoError(t, err)
		//Check balance
		origAddr := common.HexToAddress("0xc949254d682d8c9ad5682521675b8f43b102aec4")
		balance, err := opsman.CheckAccountTokenBalance(ctx, "l2", tokenAddr, &origAddr)
		require.NoError(t, err)
		
		t.Log("Token balance: ", balance, ". tokenaddress: ", tokenAddr, ". account: ", origAddr)
		destAddr := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
		amount = new(big.Int).SetUint64(1000000000000000000)
		err = opsman.SendL2Deposit(ctx, tokenAddr, amount, destNetwork, &destAddr)
		require.NoError(t, err)
		// Check globalExitRoot
		globalExitRoot2, err := opsman.GetCurrentGlobalExitRootSynced(ctx)
		require.NoError(t, err)
		t.Logf("globalExitRoot.GlobalExitRootNum: %d, globalExitRoot2.GlobalExitRootNum: %d", globalExitRootSMC.GlobalExitRootNum, globalExitRoot2.GlobalExitRootNum)
		assert.NotEqual(t, globalExitRootSMC.GlobalExitRootNum, globalExitRoot2.GlobalExitRootNum)
		t.Logf("globalExitRootSMC.mainnet: %v, globalExitRoot2.mainnet: %v", globalExitRootSMC.ExitRoots[0], globalExitRoot2.ExitRoots[0])
		assert.Equal(t, globalExitRootSMC.ExitRoots[0], globalExitRoot2.ExitRoots[0])
		t.Logf("globalExitRootSMC.rollup: %v, globalExitRoot2.rollup: %v", globalExitRootSMC.ExitRoots[1], globalExitRoot2.ExitRoots[1])
		assert.NotEqual(t, globalExitRootSMC.ExitRoots[1], globalExitRoot2.ExitRoots[1])
		// Get Bridge Info By DestAddr
		deposits, err := opsman.GetBridgeInfoByDestAddr(ctx, &destAddr)
		require.NoError(t, err)
		// Check L2 funds
		balance, err = opsman.CheckAccountTokenBalance(ctx, "l2", tokenAddr, &origAddr)
		require.NoError(t, err)
		assert.Equal(t, 0, balance.Cmp(big.NewInt(9000000000000000000)))
		t.Log("Deposits: ", deposits)
		t.Log("Before getClaimData: ", deposits[0].NetworkId, deposits[0].DepositCnt)
		// Get the claim data
		smtProof, globaExitRoot, err := opsman.GetClaimData(uint(deposits[0].NetworkId), uint(deposits[0].DepositCnt))
		require.NoError(t, err)
		// Force to propose a new batch
		err = opsman.ForceBatchProposal(ctx)
		require.NoError(t, err)
		// Claim funds in L1
		t.Logf("Deposits: %+v", deposits)
		for _, s := range smtProof {
			t.Log("smt: ", hex.EncodeToString(s[:]))
		}
		t.Logf("globalExitRoot: %+v", globaExitRoot)
		err = opsman.SendL1Claim(ctx, deposits[0], smtProof, globaExitRoot)
		require.NoError(t, err)
		// Get tokenWrappedAddr
		t.Log("token Address:", tokenAddr)
		tokenWrapped, err := opsman.GetTokenWrapped(ctx, 1, tokenAddr)
		require.NoError(t, err)
		// Check L2 funds to see if the amount has been increased
		balance2, err := opsman.CheckAccountTokenBalance(ctx, "l1", tokenWrapped.WrappedTokenAddress, &destAddr)
		require.NoError(t, err)
		t.Log("balance l1 account after claim funds: ", balance2)
		assert.NotEqual(t, balance, balance2)
		assert.Equal(t, amount, balance2)

		// Check globalExitRoot
		globalExitRoot3, err := opsman.GetCurrentGlobalExitRootSynced(ctx)
		require.NoError(t, err)
		// Send L2 Deposit to withdraw the some funds
		destNetwork = 1
		amount = new(big.Int).SetUint64(600000000000000000)
		err = opsman.SendL1Deposit(ctx, tokenWrapped.WrappedTokenAddress, amount, destNetwork, &destAddr)
		require.NoError(t, err)
		// Get Bridge Info By DestAddr
		deposits, err = opsman.GetBridgeInfoByDestAddr(ctx, &destAddr)
		require.NoError(t, err)
		t.Log("Deposits 2: ", deposits)
		// Check globalExitRoot
		globalExitRoot4, err := opsman.GetCurrentGlobalExitRootSynced(ctx)
		require.NoError(t, err)
		t.Logf("Global3 %+v: ", globalExitRoot3)
		t.Logf("Global4 %+v: ", globalExitRoot4)
		assert.NotEqual(t, globalExitRoot3.GlobalExitRootNum, globalExitRoot4.GlobalExitRootNum)
		assert.NotEqual(t, globalExitRoot3.ExitRoots[0], globalExitRoot4.ExitRoots[0])
		assert.Equal(t, common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"), globalExitRoot3.ExitRoots[0])
		assert.Equal(t, common.HexToHash("0x7a235fb7824fe08d70e462b3587fd51ac01c8ba4a575c1b8df996b56c5b675f4"), globalExitRoot3.ExitRoots[1])
		assert.Equal(t, common.HexToHash("0x2570ed0f77fb634e6ec6e5ba19b9e01aebe4b38700eac7a9eb2e9081241a2116"), globalExitRoot4.ExitRoots[0])
		assert.Equal(t, common.HexToHash("0x7a235fb7824fe08d70e462b3587fd51ac01c8ba4a575c1b8df996b56c5b675f4"), globalExitRoot4.ExitRoots[1])
		// Check L2 funds
		balance, err = opsman.CheckAccountTokenBalance(ctx, "l2", tokenAddr, &destAddr)
		require.NoError(t, err)
		t.Log("balance: ", balance)
		assert.Equal(t, 0, big.NewInt(0).Cmp(balance))
		// Get the claim data
		smtProof, globaExitRoot, err = opsman.GetClaimData(uint(deposits[0].NetworkId), uint(deposits[0].DepositCnt))
		require.NoError(t, err)
		t.Log("smt2: ", smtProof)
		// Claim funds in L1
		err = opsman.SendL2Claim(ctx, deposits[0], smtProof, globaExitRoot)
		require.NoError(t, err)
		// Check L2 funds to see if the amount has been increased
		balance, err = opsman.CheckAccountTokenBalance(ctx, "l2", tokenAddr, &destAddr)
		require.NoError(t, err)
		assert.Equal(t, big.NewInt(600000000000000000), balance)
		// Check L1 funds to see that the amount has been reduced
		balance, err = opsman.CheckAccountTokenBalance(ctx, "l1", tokenWrapped.WrappedTokenAddress, &destAddr)
		require.NoError(t, err)
		assert.Equal(t, 0, big.NewInt(400000000000000000).Cmp(balance))
		require.NoError(t, operations.Teardown())
	})
}
